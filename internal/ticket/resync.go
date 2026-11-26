package ticket

import (
	"context"
	"fmt"

	"github.com/zhouhao4221/lingo/internal/auth"
)

// ResyncTarget 校验调用者能否重新同步工单 id 的历史 Gitea 评论（REQ-011）并返回工单：只允许 reviewer / admin
// （ErrForbidden，先于查找，不暴露工单是否存在）；工单不存在或不在调用者项目范围内返回 ErrNotFound；没有关联的
// 主 Issue（未转出、待建单、合并到其他工单等）返回 ErrConflict。
func (s *Service) ResyncTarget(ctx context.Context, p auth.Principal, id int64) (*Ticket, error) {
	if !p.Staff() {
		return nil, fmt.Errorf("%w: resyncing comments requires reviewer or admin", ErrForbidden)
	}
	t, err := s.o.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !Visible(p, t) {
		return nil, ErrNotFound
	}
	if t.Repo == "" || t.IssueNumber == 0 {
		return nil, fmt.Errorf("%w: ticket has no linked Gitea issue", ErrConflict)
	}
	return t, nil
}
