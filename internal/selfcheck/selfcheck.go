package selfcheck

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
)

// Level 是检查结果的级别。
type Level string

const (
	OK    Level = "ok"
	Warn  Level = "warn"  // 功能受限或需要人工确认
	Error Level = "error" // 该仓库的对接无法工作
)

// Result 是一项检查结果；Repo 为空表示实例级检查。
type Result struct {
	Repo    string
	Item    string // version | attachments | repo | permission | labels | webhook | templates | workflow
	Level   Level
	Message string
}

// Gitea 是自检用到的 Gitea 接口，由 gitea.Client 实现。
type Gitea interface {
	GetCurrentUser(ctx context.Context) (*gitea.User, error)
	GetVersion(ctx context.Context) (string, error)
	GetAttachmentSettings(ctx context.Context) (*gitea.AttachmentSettings, error)
	GetRepo(ctx context.Context, owner, repo string) (*gitea.Repository, error)
	GetCollaboratorPermission(ctx context.Context, owner, repo, user string) (*gitea.CollaboratorPermission, error)
	ListLabels(ctx context.Context, owner, repo string, o gitea.ListOptions) ([]*gitea.Label, *gitea.Response, error)
	CreateLabel(ctx context.Context, owner, repo string, opt gitea.CreateLabelOption) (*gitea.Label, error)
	FindHook(ctx context.Context, owner, repo, target string) (*gitea.Hook, error)
	CreateHook(ctx context.Context, owner, repo string, opt gitea.CreateHookOption) (*gitea.Hook, error)
	EditHook(ctx context.Context, owner, repo string, id int64, opt gitea.EditHookOption) (*gitea.Hook, error)
	TestHook(ctx context.Context, owner, repo string, id int64) error
	ListIssueTemplates(ctx context.Context, owner, repo string) ([]*gitea.IssueTemplate, error)
	ValidateIssueConfig(ctx context.Context, owner, repo string) (*gitea.IssueConfigValidation, error)
	ListWorkflows(ctx context.Context, owner, repo string) ([]*gitea.Workflow, error)
}

// Pings 读取仓库最近一次收到 Webhook 投递的时间，由 store.Store 实现。
type Pings interface {
	LastPing(ctx context.Context, repo string) (time.Time, bool, error)
}

// Repo 是一个对接仓库的检查配置。
type Repo struct {
	Name           string            // "owner/name"
	Templates      map[string]string // 工单类型 → 模板文件名
	DeployWorkflow string
}

// Options 是自检配置。
type Options struct {
	Gitea       Gitea
	Pings       Pings
	Repos       []Repo
	LabelPrefix string
	// WebhookURL 为灵问 WebhookHandler 的外部地址；AutoRegister 为真时查找或创建 / 修正 Webhook 并发测试投递。
	WebhookURL    string
	WebhookSecret string
	AutoRegister  bool
	// PingWait 是测试投递后等待接收端确认的时间，默认 10 秒。
	PingWait time.Duration
	Now      func() time.Time
}

// 版本要求：最低 1.22；Actions 上线信号需 1.25。
var (
	minVersion     = [2]int{1, 22}
	actionsVersion = [2]int{1, 25}
)

// Run 执行启动自检（PRD 3.2.2、gitea-integration.md 12.3），按仓库逐项返回结果，不因单项失败中止。
// 写操作只有：补齐灵问标签；AutoRegister 时创建或修正 Webhook 并发测试投递。
func Run(ctx context.Context, o Options) []Result {
	if o.PingWait <= 0 {
		o.PingWait = 10 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	var out []Result
	add := func(repo, item string, l Level, format string, args ...any) {
		out = append(out, Result{Repo: repo, Item: item, Level: l, Message: fmt.Sprintf(format, args...)})
	}
	actions := true
	switch v, err := o.Gitea.GetVersion(ctx); {
	case err != nil:
		add("", "version", Error, "cannot read Gitea version: %v", err)
	default:
		mv, ok := parseVersion(v)
		switch {
		case !ok:
			add("", "version", Warn, "unrecognized Gitea version %q", v)
		case less(mv, minVersion):
			add("", "version", Error, "Gitea %s is older than the required 1.22", v)
		case less(mv, actionsVersion):
			actions = false
			add("", "version", Warn, "Gitea %s < 1.25: Actions deploy signals (Repo.Deploy.Workflow) are unavailable", v)
		default:
			add("", "version", OK, "Gitea %s", v)
		}
	}
	if as, err := o.Gitea.GetAttachmentSettings(ctx); err != nil {
		add("", "attachments", Warn, "cannot read attachment settings: %v", err)
	} else if !as.Enabled {
		add("", "attachments", Warn, "attachments are disabled in Gitea: ticket attachments stay in lingo")
	} else {
		add("", "attachments", OK, "attachments enabled, max %d MB, types %s", as.MaxSize, orAny(as.AllowedTypes))
	}
	bot := ""
	if u, err := o.Gitea.GetCurrentUser(ctx); err != nil {
		add("", "permission", Error, "cannot read bot account (GET /user): %v", err)
	} else {
		bot = u.Login
	}

	start := o.Now()
	var tested []string // 已发起测试投递的仓库
	for _, r := range o.Repos {
		owner, name, _ := strings.Cut(r.Name, "/")
		repo, err := o.Gitea.GetRepo(ctx, owner, name)
		if err != nil {
			add(r.Name, "repo", Error, "repository not accessible: %v", err)
			continue
		}
		switch {
		case !repo.HasIssues:
			add(r.Name, "repo", Error, "issues are disabled in the repository")
		case repo.ExternalTracker != nil && repo.ExternalTracker.URL != "":
			add(r.Name, "repo", Error, "repository uses an external issue tracker")
		case repo.Archived:
			add(r.Name, "repo", Warn, "repository is archived")
		default:
			add(r.Name, "repo", OK, "accessible, issues enabled")
		}
		if bot != "" {
			if p, err := o.Gitea.GetCollaboratorPermission(ctx, owner, name, bot); err != nil {
				add(r.Name, "permission", Warn, "cannot read permission of %s: %v", bot, err)
			} else if !p.CanWrite() {
				add(r.Name, "permission", Error, "bot %s has %q permission, write is required", bot, p.Permission)
			} else {
				add(r.Name, "permission", OK, "bot %s has %s permission", bot, p.Permission)
			}
		}
		if created, err := EnsureLabels(ctx, o.Gitea, owner, name, o.LabelPrefix); err != nil {
			add(r.Name, "labels", Error, "cannot ensure labels: %v", err)
		} else if len(created) > 0 {
			add(r.Name, "labels", OK, "created labels %s", strings.Join(created, ", "))
		} else {
			add(r.Name, "labels", OK, "all labels present")
		}
		if t := checkWebhook(ctx, o, owner, name, add); t {
			tested = append(tested, r.Name)
		}
		checkTemplates(ctx, o, r, owner, name, add)
		if r.DeployWorkflow != "" {
			switch ws, err := o.Gitea.ListWorkflows(ctx, owner, name); {
			case !actions:
				add(r.Name, "workflow", Warn, "deploy workflow %s configured but Gitea < 1.25", r.DeployWorkflow)
			case err != nil:
				add(r.Name, "workflow", Warn, "cannot list workflows: %v", err)
			case !slices.ContainsFunc(ws, func(w *gitea.Workflow) bool { return path.Base(w.Path) == r.DeployWorkflow }):
				add(r.Name, "workflow", Error, "deploy workflow %s not found in the repository", r.DeployWorkflow)
			default:
				add(r.Name, "workflow", OK, "deploy workflow %s found", r.DeployWorkflow)
			}
		}
	}
	if len(tested) > 0 {
		out = append(out, confirmPings(ctx, o, tested, start)...)
	}
	return out
}

// checkWebhook 检查（AutoRegister 时创建或修正）Webhook，返回是否发起了测试投递。
func checkWebhook(ctx context.Context, o Options, owner, name string, add func(repo, item string, l Level, format string, args ...any)) bool {
	repo := owner + "/" + name
	if o.WebhookURL == "" {
		if at, ok, err := o.Pings.LastPing(ctx, repo); err == nil && ok {
			add(repo, "webhook", OK, "last test or push delivery received at %s", at.Format(time.RFC3339))
		} else {
			add(repo, "webhook", Warn, "GiteaConfig.WebhookURL not set; no test or push delivery received yet")
		}
		return false
	}
	h, err := o.Gitea.FindHook(ctx, owner, name, o.WebhookURL)
	if err != nil {
		add(repo, "webhook", Warn, "cannot list webhooks (token needs write:repository): %v", err)
		return false
	}
	missing := func(h *gitea.Hook) []string {
		var m []string
		for _, e := range gitea.LingoHookEvents {
			if !slices.Contains(h.Events, e) {
				m = append(m, e)
			}
		}
		return m
	}
	switch {
	case h == nil && !o.AutoRegister:
		add(repo, "webhook", Error, "no webhook points to %s", o.WebhookURL)
		return false
	case h == nil:
		if h, err = o.Gitea.CreateHook(ctx, owner, name, gitea.NewLingoHook(o.WebhookURL, o.WebhookSecret, "")); err != nil {
			add(repo, "webhook", Error, "create webhook failed: %v", err)
			return false
		}
		add(repo, "webhook", OK, "webhook created")
	case !h.Active || len(missing(h)) > 0:
		if !o.AutoRegister {
			add(repo, "webhook", Error, "webhook inactive or missing events %v", missing(h))
			return false
		}
		active := true
		cfg := gitea.NewLingoHook(o.WebhookURL, o.WebhookSecret, "")
		if _, err := o.Gitea.EditHook(ctx, owner, name, h.ID, gitea.EditHookOption{Config: cfg.Config, Events: gitea.LingoHookEvents, Active: &active}); err != nil {
			add(repo, "webhook", Error, "fix webhook failed: %v", err)
			return false
		}
		add(repo, "webhook", OK, "webhook re-enabled with all events")
	default:
		add(repo, "webhook", OK, "webhook present and active")
	}
	if err := o.Gitea.TestHook(ctx, owner, name, h.ID); err != nil {
		add(repo, "delivery", Warn, "test delivery failed to start: %v", err)
		return false
	}
	return true
}

// confirmPings 等待测试投递到达（WebhookHandler 收到 push 时记录连通时间）。
func confirmPings(ctx context.Context, o Options, repos []string, since time.Time) []Result {
	deadline := time.NewTimer(o.PingWait)
	defer deadline.Stop()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	pending := slices.Clone(repos)
	var out []Result
	for len(pending) > 0 {
		var still []string
		for _, r := range pending {
			at, ok, err := o.Pings.LastPing(ctx, r)
			if err == nil && ok && !at.Before(since.Add(-time.Second)) {
				out = append(out, Result{Repo: r, Item: "delivery", Level: OK, Message: "test delivery received"})
			} else {
				still = append(still, r)
			}
		}
		pending = still
		if len(pending) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			deadline.Reset(0)
		case <-deadline.C:
			for _, r := range pending {
				out = append(out, Result{Repo: r, Item: "delivery", Level: Error, Message: "test delivery not received: check that " +
					"the webhook URL is reachable from Gitea and allowed by [security] ALLOWED_HOST_LIST, and that the secret matches"})
			}
			return out
		case <-tick.C:
		}
	}
	return out
}

func checkTemplates(ctx context.Context, o Options, r Repo, owner, name string, add func(repo, item string, l Level, format string, args ...any)) {
	if len(r.Templates) == 0 {
		return
	}
	ts, err := o.Gitea.ListIssueTemplates(ctx, owner, name)
	if err != nil {
		add(r.Name, "templates", Warn, "cannot list issue templates: %v", err)
		return
	}
	var missing []string
	for typ, file := range r.Templates {
		if gitea.MatchTemplate(ts, file, "", "") == nil {
			missing = append(missing, typ+"="+file)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		add(r.Name, "templates", Error, "issue templates not found: %s", strings.Join(missing, ", "))
		return
	}
	if v, err := o.Gitea.ValidateIssueConfig(ctx, owner, name); err == nil && !v.Valid {
		add(r.Name, "templates", Warn, "issue config invalid: %s", v.Message)
		return
	}
	add(r.Name, "templates", OK, "configured templates present")
}

// LabelSpecs 返回灵问使用的标签（前缀默认 lingo）：来源标签，以及 type/*、review/* 两组互斥作用域标签。
func LabelSpecs(prefix string) []gitea.CreateLabelOption {
	if prefix == "" {
		prefix = "lingo"
	}
	out := []gitea.CreateLabelOption{{Name: prefix, Color: "#6f42c1", Description: "由灵问创建"}}
	for _, t := range []struct{ name, color string }{{"consult", "#1d76db"}, {"bug", "#d73a4a"}, {"feature", "#0e8a16"}} {
		out = append(out, gitea.CreateLabelOption{Name: prefix + "/type/" + t.name, Color: t.color, Exclusive: true})
	}
	for _, r := range []struct{ name, color string }{{"pending", "#fbca04"}, {"approved", "#0e8a16"}, {"rejected", "#b60205"}, {"need-info", "#d93f0b"}} {
		out = append(out, gitea.CreateLabelOption{Name: prefix + "/review/" + r.name, Color: r.color, Exclusive: true})
	}
	return out
}

// EnsureLabels 创建仓库中缺少的灵问标签，返回新建的标签名。
func EnsureLabels(ctx context.Context, g Gitea, owner, name, prefix string) ([]string, error) {
	ls, err := gitea.ListAll(ctx, 50, func(ctx context.Context, o gitea.ListOptions) ([]*gitea.Label, *gitea.Response, error) {
		return g.ListLabels(ctx, owner, name, o)
	})
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, l := range ls {
		have[strings.ToLower(l.Name)] = true
	}
	var created []string
	var errs []error
	for _, spec := range LabelSpecs(prefix) {
		if have[strings.ToLower(spec.Name)] {
			continue
		}
		if _, err := g.CreateLabel(ctx, owner, name, spec); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", spec.Name, err))
			continue
		}
		created = append(created, spec.Name)
	}
	return created, errors.Join(errs...)
}

var versionRe = regexp.MustCompile(`^v?(\d+)\.(\d+)`)

func parseVersion(v string) ([2]int, bool) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return [2]int{}, false
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	return [2]int{a, b}, true
}

func less(a, b [2]int) bool { return a[0] < b[0] || (a[0] == b[0] && a[1] < b[1]) }

func orAny(s string) string {
	if s == "" {
		return "any"
	}
	return s
}
