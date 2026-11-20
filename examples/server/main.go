// Command server 是最小接入示例：在业务项目的 HTTP 服务中挂载 lingo。
//
// 运行：LINGO_DATABASE_URL=... LINGO_GITEA_URL=... LINGO_GITEA_TOKEN=... LINGO_WEBHOOK_SECRET=... go run ./examples/server
//
// lingo 不引入数据库驱动：实际项目中加一行驱动的空白导入，如 _ "github.com/jackc/pgx/v5/stdlib"（驱动名 "pgx"）。
// 身份解析、通知渠道、模型均由接入方实现，下面用最简单的写法示意。
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"

	"github.com/zhouhao4221/lingo"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	db, err := sql.Open("pgx", os.Getenv("LINGO_DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	// 迁移由发布流程显式执行（多实例同时执行时由 advisory lock 串行）
	if err := lingo.Migrate(ctx, db); err != nil {
		log.Fatal(err)
	}

	l, err := lingo.New(lingo.Config{
		DB:    db,
		Model: placeholderModel{}, // 替换为自己的 lingo.Model 实现（知识问答使用）
		Gitea: lingo.GiteaConfig{
			BaseURL:       os.Getenv("LINGO_GITEA_URL"),
			Token:         os.Getenv("LINGO_GITEA_TOKEN"), // 机器人账号：write:issue、read:repository、read:user
			WebhookSecret: os.Getenv("LINGO_WEBHOOK_SECRET"),
			// 开启后 Run 启动时自动创建 / 修正各仓库的 Webhook（令牌另需 write:repository）
			AutoRegisterWebhook: os.Getenv("LINGO_WEBHOOK_URL") != "",
			WebhookURL:          os.Getenv("LINGO_WEBHOOK_URL"), // 如 https://app.example.com/webhooks/gitea
		},
		Projects: []lingo.Project{{
			Key:  "demo",
			Name: "示例项目",
			Repos: []lingo.Repo{
				{Owner: "demo", Name: "web", Kind: lingo.RepoFrontend},
				{Owner: "demo", Name: "api", Kind: lingo.RepoBackend,
					Assignees: []string{"dev-agent"},                            // 新 Issue 默认指派给研发 Agent
					Templates: map[string]string{"bug": "bug_report.yaml"},      // Bug 按仓库表单模板建单
					Deploy:    lingo.DeploySignal{Workflow: "deploy-prod.yml"}}, // 该工作流成功即视为上线
			},
			DefaultRepo: "demo/api",
			AutoForward: map[lingo.TicketType]bool{lingo.TicketBug: true}, // Bug 自动转出，其余待后台确认
		}},
		Authenticate: authenticate,
		Notifier:     logNotifier{},
	})
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/lingo/", http.StripPrefix("/lingo", l.Handler())) // REST JSON
	mux.Handle("/mcp", l.MCPHandler())                             // MCP（Streamable HTTP）
	mux.Handle("/webhooks/gitea", l.WebhookHandler())              // Gitea Webhook
	go func() {
		// 后台任务：Webhook 处理、建 Issue、通知投递、定时对账、超时提醒；启动时执行一次自检
		if err := l.Run(ctx); err != nil {
			log.Print(err)
		}
	}()

	srv := &http.Server{Addr: ":8080", Handler: mux}
	go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
	log.Print("listening on :8080")
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// authenticate 替换为业务项目自己的登录态解析：把当前用户映射为固定角色。
func authenticate(r *http.Request) (lingo.User, error) {
	id := r.Header.Get("X-User-ID")
	if id == "" {
		return lingo.User{}, errors.New("unauthenticated")
	}
	role := lingo.RoleExternal
	if r.Header.Get("X-User-Role") == "reviewer" {
		role = lingo.RoleReviewer
	}
	return lingo.User{ID: id, Name: r.Header.Get("X-User-Name"), Role: role}, nil
}

// logNotifier 把进展事件写日志；实际项目中发邮件、站内信或推送到前端。返回错误会重试，接入方按 Event.ID 去重。
type logNotifier struct{}

func (logNotifier) Notify(ctx context.Context, e lingo.Event) error {
	slog.InfoContext(ctx, "lingo event", "id", e.ID, "kind", e.Kind, "ticket", e.TicketID, "status", e.Status, "user", e.UserID)
	return nil
}

// placeholderModel 是占位实现：工单流程不调用模型。
type placeholderModel struct{}

func (placeholderModel) Chat(context.Context, []lingo.Message) (string, error) {
	return "", errors.New("model not configured")
}

func (placeholderModel) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("model not configured")
}
