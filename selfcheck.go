package lingo

import (
	"context"
	"log/slog"

	"github.com/zhouhao4221/lingo/internal/selfcheck"
)

// selfCheckLockKey 是自检的 advisory lock 键。
const selfCheckLockKey = 0x6c696e676f02

// SelfCheck 执行启动自检并返回结果（Run 启动时会在后台执行一次并写日志）：Gitea 版本（≥ 1.22，Actions 上线信号需
// ≥ 1.25）与附件设置；每个仓库可访问、Issue 已启用、机器人有写权限；补齐灵工标签；Webhook 指向
// GiteaConfig.WebhookURL、已启用且事件订阅完整（AutoRegisterWebhook 时自动创建或修正），并发一次测试投递确认
// 能到达 WebhookHandler（最多等 10 秒）；Repo.Templates 中的模板与 Repo.Deploy.Workflow 存在。
// 会写入 Gitea 的只有标签与（开启自动注册时的）Webhook。多实例同时执行时只有一个实例真正检查，其他实例返回一条
// item=lock 的结果。
//
// Gitea 未配置（地址或令牌为空）时只返回一条 item=config 的警告，不做其他检查。
func (l *Lingo) SelfCheck(ctx context.Context) []CheckResult {
	c := l.conn()
	if c == nil || !l.gitea.Configured() {
		return []CheckResult{{Item: "config", Level: string(selfcheck.Warn), Message: "Gitea 未配置（地址或令牌为空），已跳过自检"}}
	}
	g := c.eff.Gitea
	var repos []selfcheck.Repo
	for _, p := range l.cfg.Projects {
		for _, r := range p.Repos {
			repos = append(repos, selfcheck.Repo{Name: r.Owner + "/" + r.Name, Templates: r.Templates, DeployWorkflow: r.Deploy.Workflow})
		}
	}
	rs := selfcheck.Run(ctx, selfcheck.Options{Gitea: l.gitea, Pings: l.store, Repos: repos, LabelPrefix: l.labelPrefix(),
		WebhookURL: g.WebhookURL, WebhookSecret: g.WebhookSecret, AutoRegister: g.AutoRegisterWebhook,
		// 多实例同时启动时只有一个执行（其他实例跳过），避免重复创建 Webhook 与标签
		Lock: func(ctx context.Context) (func(), bool, error) { return l.store.TryLock(ctx, selfCheckLockKey) }})
	out := make([]CheckResult, 0, len(rs))
	for _, r := range rs {
		out = append(out, CheckResult{Repo: r.Repo, Item: r.Item, Level: string(r.Level), Message: r.Message})
	}
	return out
}

// logSelfCheck 执行自检并按级别写日志，汇总一行；不阻断启动。
func (l *Lingo) logSelfCheck(ctx context.Context) {
	var warns, errs int
	for _, r := range l.SelfCheck(ctx) {
		level := slog.LevelInfo
		switch r.Level {
		case string(selfcheck.Warn):
			level, warns = slog.LevelWarn, warns+1
		case string(selfcheck.Error):
			level, errs = slog.LevelError, errs+1
		}
		l.log.Log(ctx, level, "lingo: self-check", "repo", r.Repo, "item", r.Item, "result", r.Level, "detail", r.Message)
	}
	l.log.Info("lingo: self-check finished", "warnings", warns, "errors", errs)
}
