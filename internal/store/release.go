package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/release"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

var _ release.Store = (*Store)(nil)

type releaseTx struct{ *ticketTx }

// InReleaseTx 实现 release.Store。
func (s *Store) InReleaseTx(ctx context.Context, fn func(release.Tx) error) error {
	return s.inTx(ctx, func(x *ticketTx) error { return fn(releaseTx{x}) })
}

// ListReleases 实现 release.Store：按记录 ID 倒序（与 before 游标一致，翻页不重不漏），通常即发布先后。
func (s *Store) ListReleases(ctx context.Context, q release.Query) ([]*release.Release, error) {
	owner, name, _ := strings.Cut(q.Repo, "/")
	rows, err := s.db.QueryContext(ctx, "SELECT "+releaseColumns+`, rp.owner || '/' || rp.name FROM releases r
		JOIN repos rp ON rp.id = r.repo_id JOIN projects p ON p.id = rp.project_id
		WHERE p.key = ANY(($1::text)::text[]) AND ($2 = '' OR p.key = $2)
		  AND ($3 = '' OR (lower(rp.owner) = lower($3) AND lower(rp.name) = lower($4)))
		  AND ($5::bigint = 0 OR r.id < $5::bigint)
		ORDER BY r.id DESC LIMIT $6`,
		textArray(q.Projects), q.Project, owner, name, q.BeforeID, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("store: list releases: %w", err)
	}
	defer rows.Close()
	var out []*release.Release
	for rows.Next() {
		var r release.Release
		if err := rows.Scan(&r.ID, &r.RepoID, &r.Tag, &r.Name, &r.Body, &r.Prerelease, &r.PublishedAt, &r.GiteaID, &r.CommitSHA,
			&r.URL, &r.Repo); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// GetRelease 实现 release.Store。
func (s *Store) GetRelease(ctx context.Context, id int64) (*release.Release, string, error) {
	var r release.Release
	var key string
	err := s.db.QueryRowContext(ctx, "SELECT "+releaseColumns+`, rp.owner || '/' || rp.name, p.key FROM releases r
		JOIN repos rp ON rp.id = r.repo_id JOIN projects p ON p.id = rp.project_id WHERE r.id = $1`, id).Scan(
		&r.ID, &r.RepoID, &r.Tag, &r.Name, &r.Body, &r.Prerelease, &r.PublishedAt, &r.GiteaID, &r.CommitSHA, &r.URL, &r.Repo, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", release.ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("store: release %d: %w", id, err)
	}
	return &r, key, nil
}

// ReleaseTicketIDs 实现 release.Store。
func (s *Store) ReleaseTicketIDs(ctx context.Context, id int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT ticket_id FROM release_tickets WHERE release_id = $1 ORDER BY ticket_id", id)
	if err != nil {
		return nil, fmt.Errorf("store: release tickets: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ReleaseDeployments 实现 release.Store。
func (s *Store) ReleaseDeployments(ctx context.Context, id int64) ([]release.DeploymentRecord, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT env, source, deployed_at FROM deployments WHERE release_id = $1 ORDER BY deployed_at", id)
	if err != nil {
		return nil, fmt.Errorf("store: release deployments: %w", err)
	}
	defer rows.Close()
	var out []release.DeploymentRecord
	for rows.Next() {
		var d release.DeploymentRecord
		if err := rows.Scan(&d.Env, &d.Source, &d.At); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (x releaseTx) RepoID(ctx context.Context, repo string) (int64, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return 0, release.ErrNotFound
	}
	var id int64
	err := x.tx.QueryRowContext(ctx, "SELECT id FROM repos WHERE lower(owner) = lower($1) AND lower(name) = lower($2)", owner, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, release.ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: repo %s: %w", repo, err)
	}
	return id, nil
}

func (x releaseTx) UpsertRelease(ctx context.Context, r *release.Release, fromTag bool) error {
	q := `INSERT INTO releases (repo_id, tag, name, body, prerelease, published_at, gitea_release_id, commit_sha, html_url)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (repo_id, tag) DO UPDATE SET `
	if fromTag {
		// 只有 tag：不覆盖 Release 的名称、说明、发布时间与预发布标记
		q += `commit_sha = CASE WHEN releases.commit_sha = '' THEN EXCLUDED.commit_sha ELSE releases.commit_sha END, synced_at = now()`
	} else {
		q += `name = EXCLUDED.name, body = EXCLUDED.body, prerelease = EXCLUDED.prerelease,
		    published_at = COALESCE(EXCLUDED.published_at, releases.published_at),
		    gitea_release_id = EXCLUDED.gitea_release_id, html_url = EXCLUDED.html_url,
		    commit_sha = CASE WHEN EXCLUDED.commit_sha = '' THEN releases.commit_sha ELSE EXCLUDED.commit_sha END, synced_at = now()`
	}
	q += ` RETURNING id, name, body, prerelease, COALESCE(published_at, synced_at), commit_sha, html_url`
	var published sql.NullTime
	var at *time.Time
	if !r.PublishedAt.IsZero() {
		at = &r.PublishedAt
	}
	err := x.tx.QueryRowContext(ctx, q, r.RepoID, r.Tag, r.Name, r.Body, r.Prerelease, nullTime(at),
		nullInt64(r.GiteaID), r.CommitSHA, r.URL).Scan(&r.ID, &r.Name, &r.Body, &r.Prerelease, &published, &r.CommitSHA, &r.URL)
	if err != nil {
		return fmt.Errorf("store: upsert release %s %s: %w", r.Repo, r.Tag, err)
	}
	if published.Valid {
		r.PublishedAt = published.Time
	}
	return nil
}

const releaseColumns = `r.id, r.repo_id, r.tag, r.name, r.body, r.prerelease, COALESCE(r.published_at, r.synced_at),
	COALESCE(r.gitea_release_id, 0), r.commit_sha, r.html_url`

func scanRelease(row rowScanner) (*release.Release, error) {
	var r release.Release
	err := row.Scan(&r.ID, &r.RepoID, &r.Tag, &r.Name, &r.Body, &r.Prerelease, &r.PublishedAt, &r.GiteaID, &r.CommitSHA, &r.URL)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, release.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: read release: %w", err)
	}
	return &r, nil
}

// Candidates 返回修复提交在仓库 repoID 中、处于 status 的工单：修复 PR 在该仓库合并的（fixed_repo），
// 以及未记录修复仓库、工单就在该仓库的。修复 PR 可能合在同项目的另一个仓库（前端 PR 修后端 Issue）。
func (x releaseTx) Candidates(ctx context.Context, repoID int64, status ticket.Status) ([]release.Candidate, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT t.id, t.fixed_commit, COALESCE(t.fixed_release_id, 0) FROM tickets t, repos r
		WHERE r.id = $1 AND t.status = $2
		  AND (t.fixed_repo = lower(r.owner || '/' || r.name) OR (t.fixed_repo = '' AND t.repo_id = r.id))
		ORDER BY t.id`, repoID, string(status))
	if err != nil {
		return nil, fmt.Errorf("store: release candidates: %w", err)
	}
	defer rows.Close()
	var out []release.Candidate
	for rows.Next() {
		var c release.Candidate
		if err := rows.Scan(&c.ID, &c.FixedCommit, &c.FixedReleaseID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (x releaseTx) LinkTicket(ctx context.Context, releaseID, ticketID int64, ref string) error {
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO release_tickets (release_id, ticket_id, ref) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, releaseID, ticketID, ref); err != nil {
		return fmt.Errorf("store: link release ticket: %w", err)
	}
	return nil
}

func (x releaseTx) FindRelease(ctx context.Context, repoID int64, tag, sha string) (*release.Release, error) {
	return scanRelease(x.tx.QueryRowContext(ctx, "SELECT "+releaseColumns+` FROM releases r
		WHERE r.repo_id = $1 AND (($2 <> '' AND r.tag = $2) OR ($3 <> '' AND lower(r.commit_sha) = lower($3)))
		ORDER BY (r.tag = $2) DESC, r.prerelease, r.id DESC LIMIT 1`, repoID, tag, sha))
}

func (x releaseTx) AddDeployment(ctx context.Context, releaseID int64, env, source, ref string, at time.Time) error {
	if _, err := x.tx.ExecContext(ctx, `INSERT INTO deployments (release_id, env, source, ref, deployed_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (release_id, env) DO NOTHING`, releaseID, env, source, ref, at); err != nil {
		return fmt.Errorf("store: add deployment: %w", err)
	}
	return nil
}

func (x releaseTx) Deployments(ctx context.Context, r *release.Release) (map[string]time.Time, error) {
	// 同一提交上的其他版本（如先打 rc 上线、再在同一提交打正式 tag）已上线，也算本版本已上线
	rows, err := x.tx.QueryContext(ctx, `SELECT d.env, min(d.deployed_at) FROM deployments d JOIN releases o ON o.id = d.release_id
		WHERE d.release_id = $1 OR ($2 <> '' AND o.repo_id = $3 AND lower(o.commit_sha) = lower($2))
		GROUP BY d.env`, r.ID, r.CommitSHA, r.RepoID)
	if err != nil {
		return nil, fmt.Errorf("store: deployments: %w", err)
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var env string
		var at time.Time
		if err := rows.Scan(&env, &at); err != nil {
			return nil, err
		}
		out[env] = at
	}
	return out, rows.Err()
}
