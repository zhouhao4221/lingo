package worker

import (
	"context"
	"errors"
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
	GetReleaseByTag(ctx context.Context, owner, repo, tag string) (*gitea.Release, error)
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

// Contains 实现 release.Git：比较 ref...sha，sha 相对 ref 没有新提交即已包含。
func (g Git) Contains(ctx context.Context, repo, ref, sha string) (bool, error) {
	owner, name, _ := strings.Cut(repo, "/")
	cp, err := g.R.Compare(ctx, owner, name, ref, sha)
	if errors.Is(err, gitea.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, giteaErr("compare "+ref+"..."+sha, err)
	}
	return cp.TotalCommits == 0, nil
}

// deployWait 是上线信号先于版本记录到达时的等待时间（release / tag 事件与部署事件之间没有顺序保证）。
const deployWait = 2 * time.Minute

// semverPre 匹配带预发布后缀的版本号，如 v1.2.0-rc.1；versionTag 匹配版本号形式的 tag（可带预发布后缀）。
var (
	semverPre  = regexp.MustCompile(`^v?\d+\.\d+\.\d+-[0-9A-Za-z.-]+$`)
	versionTag = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
)

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
		err = w.tagCreated(ctx, ev.Repo, strings.TrimPrefix(ev.Ref, "refs/tags/"), received)
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
		if cfg.DeployWorkflow == "" || run.Conclusion != "success" || !gitea.WorkflowMatches(run.Path, cfg.DeployWorkflow) {
			return nil
		}
		d := release.Deployment{Repo: ev.Repo, SHA: run.HeadSHA, Env: cfg.DeployEnv, Source: "workflow", Ref: run.HTMLURL}
		if versionTag.MatchString(run.HeadBranch) {
			d.Tag = run.HeadBranch // 以 tag 触发的运行；分支名（如 main）不当作版本
		}
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

// tagCreated 处理只打 tag 的版本：只认版本号形式的 tag（备份、标记类 tag 忽略）。tag 常随 Gitea Release 一起创建，
// 两个事件顺序不定，先等待 deployWait，之后若该 tag 已有 Release（是否预发布以它为准）交给 release 事件处理。
// 提交用 GetTag 解析（附注 tag 的事件 sha 是 tag 对象而非提交）。
func (w *Worker) tagCreated(ctx context.Context, repo, tag string, received time.Time) error {
	if !versionTag.MatchString(tag) {
		return nil
	}
	if wait := received.Add(deployWait); w.o.Now().Before(wait) {
		return &ticket.DeferError{Until: wait, Reason: "waiting for a possible release event of the tag"}
	}
	owner, name, _ := strings.Cut(repo, "/")
	_, err := w.o.Git.GetReleaseByTag(ctx, owner, name, tag)
	switch {
	case err == nil:
		return nil
	case !errors.Is(err, gitea.ErrNotFound):
		return giteaErr("get release "+tag, err)
	}
	return w.o.Releases.Published(ctx, release.Release{Repo: repo, Tag: tag, Name: tag,
		Prerelease: semverPre.MatchString(tag), PublishedAt: received}, nil, true)
}

// deployed 记录上线。版本记录尚未到达时等待 deployWait；之后带 tag 的信号（工作流以 tag 触发）按 tag 补建版本记录
// 再记录上线，只有提交的信号无法对应版本时放弃。
func (w *Worker) deployed(ctx context.Context, d release.Deployment, received time.Time) error {
	err := w.o.Releases.Deployed(ctx, d)
	if !errors.Is(err, release.ErrNotFound) {
		return err
	}
	if wait := received.Add(deployWait); w.o.Now().Before(wait) {
		return &ticket.DeferError{Until: wait, Reason: "release for deployment not recorded yet"}
	}
	if d.Tag != "" && versionTag.MatchString(d.Tag) {
		err := w.o.Releases.Published(ctx, release.Release{Repo: d.Repo, Tag: d.Tag, Name: d.Tag,
			Prerelease: semverPre.MatchString(d.Tag), PublishedAt: d.At}, nil, true)
		if err != nil && !errors.Is(err, release.ErrNotFound) {
			return err
		}
		if err == nil {
			return w.o.Releases.Deployed(ctx, d)
		}
	}
	w.log.Warn("deployment does not match any release", "repo", d.Repo, "tag", d.Tag, "sha", d.SHA)
	return nil
}

// releaseRefs 解析版本说明中的 Issue 引用（补全仓库、去重）。
func releaseRefs(repo, body string) []release.IssueRef {
	var out []release.IssueRef
	for _, r := range resolveRefs(repo, gitea.ParseIssueRefs(body)) {
		out = append(out, release.IssueRef{Repo: r.Repo, Number: r.Number, Text: r.Text})
	}
	return out
}
