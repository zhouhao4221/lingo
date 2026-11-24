package assistant

import (
	"context"
	"fmt"
	"github.com/zhouhao4221/lingo/internal/knowledge"
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
	if err := s.recheckCites(ctx, p, ms); err != nil {
		return nil, err
	}
	return ms, nil
}

// recheckCites 按当前可读范围重新判断消息引用的工单：已不可读的（如同事已不在 TicketAudience 名单内，或项目
// 不在调用者范围内）只保留编号，标题、状态与 Issue 地址置空。回答正文是生成时的文本，不做处理（REQ-004）。
func (s *Service) recheckCites(ctx context.Context, p auth.Principal, ms []*Message) error {
	if err := s.recheckKnowledge(ctx, p, ms); err != nil {
		return err
	}
	var ids []int64
	for _, m := range ms {
		for _, r := range m.Citations.Tickets {
			ids = append(ids, r.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	ts, err := s.o.TicketsByID(ctx, ids)
	if err != nil {
		return err
	}
	readable := make(map[int64]bool, len(ts))
	for _, t := range ts {
		readable[t.ID] = ticket.Readable(p, t)
	}
	for _, m := range ms {
		for i := range m.Citations.Tickets {
			if r := &m.Citations.Tickets[i]; !readable[r.ID] {
				r.Title, r.Status, r.IssueURL = "", "", ""
			}
		}
	}
	return nil
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

// recheckKnowledge 按当前状态重新判断消息引用的知识条目：已停用、已删除或不在调用者项目内的只保留编号；来源工单只在
// 调用者仍能查看时保留（REQ-006）。
func (s *Service) recheckKnowledge(ctx context.Context, p auth.Principal, ms []*Message) error {
	if s.o.Knowledge == nil {
		return nil
	}
	var ids, srcs []int64
	for _, m := range ms {
		for _, r := range m.Citations.Knowledge {
			ids = append(ids, r.ID)
			if r.SourceTicketID > 0 {
				srcs = append(srcs, r.SourceTicketID)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	es, err := s.o.Knowledge.ByIDs(ctx, ids)
	if err != nil {
		return err
	}
	live := make(map[int64]bool, len(es))
	for _, e := range es {
		live[e.ID] = e.Status == knowledge.StatusActive && p.InProject(e.ProjectKey)
	}
	readable := map[int64]bool{}
	if len(srcs) > 0 {
		ts, err := s.o.TicketsByID(ctx, srcs)
		if err != nil {
			return err
		}
		for _, t := range ts {
			readable[t.ID] = ticket.Readable(p, t)
		}
	}
	for _, m := range ms {
		for i := range m.Citations.Knowledge {
			r := &m.Citations.Knowledge[i]
			if !live[r.ID] {
				r.Question, r.SourceTicketID = "", 0
			} else if !readable[r.SourceTicketID] {
				r.SourceTicketID = 0
			}
		}
	}
	return nil
}
