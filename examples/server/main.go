// Command server 是最小接入示例：在业务项目的 HTTP 服务中挂载 lingo。
//
// 运行：LINGO_DATABASE_URL=... go run ./examples/server
//
// 只配数据库也能启动：Gitea 与模型的连接配置可以之后由 admin 经 PUT /lingo/settings/connections 写入数据库（无需重启）。
// 也可以用环境变量 LINGO_GITEA_URL、LINGO_GITEA_TOKEN、LINGO_WEBHOOK_SECRET 等在代码中配置，代码配置的项优先且只读。
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

	model := lightModel()
	l, err := lingo.New(lingo.Config{
		DB: db,
		// LightModel：建 Issue 前用轻量模型整理工单（标题、模板字段、描述），以及知识问答的意图解析；不配置时按原内容建单。
		// Model：主模型，知识问答组织回答（内置模型支持流式）；不配置时知识问答返回 ErrNoModel，其余功能不受影响。
		// 这里两者共用同一个模型；生产环境可给 LightModel 配便宜、快速的模型。
		Model:      model,
		LightModel: model,
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
		// 连接配置（可选）：谁能查看与修改模型、Gitea 的连接配置。不配置时只允许 admin。
		AuthorizeSettings: func(ctx context.Context, u lingo.User, write bool) error {
			if u.Role == lingo.RoleAdmin {
				return nil
			}
			return lingo.ErrForbidden
		},
		// 需求视角（可选）：谁能给哪条需求补充 / 查看视角补充。不配置时同工单可见范围（external 只能是本人的需求）；
		// 这里放行有视角的人（业务方、专家、决策者）参与项目内全部需求。
		AuthorizePerspective: func(ctx context.Context, u lingo.User, t lingo.Ticket, write bool) error {
			if u.Perspective != "" || t.UserID == u.ID {
				return nil
			}
			return lingo.ErrNotFound
		},
		// 需求转出时缺少这些视角会发 ticket.perspective_missing，并在 Issue 的视角区块标注
		RequiredPerspectives: []lingo.Perspective{lingo.PerspectiveBusiness, lingo.PerspectiveLeader},
		Notifier:             logNotifier{},
		// 附件存在业务系统自己的附件服务中，提交工单时只传引用（ID、文件名、地址）。可选配置 FetchAttachment：
		// 转出时读取内容上传为 Gitea 附件；不配置时只把附件地址写进 Issue。
		// 读取时须校验附件属于 userID（附件 ID 由前端传入），否则返回 lingo.ErrNotFound。知识问答的带图提问也经它读图：
		// a.Purpose 为 lingo.AttachmentForQuestion 时 userID 是提问人，图片会发给回答模型（需模型声明支持图片）：
		// FetchAttachment: func(ctx context.Context, userID string, a lingo.Attachment) (io.ReadCloser, error) {
		// 	return files.OpenOwned(ctx, userID, a.ID)
		// },
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

// lightModel 按环境变量选择建单整理用的轻量模型：DeepSeek、OpenAI 或任意 OpenAI 兼容接口；都没配置时不整理。
func lightModel() lingo.Model {
	var (
		m   lingo.Model
		err error
	)
	switch {
	case os.Getenv("DEEPSEEK_API_KEY") != "":
		m, err = lingo.NewDeepSeekModel(os.Getenv("DEEPSEEK_API_KEY"), "")
	case os.Getenv("LINGO_LLM_MODEL") != "":
		// OpenAI 或兼容接口：LINGO_LLM_BASE_URL 为空时使用 OpenAI 官方地址
		m, err = lingo.NewOpenAIModel(lingo.OpenAIConfig{BaseURL: os.Getenv("LINGO_LLM_BASE_URL"),
			APIKey: os.Getenv("LINGO_LLM_API_KEY"), Model: os.Getenv("LINGO_LLM_MODEL")})
	}
	if err != nil {
		log.Fatal(err)
	}
	return m
}

// authenticate 替换为业务项目自己的登录态解析（如校验会话 cookie 或令牌），并按服务端记录的用户身份映射固定角色
// 与需求视角（user / business / expert / leader，与权限无关）。示例从请求头取用户 ID 与视角仅为演示，
// 生产环境不能信任客户端自报的身份、角色或视角。
func authenticate(r *http.Request) (lingo.User, error) {
	id := r.Header.Get("X-User-ID")
	if id == "" {
		return lingo.User{}, errors.New("unauthenticated")
	}
	return lingo.User{ID: id, Name: r.Header.Get("X-User-Name"), Role: lingo.RoleExternal,
		Perspective: lingo.Perspective(r.Header.Get("X-User-Perspective"))}, nil
}

// logNotifier 把进展事件写日志；实际项目中发邮件、站内信或推送到前端。返回错误会重试，接入方按 Event.ID 去重。
type logNotifier struct{}

func (logNotifier) Notify(ctx context.Context, e lingo.Event) error {
	slog.InfoContext(ctx, "lingo event", "id", e.ID, "kind", e.Kind, "ticket", e.TicketID, "status", e.Status, "user", e.UserID,
		"perspectives", e.Perspectives) // perspective_added：本次补充的视角；perspective_missing：缺失的必填视角，据此提醒相应的人
	return nil
}
