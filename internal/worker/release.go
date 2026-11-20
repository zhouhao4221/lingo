package worker

import (
	"context"
	"errors"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/release"
	"github.com/zhouhao4221/lingo/internal/ticket"
	"github.com/zhouhao4221/lingo/internal/webhook"
)

// Releases 同步版本与上线，由 release.Service 实现。
type Releases interface {
	Published(ctx context.Context, r release.Release, refs []release.IssueRef, fromTag bool) error
	Deployed(ctx context.Context, d release.Deployment) error
}

// GitReader 读取 tag 与提交区间，由 gitea.Client 实现。
type GitReader interface {
	GetTag(ctx context.Context, owner, repo, tag string) (*gitea.Tag, error)
	Compare(ctx context.Context, owner, repo, base, head string) (*gitea.Compare, error)
}

// Git 把 GitReader 适配为 release.Git。
type Git struct{ R GitReader }

// TagCommit 实现 release.Git。
func (g Git) TagCommit(ctx context.Context, repo, tag string) (string, error) {
	owner, name, _ := strings.Cut(repo, "/")
	t, err := g.R.GetTag(ctx, owner, name, tag)
	if errors.Is(err, gitea.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", giteaErr("get tag "+tag, err)
	}
	if t.Commit == nil {
		return "", nil
	}
	return t.Commit.SHA, nil
}

// Commits 实现 release.Git。
func (g Git) Commits(ctx context.Context, repo, base, head string) ([]string, bool, error) {
	owner, name, _ := strings.Cut(repo, "/")
	cp, err := g.R.Compare(ctx, owner, name, base, head)
	if errors.Is(err, gitea.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, giteaErr("compare "+base+"..."+head, err)
	}
	shas := make([]string, 0, len(cp.Commits))
	for _, c := range cp.Commits {
		shas = append(shas, c.SHA)
	}
	return shas, true, nil
}

// deployWait 是上线信号先于版本记录到达时的等待时间（release / tag 事件与部署事件之间没有顺序保证）。
const deployWait = 2 * time.Minute

// semverPre 匹配带预发布后缀的版本号，如 v1.2.0-rc.1。
var semverPre = regexp.MustCompile(`^v?\d+\.\d+\.\d+-[0-9A-Za-z.-]+`)

func (w *Worker) applyRelease(ctx context.Context, ev *webhook.Event, received time.Time) error {
	if w.o.Releases == nil {
		return nil
	}
	var err error
	switch ev.Kind {
	case webhook.ReleasePublished, webhook.ReleaseUpdated:
		r := ev.Release
		if r.Draft {
			return nil
		}
		rel := release.Release{Repo: ev.Repo, Tag: r.TagName, Name: r.Name, Body: r.Body, Prerelease: r.Prerelease,
			GiteaID: r.ID, URL: r.HTMLURL}
		if r.PublishedAt != nil {
			rel.PublishedAt = *r.PublishedAt
		}
		err = w.o.Releases.Published(ctx, rel, releaseRefs(ev.Repo, r.Body), false)
	case webhook.TagCreated:
		tag := strings.TrimPrefix(ev.Ref, "refs/tags/")
		err = w.o.Releases.Published(ctx, release.Release{Repo: ev.Repo, Tag: tag, Name: tag, CommitSHA: ev.SHA,
			Prerelease: semverPre.MatchString(tag), PublishedAt: received}, nil, true)
	case webhook.StatusChanged:
		cfg := w.o.Repos(ev.Repo)
		s := ev.Status
		if cfg.DeployStatus == "" || s.Context != cfg.DeployStatus || s.State != "success" {
			return nil
		}
		return w.deployed(ctx, release.Deployment{Repo: ev.Repo, SHA: s.SHA, Env: cfg.DeployEnv, Source: "status", Ref: s.TargetURL, At: s.CreatedAt}, received)
	case webhook.WorkflowRunCompleted:
		cfg := w.o.Repos(ev.Repo)
		run := ev.WorkflowRun
		if cfg.DeployWorkflow == "" || run.Conclusion != "success" || !workflowIs(run.Path, cfg.DeployWorkflow) {
			return nil
		}
		d := release.Deployment{Repo: ev.Repo, Tag: run.HeadBranch, SHA: run.HeadSHA, Env: cfg.DeployEnv, Source: "workflow", Ref: run.HTMLURL}
		if run.CompletedAt != nil {
			d.At = *run.CompletedAt
		}
		return w.deployed(ctx, d, received)
	default:
		return nil // release.deleted：保留版本记录与已发布的关联
	}
	if errors.Is(err, release.ErrNotFound) {
		return nil // 不是对接仓库
	}
	return err
}

func (w *Worker) deployed(ctx context.Context, d release.Deployment, received time.Time) error {
	err := w.o.Releases.Deployed(ctx, d)
	if !errors.Is(err, release.ErrNotFound) {
		return err
	}
	if wait := received.Add(deployWait); w.o.Now().Before(wait) {
		return &ticket.DeferError{Until: wait, Reason: "release for deployment not recorded yet"}
	}
	w.log.Debug("deployment does not match any release", "repo", d.Repo, "tag", d.Tag, "sha", d.SHA)
	return nil
}

// workflowIs 报告运行的工作流路径（如 ".gitea/workflows/deploy-prod.yml@refs/heads/main"）是否为文件 file。
func workflowIs(runPath, file string) bool {
	p, _, _ := strings.Cut(runPath, "@")
	return p != "" && (path.Base(p) == file || p == file)
}

// releaseRefs 解析版本说明中的 Issue 引用，补全仓库。
func releaseRefs(repo, body string) []release.IssueRef {
	var out []release.IssueRef
	for _, r := range gitea.ParseIssueRefs(body) {
		full, text := repo, "#"
		if r.Owner != "" {
			full = r.Owner + "/" + r.Repo
			text = full + "#"
		}
		out = append(out, release.IssueRef{Repo: full, Number: r.Index, Text: text + itoa(r.Index)})
	}
	return out
}
