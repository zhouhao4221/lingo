package lingo

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
)

// Config 是 lingo 的全部配置，由接入方在代码中构造。
type Config struct {
	DB           *sql.DB // PostgreSQL，驱动由接入方选择
	Model        Model
	Gitea        GiteaConfig
	Projects     []Project // 第一个为默认项目（请求未指定 project 时使用）
	Authenticate Authenticator
	Notifier     Notifier // 可空，为空时不回传进展
	Tools        []Tool
}

// Lingo 是一个灵问实例。
type Lingo struct {
	cfg Config
}

// New 校验配置并创建实例；不执行数据库迁移。
func New(cfg Config) (*Lingo, error) {
	switch {
	case cfg.DB == nil:
		return nil, errors.New("lingo: Config.DB is required")
	case cfg.Model == nil:
		return nil, errors.New("lingo: Config.Model is required")
	case cfg.Gitea.BaseURL == "" || cfg.Gitea.Token == "":
		return nil, errors.New("lingo: Config.Gitea.BaseURL and Token are required")
	case cfg.Authenticate == nil:
		return nil, errors.New("lingo: Config.Authenticate is required")
	case len(cfg.Projects) == 0:
		return nil, errors.New("lingo: at least one Project is required")
	}
	return &Lingo{cfg: cfg}, nil
}

// Handler 返回 REST JSON 接口（提问、工单、评审、知识库、版本），路径为相对路径，
// 由接入方挂载到任意前缀（配合 http.StripPrefix）。
func (l *Lingo) Handler() http.Handler { return notImplemented() }

// MCPHandler 返回 MCP Server（Streamable HTTP），工具与 Handler 能力对齐。
func (l *Lingo) MCPHandler() http.Handler { return notImplemented() }

// WebhookHandler 接收 Gitea Webhook（Issue、Release），验签并按事件去重。
func (l *Lingo) WebhookHandler() http.Handler { return notImplemented() }

// Run 运行后台任务（outbox 投递、Gitea 同步补偿、知识库生成），直到 ctx 取消；多实例运行安全。
func (l *Lingo) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// TODO: 骨架阶段，接口待实现。
func notImplemented() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "lingo: not implemented", http.StatusNotImplemented)
	})
}
