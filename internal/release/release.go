package release

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/notify"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// Release 是一个版本记录（Gitea Release 或只打了 tag 的版本）。
type Release struct {
	ID          int64
	RepoID      int64
	Repo        string // "owner/name"
	Tag         string
	Name        string
	Body        string
	Prerelease  bool
	PublishedAt time.Time
	GiteaID     int64  // Gitea Release ID；只有 tag 时为 0
	CommitSHA   string // tag 所指提交
	URL         string
}

// FixedTicket 是等待随版本发布的工单（状态 fixed）及其修复信息。
type FixedTicket struct {
	ID          int64
	Repo        string
	IssueNumber int64
	FixedCommit string
	FixedPR     int64
	UpdatedAt   time.Time
}

// ErrNotFound 表示仓库不属于任何项目或版本不存在。
var ErrNotFound = errors.New("release: not found")

// Tx 是事务内的版本操作，同时具备工单操作（推进状态、写事件）。
type Tx interface {
	ticket.Tx
	// RepoID 按 "owner/name"（不区分大小写）查找仓库；不属于任何项目返回 ErrNotFound。
	RepoID(ctx context.Context, repo string) (int64, error)
	// UpsertRelease 按仓库 + tag 写入版本并回填 ID。fromTag 为真（只来自 tag 事件）时不覆盖已有的
	// Release 名称、说明与预发布标记，只补提交。
	UpsertRelease(ctx context.Context, r *Release, fromTag bool) error
	// PreviousRelease 返回同仓库在 r 之前发布的最近一个正式版本；没有时返回 nil。
	PreviousRelease(ctx context.Context, r *Release) (*Release, error)
	// FixedTickets 返回仓库中状态为 fixed 的工单。
	FixedTickets(ctx context.Context, repoID int64) ([]FixedTicket, error)
	// LinkTicket 记录工单与版本的关联（ref 为 "commit" 或说明中的引用原文），已存在时忽略。
	LinkTicket(ctx context.Context, releaseID, ticketID int64, ref string) error
	// FindRelease 按 tag 或提交在仓库中查找版本（tag 优先）；不存在返回 ErrNotFound。
	FindRelease(ctx context.Context, repoID int64, tag, sha string) (*Release, error)
	// AddDeployment 记录版本在环境 env 上线，已记录过时 inserted 为 false。
	AddDeployment(ctx context.Context, releaseID int64, env, source, ref string, at time.Time) (inserted bool, err error)
	// ReleasedTickets 返回仓库中状态为 released、修复版本不晚于 r 的工单 ID。
	ReleasedTickets(ctx context.Context, repoID int64, r *Release) ([]int64, error)
}

// Store 是版本仓储，由 internal/store 实现。
type Store interface {
	InReleaseTx(ctx context.Context, fn func(Tx) error) error
}

// Git 读取仓库提交信息，由 worker 基于 gitea.Client 实现。
type Git interface {
	// TagCommit 返回 tag 所指提交；tag 不存在时返回空串。
	TagCommit(ctx context.Context, repo, tag string) (string, error)
	// Commits 返回 base...head 之间的提交 SHA；任一端不存在（如 tag 已删除）时 ok 为 false。
	Commits(ctx context.Context, repo, base, head string) (shas []string, ok bool, err error)
}

// Options 是 Service 的配置。
type Options struct {
	Store Store
	Git   Git
	Now   func() time.Time
}

// Service 同步版本记录、关联工单的修复版本、识别上线（PRD 3.2.0.4）。
type Service struct{ o Options }

// New 创建 Service。
func New(o Options) (*Service, error) {
	if o.Store == nil || o.Git == nil {
		return nil, errors.New("release: Store and Git are required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{o: o}, nil
}

// IssueRef 是版本说明中对 Issue 的引用，Repo 已补全。
type IssueRef struct {
	Repo   string
	Number int64
	Text   string // 引用原文，如 "#12"、"org/api#12"
}

// Published 处理一个已发布的版本（Release 发布 / 更新，或只打了 tag）：保存版本记录；按“上一正式版本...本版本”
// 区间内的提交匹配工单的修复提交，再按说明中的 Issue 引用补漏；正式版本把命中的 fixed 工单推进为 released 并通知，
// 预发布只记录关联。仓库首个版本没有可比较的区间，此前已修复的工单都算包含在内。可重复调用（幂等）。
func (s *Service) Published(ctx context.Context, r Release, refs []IssueRef, fromTag bool) error {
	if r.CommitSHA == "" {
		sha, err := s.o.Git.TagCommit(ctx, r.Repo, r.Tag)
		if err != nil {
			return err
		}
		r.CommitSHA = sha
	}
	if r.PublishedAt.IsZero() {
		r.PublishedAt = s.o.Now()
	}
	var prev *Release
	if err := s.o.Store.InReleaseTx(ctx, func(tx Tx) error {
		id, err := tx.RepoID(ctx, r.Repo)
		if err != nil {
			return err
		}
		r.RepoID = id
		if err := tx.UpsertRelease(ctx, &r, fromTag); err != nil {
			return err
		}
		prev, err = tx.PreviousRelease(ctx, &r)
		return err
	}); err != nil {
		return err
	}
	var inRange map[string]bool
	if prev != nil {
		shas, ok, err := s.o.Git.Commits(ctx, r.Repo, prev.Tag, r.Tag)
		if err != nil {
			return err
		}
		inRange = map[string]bool{} // 比较不了时为空：只按说明中的引用关联，不能当成首个版本全部算入
		if !ok {
			shas = nil
		}
		for _, sha := range shas {
			inRange[strings.ToLower(sha)] = true
		}
	}
	return s.o.Store.InReleaseTx(ctx, func(tx Tx) error {
		fixed, err := tx.FixedTickets(ctx, r.RepoID)
		if err != nil {
			return err
		}
		matched := map[int64]string{}
		for _, t := range fixed {
			switch {
			case inRange != nil && t.FixedCommit != "" && inRange[strings.ToLower(t.FixedCommit)]:
				matched[t.ID] = "commit"
			case inRange == nil && !t.UpdatedAt.After(r.PublishedAt):
				matched[t.ID] = "commit" // 首个版本：此前已修复的都包含在内
			}
		}
		for _, ref := range refs {
			t, err := tx.LockByIssue(ctx, ref.Repo, ref.Number)
			if errors.Is(err, ticket.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if _, ok := matched[t.ID]; !ok && t.Status == ticket.StatusFixed {
				matched[t.ID] = ref.Text
			}
		}
		for id, ref := range matched {
			if err := tx.LinkTicket(ctx, r.ID, id, ref); err != nil {
				return err
			}
			if r.Prerelease {
				continue
			}
			if err := s.release(ctx, tx, id, &r, ref); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) release(ctx context.Context, tx Tx, id int64, r *Release, ref string) error {
	t, err := tx.LockByID(ctx, id)
	if err != nil {
		return err
	}
	if t.Status != ticket.StatusFixed {
		return nil
	}
	prev := t.Status
	t.Status, t.FixedReleaseID = ticket.StatusReleased, r.ID
	if err := tx.Update(ctx, t); err != nil {
		return err
	}
	if err := tx.AddHistory(ctx, ticket.History{TicketID: t.ID, Source: "gitea", Kind: string(notify.TicketReleased),
		Payload: map[string]any{"release": r.Tag, "repo": r.Repo, "ref": ref}}); err != nil {
		return err
	}
	e := ticket.EventFor(t, notify.TicketReleased, prev, "", "已在 "+r.Tag+" 发布")
	e.URL = r.URL
	return tx.Emit(ctx, e)
}

// Deployment 是一次上线信号。
type Deployment struct {
	Repo   string
	Tag    string // 版本 tag；与 SHA 至少一个
	SHA    string
	Env    string // 默认 production
	Source string // workflow | status | manual
	Ref    string // 来源说明，如工作流运行地址
	At     time.Time
}

// Deployed 记录版本上线：仓库中修复版本不晚于该版本、状态为 released 的工单推进为 deployed 并通知。
// 找不到对应版本时返回 ErrNotFound。可重复调用（幂等）。
func (s *Service) Deployed(ctx context.Context, d Deployment) error {
	if d.Tag == "" && d.SHA == "" {
		return fmt.Errorf("%w: tag or sha is required", ErrNotFound)
	}
	if d.Env == "" {
		d.Env = "production"
	}
	if d.At.IsZero() {
		d.At = s.o.Now()
	}
	return s.o.Store.InReleaseTx(ctx, func(tx Tx) error {
		repoID, err := tx.RepoID(ctx, d.Repo)
		if err != nil {
			return err
		}
		r, err := tx.FindRelease(ctx, repoID, d.Tag, d.SHA)
		if err != nil {
			return err
		}
		if _, err := tx.AddDeployment(ctx, r.ID, d.Env, d.Source, d.Ref, d.At); err != nil {
			return err
		}
		if r.Prerelease {
			return nil // 预发布上线（如预发环境）不算工单上线
		}
		ids, err := tx.ReleasedTickets(ctx, repoID, r)
		if err != nil {
			return err
		}
		for _, id := range ids {
			t, err := tx.LockByID(ctx, id)
			if err != nil {
				return err
			}
			if t.Status != ticket.StatusReleased {
				continue
			}
			prev, at := t.Status, d.At
			t.Status, t.DeployedAt = ticket.StatusDeployed, &at
			if err := tx.Update(ctx, t); err != nil {
				return err
			}
			source := "gitea"
			if d.Source == "manual" {
				source = "go"
			}
			if err := tx.AddHistory(ctx, ticket.History{TicketID: t.ID, Source: source, Kind: string(notify.TicketDeployed),
				Payload: map[string]any{"release": r.Tag, "env": d.Env, "signal": d.Source, "ref": d.Ref}}); err != nil {
				return err
			}
			e := ticket.EventFor(t, notify.TicketDeployed, prev, "", r.Tag+" 已上线（"+d.Env+"）")
			e.URL = r.URL
			if err := tx.Emit(ctx, e); err != nil {
				return err
			}
		}
		return nil
	})
}
