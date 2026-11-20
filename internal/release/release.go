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

// Candidate 是可能随版本发布或上线的工单及其修复提交。
type Candidate struct {
	ID          int64
	FixedCommit string
}

// ErrNotFound 表示仓库不属于任何项目或版本不存在。
var ErrNotFound = errors.New("release: not found")

// Tx 是事务内的版本操作，同时具备工单操作（推进状态、写事件）。
type Tx interface {
	ticket.Tx
	// RepoID 按 "owner/name"（不区分大小写）查找仓库；不属于任何项目返回 ErrNotFound。
	RepoID(ctx context.Context, repo string) (int64, error)
	// UpsertRelease 按仓库 + tag 写入版本并回填 ID 与库中的值。fromTag 为真（只来自 tag）时不覆盖已有的
	// Release 名称、说明与预发布标记，只补提交。
	UpsertRelease(ctx context.Context, r *Release, fromTag bool) error
	// Candidates 返回仓库中处于 status 的工单及其修复提交。
	Candidates(ctx context.Context, repoID int64, status ticket.Status) ([]Candidate, error)
	// LinkTicket 记录工单与版本的关联（ref 为 "commit" 或说明中的引用原文），已存在时忽略。
	LinkTicket(ctx context.Context, releaseID, ticketID int64, ref string) error
	// FindRelease 按 tag 或提交在仓库中查找版本（tag 优先）；不存在返回 ErrNotFound。
	FindRelease(ctx context.Context, repoID int64, tag, sha string) (*Release, error)
	// AddDeployment 记录版本在环境 env 上线，已记录过时忽略。
	AddDeployment(ctx context.Context, releaseID int64, env, source, ref string, at time.Time) error
	// Deployments 返回版本已记录的上线（环境 → 时间）。
	Deployments(ctx context.Context, releaseID int64) (map[string]time.Time, error)
}

// Store 是版本仓储，由 internal/store 实现。
type Store interface {
	InReleaseTx(ctx context.Context, fn func(Tx) error) error
}

// Git 读取仓库提交信息，由 worker 基于 gitea.Client 实现。
type Git interface {
	// TagCommit 返回 tag 所指提交（附注 tag 也解析到提交）；tag 不存在时返回空串。
	TagCommit(ctx context.Context, repo, tag string) (string, error)
	// Contains 报告提交 sha 是否已包含在 ref（tag 或提交）中；任一端不存在时为 false。
	Contains(ctx context.Context, repo, ref, sha string) (bool, error)
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
	Text   string // 引用原文，如 "#12"
}

// Published 处理一个已发布的版本（Release 发布 / 更新，或只打了 tag）：保存版本记录；逐个检查仓库中已修复
// （fixed）工单的修复提交是否包含在该版本中，再按说明中对本仓库 Issue 的引用补漏（只用于修复提交未知的工单）。
// 正式版本把命中的工单推进为 released 并通知，预发布只记录关联。按提交是否包含判定，不依赖版本先后，
// 维护分支并行发版、首个版本都适用。版本已记录过上线时，新发布的工单同时推进为 deployed。可重复调用（幂等）。
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
	var cands []Candidate
	if err := s.o.Store.InReleaseTx(ctx, func(tx Tx) error {
		id, err := tx.RepoID(ctx, r.Repo)
		if err != nil {
			return err
		}
		r.RepoID = id
		if err := tx.UpsertRelease(ctx, &r, fromTag); err != nil {
			return err
		}
		cands, err = tx.Candidates(ctx, id, ticket.StatusFixed)
		return err
	}); err != nil {
		return err
	}
	matched, err := s.contained(ctx, r.Repo, r.Tag, cands)
	if err != nil {
		return err
	}
	return s.o.Store.InReleaseTx(ctx, func(tx Tx) error {
		for _, ref := range refs {
			if !strings.EqualFold(ref.Repo, r.Repo) {
				continue // 其他仓库的 Issue：修复不在本仓库的版本里
			}
			t, err := tx.LockByIssue(ctx, ref.Repo, ref.Number)
			if errors.Is(err, ticket.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if _, ok := matched[t.ID]; !ok && t.Status == ticket.StatusFixed && t.FixedCommit == "" {
				matched[t.ID] = ref.Text
			}
		}
		deployed, err := tx.Deployments(ctx, r.ID)
		if err != nil {
			return err
		}
		for id, ref := range matched {
			if err := tx.LinkTicket(ctx, r.ID, id, ref); err != nil {
				return err
			}
			if r.Prerelease {
				continue
			}
			if err := s.transit(ctx, tx, id, &r, ticket.StatusFixed, ticket.StatusReleased, ref, "", time.Time{}); err != nil {
				return err
			}
			for env, at := range deployed {
				if err := s.transit(ctx, tx, id, &r, ticket.StatusReleased, ticket.StatusDeployed, "", env, at); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// contained 返回修复提交包含在 ref 中的候选工单（值为关联来源 "commit"）。
func (s *Service) contained(ctx context.Context, repo, ref string, cands []Candidate) (map[int64]string, error) {
	out := map[int64]string{}
	for _, c := range cands {
		if c.FixedCommit == "" {
			continue
		}
		ok, err := s.o.Git.Contains(ctx, repo, ref, c.FixedCommit)
		if err != nil {
			return nil, err
		}
		if ok {
			out[c.ID] = "commit"
		}
	}
	return out, nil
}

// transit 把工单从 from 推进到 to（released 记录修复版本，deployed 记录上线时间）并通知；状态不符时不变。
func (s *Service) transit(ctx context.Context, tx Tx, id int64, r *Release, from, to ticket.Status, ref, env string, at time.Time) error {
	t, err := tx.LockByID(ctx, id)
	if err != nil {
		return err
	}
	if t.Status != from {
		return nil
	}
	t.Status = to
	kind, body := notify.TicketReleased, "已在 "+r.Tag+" 发布"
	payload := map[string]any{"release": r.Tag, "repo": r.Repo}
	if to == ticket.StatusDeployed {
		t.DeployedAt = &at
		kind, body = notify.TicketDeployed, r.Tag+" 已上线（"+env+"）"
		payload["env"] = env
	} else {
		t.FixedReleaseID = r.ID
		payload["ref"] = ref
	}
	if err := tx.Update(ctx, t); err != nil {
		return err
	}
	if err := tx.AddHistory(ctx, ticket.History{TicketID: t.ID, Source: "gitea", Kind: string(kind), Payload: payload}); err != nil {
		return err
	}
	e := ticket.EventFor(t, kind, from, "", body)
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

// Deployed 记录版本上线：仓库中已发布（released）且修复提交包含在上线版本中的工单推进为 deployed 并通知；
// 修复提交未知的工单按其修复版本就是该版本判定。预发布版本的上线（如预发环境）只记录。
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
	var r *Release
	var cands []Candidate
	if err := s.o.Store.InReleaseTx(ctx, func(tx Tx) error {
		repoID, err := tx.RepoID(ctx, d.Repo)
		if err != nil {
			return err
		}
		if r, err = tx.FindRelease(ctx, repoID, d.Tag, d.SHA); err != nil {
			return err
		}
		r.Repo = d.Repo
		if err := tx.AddDeployment(ctx, r.ID, d.Env, d.Source, d.Ref, d.At); err != nil {
			return err
		}
		if r.Prerelease {
			return nil
		}
		cands, err = tx.Candidates(ctx, repoID, ticket.StatusReleased)
		return err
	}); err != nil || r.Prerelease || len(cands) == 0 {
		return err
	}
	ref := r.Tag
	if d.SHA != "" {
		ref = d.SHA
	}
	matched, err := s.contained(ctx, d.Repo, ref, cands)
	if err != nil {
		return err
	}
	return s.o.Store.InReleaseTx(ctx, func(tx Tx) error {
		for _, c := range cands {
			if _, ok := matched[c.ID]; !ok {
				if c.FixedCommit != "" {
					continue
				}
				t, err := tx.LockByID(ctx, c.ID)
				if err != nil {
					return err
				}
				if t.FixedReleaseID != r.ID {
					continue
				}
			}
			if err := s.transit(ctx, tx, c.ID, r, ticket.StatusReleased, ticket.StatusDeployed, "", d.Env, d.At); err != nil {
				return err
			}
		}
		return nil
	})
}
