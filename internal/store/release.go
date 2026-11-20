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

func (x releaseTx) Candidates(ctx context.Context, repoID int64, status ticket.Status) ([]release.Candidate, error) {
	rows, err := x.tx.QueryContext(ctx, `SELECT id, fixed_commit FROM tickets WHERE repo_id = $1 AND status = $2 ORDER BY id`,
		repoID, string(status))
	if err != nil {
		return nil, fmt.Errorf("store: release candidates: %w", err)
	}
	defer rows.Close()
	var out []release.Candidate
	for rows.Next() {
		var c release.Candidate
		if err := rows.Scan(&c.ID, &c.FixedCommit); err != nil {
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

func (x releaseTx) Deployments(ctx context.Context, releaseID int64) (map[string]time.Time, error) {
	rows, err := x.tx.QueryContext(ctx, "SELECT env, deployed_at FROM deployments WHERE release_id = $1", releaseID)
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
