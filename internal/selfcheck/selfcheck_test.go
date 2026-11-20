package selfcheck

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
)

type fake struct {
	mu       sync.Mutex
	version  string
	repos    map[string]*gitea.Repository
	perm     string
	labels   []*gitea.Label
	created  []string
	hook     *gitea.Hook
	hookErr  error
	edited   bool
	tested   []string
	pings    map[string]time.Time
	deliver  bool // TestHook 时模拟投递到达
	tmpls    []*gitea.IssueTemplate
	workflow []*gitea.Workflow
}

var errNF = &gitea.APIError{StatusCode: 404}

func (f *fake) GetCurrentUser(context.Context) (*gitea.User, error) {
	return &gitea.User{Login: "bot"}, nil
}
func (f *fake) GetVersion(context.Context) (string, error) { return f.version, nil }
func (f *fake) GetAttachmentSettings(context.Context) (*gitea.AttachmentSettings, error) {
	return &gitea.AttachmentSettings{Enabled: true, MaxSize: 4}, nil
}
func (f *fake) GetRepo(_ context.Context, o, n string) (*gitea.Repository, error) {
	if r, ok := f.repos[o+"/"+n]; ok {
		return r, nil
	}
	return nil, errNF
}
func (f *fake) GetCollaboratorPermission(context.Context, string, string, string) (*gitea.CollaboratorPermission, error) {
	return &gitea.CollaboratorPermission{Permission: f.perm}, nil
}
func (f *fake) ListLabels(context.Context, string, string, gitea.ListOptions) ([]*gitea.Label, *gitea.Response, error) {
	return f.labels, &gitea.Response{}, nil
}
func (f *fake) CreateLabel(_ context.Context, _, _ string, o gitea.CreateLabelOption) (*gitea.Label, error) {
	f.created = append(f.created, o.Name)
	return &gitea.Label{Name: o.Name}, nil
}
func (f *fake) FindHook(context.Context, string, string, string) (*gitea.Hook, error) {
	return f.hook, f.hookErr
}
func (f *fake) CreateHook(_ context.Context, _, _ string, o gitea.CreateHookOption) (*gitea.Hook, error) {
	f.hook = &gitea.Hook{ID: 7, Active: true, Events: o.Events, Config: o.Config}
	return f.hook, nil
}
func (f *fake) EditHook(context.Context, string, string, int64, gitea.EditHookOption) (*gitea.Hook, error) {
	f.edited = true
	return f.hook, nil
}
func (f *fake) TestHook(_ context.Context, o, n string, _ int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tested = append(f.tested, o+"/"+n)
	if f.deliver {
		f.pings[o+"/"+n] = time.Now()
	}
	return nil
}
func (f *fake) ListIssueTemplates(context.Context, string, string) ([]*gitea.IssueTemplate, error) {
	return f.tmpls, nil
}
func (f *fake) ValidateIssueConfig(context.Context, string, string) (*gitea.IssueConfigValidation, error) {
	return &gitea.IssueConfigValidation{Valid: true}, nil
}
func (f *fake) ListWorkflows(context.Context, string, string) ([]*gitea.Workflow, error) {
	return f.workflow, nil
}
func (f *fake) LastPing(_ context.Context, repo string) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	at, ok := f.pings[repo]
	return at, ok, nil
}

func byItem(rs []Result) map[string]Result {
	m := map[string]Result{}
	for _, r := range rs {
		m[r.Repo+"|"+r.Item] = r
	}
	return m
}

func TestRunHealthy(t *testing.T) {
	f := &fake{version: "1.27.3", perm: "write", deliver: true, pings: map[string]time.Time{},
		repos:    map[string]*gitea.Repository{"demo/api": {HasIssues: true}},
		labels:   []*gitea.Label{{Name: "lingo"}},
		tmpls:    []*gitea.IssueTemplate{{FileName: "bug.yaml"}},
		workflow: []*gitea.Workflow{{Path: ".gitea/workflows/deploy.yml"}}}
	rs := Run(context.Background(), Options{Gitea: f, Pings: f, LabelPrefix: "lingo", WebhookURL: "https://app/wh", WebhookSecret: "s",
		AutoRegister: true, PingWait: 2 * time.Second,
		Repos: []Repo{{Name: "demo/api", Templates: map[string]string{"bug": "bug.yaml"}, DeployWorkflow: "deploy.yml"}}})
	for _, r := range rs {
		if r.Level != OK {
			t.Errorf("%s %s: %s %s", r.Repo, r.Item, r.Level, r.Message)
		}
	}
	m := byItem(rs)
	if m["demo/api|webhook"].Message != "webhook created" || m["demo/api|delivery"].Level != OK {
		t.Errorf("webhook = %+v %+v", m["demo/api|webhook"], m["demo/api|delivery"])
	}
	if len(f.created) != len(LabelSpecs("lingo"))-1 {
		t.Errorf("created labels = %v", f.created)
	}
}

func TestRunProblems(t *testing.T) {
	f := &fake{version: "1.24.0", perm: "read", pings: map[string]time.Time{},
		repos: map[string]*gitea.Repository{"demo/api": {HasIssues: true}, "demo/docs": {HasIssues: false}},
		hook:  &gitea.Hook{ID: 3, Active: false, Events: []string{"issues"}}}
	rs := Run(context.Background(), Options{Gitea: f, Pings: f, WebhookURL: "https://app/wh", PingWait: 600 * time.Millisecond,
		Repos: []Repo{{Name: "demo/api", DeployWorkflow: "deploy.yml", Templates: map[string]string{"bug": "nope.yaml"}},
			{Name: "demo/docs"}, {Name: "demo/gone"}}})
	m := byItem(rs)
	for key, want := range map[string]Level{
		"|version":            Warn,  // < 1.25
		"demo/api|permission": Error, // read
		"demo/api|webhook":    Error, // 未开启自动注册、Webhook 不完整
		"demo/api|workflow":   Warn,  // 版本不支持 Actions
		"demo/api|templates":  Error,
		"demo/docs|repo":      Error, // Issue 未启用
		"demo/gone|repo":      Error, // 不可访问
	} {
		if m[key].Level != want {
			t.Errorf("%s = %+v, want %s", key, m[key], want)
		}
	}
	if f.edited || len(f.tested) != 0 {
		t.Error("modified webhook without AutoRegister")
	}

	// 开启自动注册：修正 Webhook；测试投递没到达时报错并提示 ALLOWED_HOST_LIST
	rs = Run(context.Background(), Options{Gitea: f, Pings: f, WebhookURL: "https://app/wh", AutoRegister: true, PingWait: 600 * time.Millisecond,
		Repos: []Repo{{Name: "demo/api"}}})
	m = byItem(rs)
	if !f.edited || m["demo/api|delivery"].Level != Error || !strings.Contains(m["demo/api|delivery"].Message, "ALLOWED_HOST_LIST") {
		t.Errorf("auto-register = %+v %+v", m["demo/api|webhook"], m["demo/api|delivery"])
	}
	f.hookErr = errors.New("403")
	if m := byItem(Run(context.Background(), Options{Gitea: f, Pings: f, WebhookURL: "https://app/wh", Repos: []Repo{{Name: "demo/api"}}})); m["demo/api|webhook"].Level != Warn {
		t.Errorf("list hooks forbidden = %+v", m["demo/api|webhook"])
	}
}

func TestParseVersion(t *testing.T) {
	for v, want := range map[string][2]int{"1.22.3": {1, 22}, "v1.25.0+dev-12": {1, 25}, "1.27": {1, 27}} {
		if got, ok := parseVersion(v); !ok || got != want {
			t.Errorf("parseVersion(%s) = %v %v", v, got, ok)
		}
	}
	if _, ok := parseVersion("main"); ok {
		t.Error("parsed garbage")
	}
}
