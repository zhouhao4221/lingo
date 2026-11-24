package assistant

import (
	"context"
	"fmt"
	"strings"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/textutil"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

const maxTitleRunes = 60

// caller 校验调用者：会话按用户 ID 隔离，必须有 ID 与固定角色。
func caller(p auth.Principal) error {
	if p.ID == "" || !p.Known() {
		return fmt.Errorf("%w: caller has no user id or role", ticket.ErrForbidden)
	}
	return nil
}

// Create 新建会话；title 可空（由首条提问生成）。
func (s *Service) Create(ctx context.Context, p auth.Principal, title string) (*Conversation, error) {
	if err := caller(p); err != nil {
		return nil, err
	}
	return s.o.Store.CreateConversation(ctx, p.ID, cleanTitle(title))
}

// List 返回本人的会话，按最近活动倒序；before 为上一页最后一条的 Seq。
func (s *Service) List(ctx context.Context, p auth.Principal, before int64, limit int) ([]*Conversation, error) {
	if err := caller(p); err != nil {
		return nil, err
	}
	return s.o.Store.ListConversations(ctx, p.ID, before, ticket.Limit(limit, ticket.DefaultLimit))
}

// Messages 返回本人会话中 id < beforeID 的最近 limit 条消息（页内按 ID 升序）。
func (s *Service) Messages(ctx context.Context, p auth.Principal, conversationID, beforeID int64, limit int) ([]*Message, error) {
	if err := caller(p); err != nil {
		return nil, err
	}
	if _, err := s.o.Store.GetConversation(ctx, p.ID, conversationID); err != nil {
		return nil, err
	}
	ms, err := s.o.Store.Messages(ctx, conversationID, beforeID, ticket.Limit(limit, ticket.DefaultLimit))
	if err != nil {
		return nil, err
	}
	now := s.o.Now()
	for _, m := range ms {
		if a := m.Action; a != nil && a.Status == ActionPending && !now.Before(a.ExpiresAt) {
			a.Status = ActionExpired // 惰性过期：读取时按过期展示，下次写入时落库
		}
	}
	return ms, nil
}

// Delete 删除本人的会话（软删除：统计取自消息行，删除后不再可见）。
func (s *Service) Delete(ctx context.Context, p auth.Principal, conversationID int64) error {
	if err := caller(p); err != nil {
		return err
	}
	return s.o.Store.DeleteConversation(ctx, p.ID, conversationID)
}

func cleanTitle(s string) string {
	return textutil.Clip(strings.Join(strings.Fields(s), " "), maxTitleRunes)
}
