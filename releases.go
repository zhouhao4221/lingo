package lingo

import (
	"context"
	"errors"

	"github.com/zhouhao4221/lingo/internal/release"
)

// ListProjects 返回 u 可访问的项目及其仓库（Config 中声明的项目，按 User.Projects 过滤）。
func (l *Lingo) ListProjects(ctx context.Context, u User) ([]ProjectInfo, error) {
	p := l.principal(u)
	if !p.Known() {
		return nil, ErrForbidden
	}
	var out []ProjectInfo
	for _, pr := range l.cfg.Projects {
		if !p.InProject(pr.Key) {
			continue
		}
		info := ProjectInfo{Key: pr.Key, Name: pr.Name, Description: pr.Description, DefaultRepo: pr.DefaultRepo, Repos: []RepoInfo{}}
		for _, r := range pr.Repos {
			kind := r.Kind
			if kind == "" {
				kind = RepoOther
			}
			info.Repos = append(info.Repos, RepoInfo{Repo: r.Owner + "/" + r.Name, Kind: kind})
		}
		if info.DefaultRepo == "" && len(info.Repos) > 0 {
			info.DefaultRepo = info.Repos[0].Repo
		}
		out = append(out, info)
	}
	return out, nil
}

// ListReleases 返回 u 可访问项目的版本记录，新的在前；project、repo 可空；beforeID 为上一页最后一条的 ID；
// limit 为 0 时默认 50，最多 200。
func (l *Lingo) ListReleases(ctx context.Context, u User, project, repo string, beforeID int64, limit int) ([]Release, error) {
	if err := l.ensureProjects(ctx); err != nil {
		return nil, err
	}
	rs, err := l.releases.List(ctx, l.principal(u), release.Query{Project: project, Repo: repo, BeforeID: beforeID, Limit: limit})
	if err != nil {
		return nil, publicErr(err)
	}
	out := make([]Release, 0, len(rs))
	for _, r := range rs {
		out = append(out, publicRelease(r))
	}
	return out, nil
}

// GetRelease 返回版本详情：关联工单只含 u 可见的；版本所在项目不可访问时返回 ErrNotFound。
func (l *Lingo) GetRelease(ctx context.Context, u User, id int64) (ReleaseDetail, error) {
	p := l.principal(u)
	d, err := l.releases.Get(ctx, p, id, l.tickets.GetMany)
	if errors.Is(err, release.ErrNotFound) {
		return ReleaseDetail{}, ErrNotFound
	}
	if err != nil {
		return ReleaseDetail{}, publicErr(err)
	}
	out := ReleaseDetail{Release: publicRelease(d.Release), Tickets: []Ticket{}, Deployments: []Deployment{}}
	for _, t := range d.Tickets {
		out.Tickets = append(out.Tickets, l.publicTicket(t))
	}
	for _, dep := range d.Deployments {
		out.Deployments = append(out.Deployments, Deployment{Env: dep.Env, Source: dep.Source, At: dep.At})
	}
	return out, nil
}

func publicRelease(r *release.Release) Release {
	return Release{ID: r.ID, Repo: r.Repo, Tag: r.Tag, Name: r.Name, Body: r.Body, Prerelease: r.Prerelease,
		PublishedAt: r.PublishedAt, URL: r.URL}
}
