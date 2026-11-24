package lingo

import (
	"context"

	"github.com/zhouhao4221/lingo/internal/auth"
)

// viewer 是只读请求的调用者身份：principal 之外，external 用户带上 Config.TicketAudience 返回的额外可见提交人
// （Principal.Peers，只用于 ticket.Readable）。写操作用 principal，不调用回调。
func (l *Lingo) viewer(ctx context.Context, u User) auth.Principal {
	p := l.principal(u)
	if l.cfg.TicketAudience == nil || u.Role != RoleExternal || u.ID == "" {
		return p
	}
	a, err := l.cfg.TicketAudience(ctx, u)
	if err != nil {
		l.log.Warn("lingo: TicketAudience failed; showing only the caller's own tickets", "user", u.ID, "err", err)
		return p
	}
	var truncated bool
	p.Peers, truncated = audienceIDs(u.ID, a.UserIDs)
	if truncated {
		l.log.Warn("lingo: TicketAudience returned too many users; extra ones ignored", "user", u.ID, "returned", len(a.UserIDs),
			"max", MaxTicketAudience)
	}
	return p
}

// audienceIDs 去重、去掉空串与调用者本人，最多保留前 MaxTicketAudience 个；truncated 报告是否丢弃了超出上限的 ID。
func audienceIDs(self string, ids []string) (out []string, truncated bool) {
	seen := make(map[string]bool, min(len(ids), MaxTicketAudience))
	for _, id := range ids {
		if id == "" || id == self || seen[id] {
			continue
		}
		seen[id] = true
		if len(out) == MaxTicketAudience {
			return out, true
		}
		out = append(out, id)
	}
	return out, false
}
