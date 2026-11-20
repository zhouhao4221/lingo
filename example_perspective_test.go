package lingo_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/zhouhao4221/lingo"
)

// 接入方自己的用户体系（示意）：业务角色 → lingo 固定角色 + 需求视角。
var appUsers = map[string]struct {
	name, role string
	project    string
}{
	"u100": {"王五", "customer", "demo"},
	"u200": {"张三", "ops", "demo"},       // 运营：业务方
	"u300": {"李四", "architect", "demo"}, // 架构师：专家
	"u400": {"赵六", "director", "demo"},  // 部门负责人：决策者
}

// 需求视角的接入配置：在 Authenticate 中映射 Perspective，用 AuthorizePerspective 决定谁能补充 / 查看哪条需求，
// 用 RequiredPerspectives 声明必填视角，在 Notifier 中按 Event.Perspectives 提醒相应的人。
func ExampleConfig_perspectives() {
	var db *sql.DB // 接入方的 PostgreSQL 连接

	l, err := lingo.New(lingo.Config{
		DB:       db,
		Gitea:    lingo.GiteaConfig{BaseURL: "https://git.example.com", Token: "bot-token", WebhookSecret: "secret"},
		Projects: []lingo.Project{{Key: "demo", Name: "示例项目", Repos: []lingo.Repo{{Owner: "demo", Name: "api"}}}},

		// 1. 身份：角色决定权限，视角只决定补充归入哪一层（为空视为 user）
		Authenticate: func(r *http.Request) (lingo.User, error) {
			id := sessionUserID(r) // 接入方自己的登录态
			u, ok := appUsers[id]
			if !ok {
				return lingo.User{}, errors.New("unauthenticated")
			}
			user := lingo.User{ID: id, Name: u.name, Role: lingo.RoleExternal, Projects: []string{u.project}}
			switch u.role {
			case "ops":
				user.Perspective = lingo.PerspectiveBusiness
			case "architect":
				user.Perspective = lingo.PerspectiveExpert
			case "director":
				user.Perspective = lingo.PerspectiveLeader
			}
			return user, nil
		},

		// 2. 授权：不配置时同工单可见范围（external 只能补充 / 查看本人的需求）。这里放行有视角的人参与项目内全部需求，
		//    其他人只能处理本人的；返回 lingo.ErrNotFound 时不暴露工单是否存在
		AuthorizePerspective: func(ctx context.Context, u lingo.User, t lingo.Ticket, write bool) error {
			if u.Perspective != "" || t.UserID == u.ID {
				return nil
			}
			return lingo.ErrNotFound
		},

		// 3. 必填视角：转出时缺失发 ticket.perspective_missing，Issue 区块标注“缺少：…”；Project 上可覆盖（空切片表示不检查）
		RequiredPerspectives: []lingo.Perspective{lingo.PerspectiveBusiness, lingo.PerspectiveLeader},

		// 4. 通知：推给谁、走什么渠道由接入方决定
		Notifier: perspectiveNotifier{},
	})
	if err != nil {
		log.Fatal(err)
	}
	_ = l
}

type perspectiveNotifier struct{}

func (perspectiveNotifier) Notify(ctx context.Context, e lingo.Event) error {
	switch e.Kind {
	case lingo.EventTicketPerspectiveMissing:
		for _, p := range e.Perspectives { // 缺失的必填视角
			notifyRole(p, fmt.Sprintf("需求 #%d「%s」缺少%s视角的补充：%s", e.TicketID, e.Title, p, e.URL))
		}
	case lingo.EventTicketPerspectiveAdded:
		// e.Perspectives[0] 为本次补充的视角，e.Body 形如“业务方 · 张三 补充了内容”（不含补充内容）
		notifyUser(e.UserID, fmt.Sprintf("你的需求 #%d 有新补充：%s", e.TicketID, e.Body))
	}
	return nil // 返回错误会重试；按 e.ID 去重
}

// 以当前用户的视角追加补充，并读取分层结果（Go 方法与 REST / MCP 同一套校验）。
func ExampleLingo_AddPerspective() {
	var l *lingo.Lingo // lingo.New 创建的实例
	ctx := context.Background()
	ops := lingo.User{ID: "u200", Name: "张三", Role: lingo.RoleExternal, Perspective: lingo.PerspectiveBusiness}

	entry, err := l.AddPerspective(ctx, ops, 42, "影响华东 30 家门店，月底结算前需要上线")
	switch {
	case errors.Is(err, lingo.ErrInvalidInput): // 非需求工单、视角非法、内容为空或超过 10000 字
	case errors.Is(err, lingo.ErrConflict): // 工单已答复 / 合并 / 驳回 / 关闭
	case errors.Is(err, lingo.ErrNotFound), errors.Is(err, lingo.ErrForbidden): // 不存在或无权补充
	case err != nil:
		log.Fatal(err)
	}
	fmt.Println(entry.Perspective, entry.AuthorName)

	ps, err := l.ListPerspectives(ctx, ops, 42)
	if err != nil {
		log.Fatal(err)
	}
	for _, g := range ps.Groups { // 使用者反馈 → 业务补充 → 专家意见 → 决策意见
		fmt.Println(g.Title, len(g.Entries))
	}
	fmt.Println("缺少：", ps.Missing)
}

func sessionUserID(r *http.Request) string       { return r.Header.Get("X-Session-User") }
func notifyRole(p lingo.Perspective, msg string) {}
func notifyUser(id, msg string)                  {}
