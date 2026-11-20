package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/release"
)

var _ release.Store = (*Store)(nil)

type releaseTx struct{ *ticketTx }

// InReleaseTx 实现 release.Store。
func (s *Store) InReleaseTx(ctx context.Context, fn func(release.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // 提交后回滚为空操作
	if err := fn(releaseTx{&ticketTx{tx: tx, notify: s.notify}}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
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
		q += `name = EXCLUDED.name, body = EXCLUDED.body, prerelease = EXCLUDED.prerelease, published_at = EXCLUDED.published_at,
		    gitea_release_id = EXCLUDED.gitea_release_id, html_url = EXCLUDED.html_url,
		    commit_sha = CASE WHEN EXCLUDED.commit_sha = '' THEN releases.commit_sha ELSE EXCLUDED.commit_sha END, synced_at = now()`
	}
	q += ` RETURNING id, name, body, prerelease, published_at, commit_sha, html_url`
	var published sql.NullTime
	err := x.tx.QueryRowContext(ctx, q, r.RepoID, r.Tag, r.Name, r.Body, r.Prerelease, nullTime(&r.PublishedAt),
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

func (x releaseTx) PreviousRelease(ctx context.Context, r *release.Release) (*release.Release, error) {
	p, err := scanRelease(x.tx.QueryRowContext(ctx, "SELECT "+releaseColumns+` FROM releases r
		WHERE r.repo_id = $1 AND r.id <> $2 AND NOT r.prerelease AND COALESCE(r.published_at, r.synced_at) < $3
		ORDER BY COALESCE(r.published_at, r.synced_at) DESC, r.id DESC LIMIT 1`, r.RepoID, r.ID, r.PublishedAt))
	if errors.Is(err, release.ErrNotFound) {
		return nil, nil
	}
	return p, err
}

func (x releaseTx) FixedTickets(ctx context.Context, repoID int64) ([]release.FixedTicket, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT t.id, r.owner || '/' || r.name, COALESCE(t.gitea_issue_number, 0), t.fixed_commit,
		COALESCE(t.fixed_pr, 0), t.updated_at FROM tickets t JOIN repos r ON r.id = t.repo_id
		WHERE t.repo_id = $1 AND t.status = 'fixed' ORDER BY t.id`, repoID)
	if err != nil {
		return nil, fmt.Errorf("store: fixed tickets: %w", err)
	}
	defer rows.Close()
	var out []release.FixedTicket
	for rows.Next() {
		var t release.FixedTicket
		if err := rows.Scan(&t.ID, &t.Repo, &t.IssueNumber, &t.FixedCommit, &t.FixedPR, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
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

func (x releaseTx) AddDeployment(ctx context.Context, releaseID int64, env, source, ref string, at time.Time) (bool, error) {
	res, err := x.tx.ExecContext(ctx, `INSERT INTO deployments (release_id, env, source, ref, deployed_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (release_id, env) DO NOTHING`, releaseID, env, source, ref, at)
	if err != nil {
		return false, fmt.Errorf("store: add deployment: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (x releaseTx) ReleasedTickets(ctx context.Context, repoID int64, r *release.Release) ([]int64, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT t.id FROM tickets t JOIN releases fr ON fr.id = t.fixed_release_id
		WHERE t.repo_id = $1 AND t.status = 'released'
		  AND (fr.id = $2 OR COALESCE(fr.published_at, fr.synced_at) <= $3)
		ORDER BY t.id`, repoID, r.ID, r.PublishedAt)
	if err != nil {
		return nil, fmt.Errorf("store: released tickets: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
