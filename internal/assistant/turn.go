package assistant

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/textutil"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

const historyRunes = 1000 // 每条历史消息带给模型的长度上限

// Send 在会话中提问并返回助手消息。onDelta 非空时逐片段回调回答文本（固定回复整段回调一次）。
// attachments 是附带的图片引用（REQ-009），带图时提问可以为空；模型不支持图片或未配置读取图片时返回
// ErrImagesUnsupported（提问不保存）。模型出错、图片读取失败时提问照常保存，返回 status=failed 的助手消息（err 为 nil），
// 可用 Retry 重试。同一会话上一条提问还在处理时返回 ticket.ErrConflict。
func (s *Service) Send(ctx context.Context, p auth.Principal, convID int64, content string, attachments []Attachment, tz string,
	onDelta func(string)) (*Message, error) {
	if err := caller(p); err != nil {
		return nil, err
	}
	content = strings.TrimSpace(content)
	attachments, err := CleanAttachments(attachments)
	if err != nil {
		return nil, err
	}
	switch {
	case content == "" && len(attachments) == 0:
		return nil, fmt.Errorf("%w: content is empty", ticket.ErrInvalid)
	case utf8.RuneCountInString(content) > MaxContentRunes:
		return nil, fmt.Errorf("%w: content exceeds %d characters", ticket.ErrInvalid, MaxContentRunes)
	}
	loc, err := s.location(tz)
	if err != nil {
		return nil, err
	}
	brain := s.brain()
	if brain == nil {
		return nil, ErrNoModel
	}
	if len(attachments) > 0 && !s.imagesSupported(brain) {
		return nil, ErrImagesUnsupported // 明确拒绝，不丢掉图片只按文字回答
	}
	ctx = context.WithValue(ctx, brainKey{}, brain) // 本轮使用开始时的模型
	var conv *Conversation
	var act *Action
	var expired bool
	user := &Message{ConversationID: convID, Role: RoleUser, Content: content, Status: StatusDone, Attachments: attachments}
	err = s.o.Store.InAssistantTx(ctx, func(tx Tx) error {
		var err error
		if conv, err = s.claim(ctx, tx, p, convID); err != nil {
			return err
		}
		if err := tx.InsertMessage(ctx, user); err != nil {
			return err
		}
		act, expired, err = s.liveAction(ctx, tx, convID, user.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.run(ctx, p, conv, user, act, expired, loc, nil, onDelta)
}

// Retry 重新回答会话中失败的助手消息（须是会话的最后一条），原地更新该消息。
func (s *Service) Retry(ctx context.Context, p auth.Principal, convID, messageID int64, tz string, onDelta func(string)) (*Message, error) {
	if err := caller(p); err != nil {
		return nil, err
	}
	loc, err := s.location(tz)
	if err != nil {
		return nil, err
	}
	brain := s.brain()
	if brain == nil {
		return nil, ErrNoModel
	}
	ctx = context.WithValue(ctx, brainKey{}, brain) // 本轮使用开始时的模型
	var conv *Conversation
	var act *Action
	var expired bool
	var user, failed *Message
	err = s.o.Store.InAssistantTx(ctx, func(tx Tx) error {
		var err error
		if conv, err = s.claim(ctx, tx, p, convID); err != nil {
			return err
		}
		if failed, err = tx.GetMessage(ctx, convID, messageID); err != nil {
			return err
		}
		last, err := tx.LastUserMessageID(ctx, convID)
		if err != nil {
			return err
		}
		// 只重试最近一次提问的失败回答（之后可能插入了按钮的结果消息，不影响）
		if failed.Role != RoleAssistant || failed.Status != StatusFailed || failed.ReplyTo == 0 || last != failed.ReplyTo {
			return fmt.Errorf("%w: only the failed answer to the latest question can be retried", ticket.ErrConflict)
		}
		if user, err = tx.GetMessage(ctx, convID, failed.ReplyTo); err != nil {
			return err
		}
		if len(user.Attachments) > 0 && !s.imagesSupported(brain) {
			return ErrImagesUnsupported // 重试复用原图片：模型已换成不支持图片的，明确拒绝（事务回滚，租约不保留）
		}
		act, expired, err = s.liveAction(ctx, tx, convID, user.ID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.run(ctx, p, conv, user, act, expired, loc, failed, onDelta)
}

type brainKey struct{}

// brain 返回当前的模型调用（BrainFor 优先）。
func (s *Service) brain() Brain {
	if s.o.BrainFor != nil {
		return s.o.BrainFor()
	}
	return s.o.Brain
}

// imagesSupported 报告带图提问可用：模型声明支持图片，且配置了读取图片。
func (s *Service) imagesSupported(b Brain) bool { return s.o.FetchImage != nil && b.SupportsImages() }

// brainOf 返回本轮开始时取定的模型调用。
func (s *Service) brainOf(ctx context.Context) Brain {
	if b, ok := ctx.Value(brainKey{}).(Brain); ok {
		return b
	}
	return s.brain()
}

// claim 校验会话归属并取得生成租约；他人的或已删除的会话返回 ErrNotFound，租约未过期返回 ErrConflict。
func (s *Service) claim(ctx context.Context, tx Tx, p auth.Principal, convID int64) (*Conversation, error) {
	conv, ok, err := tx.ClaimTurn(ctx, p.ID, convID, turnLease)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: the previous message is still being answered", ticket.ErrConflict)
	}
	return conv, nil
}

// liveAction 返回会话中仍有效的待确认操作；刚过期的落库为 expired，并报告 expired。userMsgID 是本轮回答的用户消息：
// 操作提出之后、这条消息之前如果还有别的用户消息（如中间那轮回答失败了），说明用户已经转而问了别的，操作作废。
func (s *Service) liveAction(ctx context.Context, tx Tx, convID, userMsgID int64) (*Action, bool, error) {
	a, err := tx.PendingAction(ctx, convID)
	if err != nil || a == nil {
		return nil, false, err
	}
	now := s.o.Now()
	if !now.Before(a.ExpiresAt) {
		a.Status, a.ResolvedAt = ActionExpired, &now
		return nil, true, tx.ResolveAction(ctx, a)
	}
	between, err := tx.UserMessageBetween(ctx, convID, a.MessageID, userMsgID)
	if err != nil {
		return nil, false, err
	}
	if between {
		a.Status, a.ResolvedAt = ActionCancelled, &now
		return nil, false, tx.ResolveAction(ctx, a)
	}
	return a, false, nil
}

func (s *Service) location(tz string) (*time.Location, error) {
	if tz == "" {
		return s.o.Location, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("%w: time zone %q", ticket.ErrInvalid, tz)
	}
	return loc, nil
}

// run 处理一轮提问并落库助手消息（retry 非空时原地更新），最后释放租约。
func (s *Service) run(ctx context.Context, p auth.Principal, conv *Conversation, user *Message, act *Action, expired bool,
	loc *time.Location, retry *Message, onDelta func(string)) (*Message, error) {
	msg := retry
	if msg == nil {
		msg = &Message{ConversationID: conv.ID, Role: RoleAssistant, ReplyTo: user.ID}
	}
	origID := msg.ID // 重试时为原消息 ID；落库事务回滚后，InsertMessage 回填的 ID 已失效
	tctx, cancel := context.WithTimeout(ctx, turnTimeout)
	deliver := onDelta
	if onDelta != nil {
		// 超过回答时限后不再推送片段：每次推送可能阻塞到 SSE 写期限，租约要能覆盖到落库结束（见 turnLease）
		deliver = func(d string) {
			if tctx.Err() == nil {
				onDelta(d)
			}
		}
	}
	r, err := s.respond(tctx, p, conv, user, act, expired, loc, deliver)
	cancel()
	// 落库不随请求取消：客户端断开时也要记下失败并释放租约
	fctx, fcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer fcancel()
	if err != nil {
		msg.Status, msg.Error, msg.Content, msg.Outcome, msg.Citations = StatusFailed, errKind(ctx, err), "", "", Citations{}
		if ferr := s.finish(fctx, p, conv, msg, nil); ferr != nil {
			return s.salvage(ctx, conv, msg, origID, ferr)
		}
		return msg, nil
	}
	msg.Status, msg.Error = StatusDone, ""
	if err := s.finish(fctx, p, conv, msg, r); err != nil {
		return s.salvage(ctx, conv, msg, origID, err)
	}
	if onDelta != nil && !r.streamed {
		onDelta(msg.Content)
	}
	return msg, nil
}

// salvage 在落库失败时尽量记下失败的回答并释放租约，避免会话在租约过期前一直被占用；都失败时返回原错误。
// ctx 是请求的上下文：兜底另开超时，不沿用落库那次可能已经超时的上下文。
func (s *Service) salvage(ctx context.Context, conv *Conversation, msg *Message, origID int64, cause error) (*Message, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	msg.ID = origID
	msg.Status, msg.Error, msg.Content, msg.Outcome, msg.Citations, msg.Action = StatusFailed, ErrKindInternal, "", "", Citations{}, nil
	err := s.o.Store.InAssistantTx(ctx, func(tx Tx) error {
		if err := s.saveMessage(ctx, tx, conv, msg, ""); err != nil {
			return err
		}
		return tx.ReleaseTurn(ctx, conv.ID, conv.Lease)
	})
	if err != nil {
		_ = s.o.Store.InAssistantTx(ctx, func(tx Tx) error { return tx.ReleaseTurn(ctx, conv.ID, conv.Lease) })
		return nil, cause
	}
	return msg, nil
}

// modelError 标记来自模型调用（Brain）的错误，以便与数据库等内部错误区分。
type modelError struct{ err error }

func (e modelError) Error() string { return e.err.Error() }
func (e modelError) Unwrap() error { return e.err }

func errKind(ctx context.Context, err error) string {
	var timeout interface{ Timeout() bool }
	var me modelError
	var ie *imageError
	switch {
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		return ErrKindCanceled
	case errors.As(err, &ie):
		return ie.kind
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &timeout) && timeout.Timeout():
		return ErrKindTimeout
	case errors.As(err, &me):
		return ErrKindModel
	}
	return ErrKindInternal
}

// saveMessage 写入（或原地更新）助手消息并刷新会话的最近活动；title 非空且会话还没有标题时写入标题。
func (s *Service) saveMessage(ctx context.Context, tx Tx, conv *Conversation, msg *Message, title string) error {
	var err error
	if msg.ID == 0 {
		err = tx.InsertMessage(ctx, msg)
	} else {
		err = tx.UpdateMessage(ctx, msg)
	}
	if err != nil {
		return err
	}
	return tx.Touch(ctx, conv.ID, title)
}

// finish 落库助手消息、处理操作（作废、提出、执行）、刷新会话并释放租约。r 为空表示本轮失败（不动操作）。
func (s *Service) finish(ctx context.Context, p auth.Principal, conv *Conversation, msg *Message, r *reply) error {
	save := func(tx Tx) error {
		title := ""
		if r != nil && conv.Title == "" {
			title = r.title
		}
		if err := s.saveMessage(ctx, tx, conv, msg, title); err != nil {
			return err
		}
		return tx.ReleaseTurn(ctx, conv.ID, conv.Lease)
	}
	if r == nil {
		return s.o.Store.InAssistantTx(ctx, save)
	}
	msg.Intent, msg.Outcome, msg.Content, msg.Citations = r.intent, r.outcome, r.content, r.cites
	if r.execute != nil {
		var a *Action
		err := s.o.Store.InAssistantTx(ctx, func(tx Tx) error {
			var res *execResult
			var err error
			if a, res, err = s.executeTx(ctx, tx, p, conv.ID, r.execute.ID, msg.ReplyTo, msg); err != nil {
				return err
			}
			if res == nil && msg.Outcome == "" && a.Status == ActionExecuted {
				msg.Content, msg.Outcome = notPendingText(a), OutcomeNoAction
			}
			return save(tx)
		})
		if errors.Is(err, ticket.ErrConflict) {
			// 执行前操作已不在等待中（如同时点了取消按钮）：如实说明，照常落库并释放租约
			msg.Content, msg.Outcome, msg.Citations = notPendingText(a), OutcomeNoAction, Citations{}
			return s.o.Store.InAssistantTx(ctx, save)
		}
		return err
	}
	return s.o.Store.InAssistantTx(ctx, func(tx Tx) error {
		if r.cancel != nil || r.propose != nil {
			// 作废会话中仍在等待的操作（用户取消、转而问了别的，或提出了新的操作）
			a, err := tx.PendingAction(ctx, conv.ID)
			if err != nil {
				return err
			}
			if a != nil {
				now := s.o.Now()
				a.Status, a.ResolvedAt = ActionCancelled, &now
				if !now.Before(a.ExpiresAt) {
					a.Status = ActionExpired // 回答期间已经过期
				}
				if err := tx.ResolveAction(ctx, a); err != nil {
					return err
				}
			}
		}
		if err := save(tx); err != nil {
			return err
		}
		if a := r.propose; a != nil {
			a.ConversationID, a.MessageID, a.UserID, a.Status = conv.ID, msg.ID, p.ID, ActionPending
			a.ExpiresAt = s.o.Now().Add(ActionTTL)
			if err := tx.InsertAction(ctx, a); err != nil {
				return err
			}
			msg.Action = a
		}
		return nil
	})
}

// respond 决定本轮的回答（不落库）。
func (s *Service) respond(ctx context.Context, p auth.Principal, conv *Conversation, user *Message, act *Action, expired bool,
	loc *time.Location, onDelta func(string)) (*reply, error) {
	images, err := s.loadImages(ctx, conv.UserID, user)
	if err != nil {
		return nil, err
	}
	var cancelled *Action
	if act != nil && len(images) > 0 {
		// 带图的消息不作为对待确认操作的答复（不会因为图片旁的“好的”执行写操作）：操作作废，按新问题回答
		cancelled = act
	} else if act != nil {
		d, err := s.brainOf(ctx).Confirm(ctx, ConfirmInput{Summary: act.Summary, Message: user.Content})
		if err != nil {
			return nil, modelError{err}
		}
		switch d {
		case IntentConfirm:
			return &reply{execute: act, intent: IntentConfirm}, nil
		case IntentCancel:
			return &reply{cancel: act, intent: IntentCancel, content: textCancelled, outcome: OutcomeActionCancelled}, nil
		}
		cancelled = act // 用户转而问了别的：操作作废，按新问题回答
	}
	hist, err := s.history(ctx, p, conv.ID, user.ID)
	if err != nil {
		return nil, err
	}
	tc := &turnCtx{p: p, loc: loc, now: s.o.Now(), history: hist, question: user.Content, onDelta: onDelta, convID: conv.ID,
		tools: s.intentTools(ctx), images: images}
	in, err := s.brainOf(ctx).Intent(ctx, IntentInput{Today: today(tc.now, loc), Projects: s.o.Projects(p), History: hist,
		Question: user.Content, NeedTitle: conv.Title == "", Tools: tc.tools, Images: images})
	if err != nil {
		return nil, modelError{err}
	}
	if cancelled != nil && len(images) > 0 {
		if in.Kind == IntentCancel {
			return &reply{cancel: cancelled, intent: IntentCancel, content: textCancelled, outcome: OutcomeActionCancelled}, nil
		}
	} else if cancelled != nil {
		// 确认判断没认出来（如输出不合格），但意图解析认为是确认 / 取消：按用户的意思处理仍在等待的操作
		switch in.Kind {
		case IntentConfirm:
			return &reply{execute: cancelled, intent: IntentConfirm}, nil
		case IntentCancel:
			return &reply{cancel: cancelled, intent: IntentCancel, content: textCancelled, outcome: OutcomeActionCancelled}, nil
		}
	}
	r, err := s.dispatch(ctx, tc, in, expired)
	if err != nil {
		return nil, err
	}
	r.intent, r.title = in.Kind, cleanTitle(in.Title)
	if r.title == "" && conv.Title == "" {
		r.title = cleanTitle(textutil.Clip(user.Content, 30))
	}
	if r.title == "" && conv.Title == "" && len(images) > 0 {
		r.title = titleImages
	}
	if cancelled != nil && r.cancel == nil {
		r.cancel = cancelled
	}
	return r, nil
}

func (s *Service) dispatch(ctx context.Context, tc *turnCtx, in *Intent, expired bool) (*reply, error) {
	if in.UnknownProject || (in.Project != "" && !s.accessible(tc.p, in.Project)) {
		// 提到的项目不可访问：只影响按项目查询的意图，按问题类型说明
		switch in.Kind {
		case IntentTicketFind, IntentTicketFix, IntentTicketSummary:
			return &reply{content: textNotFound, outcome: OutcomeNotFound}, nil
		case IntentReleaseList:
			return &reply{content: textNoRelease, outcome: OutcomeNotFound}, nil
		case IntentKnowledgeAsk:
			return &reply{content: textNoKnowledge, outcome: OutcomeNotFound}, nil
		}
		in.Project = ""
	}
	switch in.Kind {
	case IntentTicketFind, IntentTicketFix:
		return s.findTickets(ctx, tc, in)
	case IntentTicketSummary:
		return s.summarize(ctx, tc, in)
	case IntentReleaseList:
		return s.listReleases(ctx, tc, in)
	case IntentComment:
		if len(tc.images) > 0 {
			return &reply{content: textNoImageComment, outcome: OutcomeClarify}, nil // 留言不带图片，也不在带图的消息中提出写操作
		}
		return s.proposeComment(ctx, tc, in)
	case IntentConfirm, IntentCancel:
		if expired {
			return &reply{content: textExpired, outcome: OutcomeActionExpired}, nil
		}
		return &reply{content: textNoAction, outcome: OutcomeNoAction}, nil
	case IntentKnowledgeAsk:
		if s.o.Knowledge == nil {
			return &reply{content: textOutOfScope, outcome: OutcomeOutOfScope}, nil
		}
		return s.askKnowledge(ctx, tc, in)
	case IntentOutOfScope:
		return &reply{content: textOutOfScope, outcome: OutcomeOutOfScope}, nil
	case IntentDataQuery:
		return s.queryData(ctx, tc, in)
	}
	txt := strings.TrimSpace(in.Clarify)
	if txt == "" {
		txt = textClarify
	}
	return &reply{content: txt, outcome: OutcomeClarify}, nil
}

func (s *Service) accessible(p auth.Principal, key string) bool {
	for _, pr := range s.o.Projects(p) {
		if pr.Key == key {
			return true
		}
	}
	return false
}

// history 返回当前提问之前的最近 HistoryTurns 轮已完成消息；引用的工单按当前可读范围重新判断（见 recheckCites）。
func (s *Service) history(ctx context.Context, p auth.Principal, convID, beforeID int64) ([]HistoryItem, error) {
	ms, err := s.o.Store.RecentDone(ctx, convID, beforeID, 2*HistoryTurns)
	if err != nil {
		return nil, err
	}
	if err := s.recheckCites(ctx, p, ms); err != nil {
		return nil, err
	}
	out := make([]HistoryItem, 0, len(ms))
	for _, m := range ms {
		h := HistoryItem{Role: m.Role, Content: textutil.ClipEllipsis(m.Content, historyRunes), Tickets: m.Citations.Tickets,
			Knowledge: m.Citations.Knowledge, Images: len(m.Attachments)}
		for _, r := range m.Citations.Releases {
			h.Tags = append(h.Tags, r.Repo+" "+r.Tag)
		}
		out = append(out, h)
	}
	return out, nil
}

var spaceRe = regexp.MustCompile(`\s+`)

func normSpace(s string) string { return strings.TrimSpace(spaceRe.ReplaceAllString(s, " ")) }

// proposeComment 提出留言的待确认操作。防注入：留言正文须是本条提问中的原文片段；目标工单须由本条提问给出
// （"#12" 或 "12"），或是上一条助手回答唯一引用的工单。
func (s *Service) proposeComment(ctx context.Context, tc *turnCtx, in *Intent) (*reply, error) {
	body := strings.TrimSpace(in.CommentBody)
	if body == "" || !strings.Contains(normSpace(tc.question), normSpace(body)) {
		return &reply{content: textNeedBody, outcome: OutcomeClarify}, nil
	}
	var id int64
	if len(in.TicketIDs) == 1 && mentions(tc.question, in.TicketIDs[0]) {
		id = in.TicketIDs[0]
	} else if len(in.TicketIDs) <= 1 {
		if ref, ok := lastUniqueTicket(tc.history); ok && (len(in.TicketIDs) == 0 || in.TicketIDs[0] == ref) {
			id = ref
		}
	}
	if id == 0 {
		return &reply{content: textNeedTarget, outcome: OutcomeClarify}, nil
	}
	t, err := s.o.Tickets.Get(ctx, tc.p, id)
	if errors.Is(err, ticket.ErrNotFound) {
		if _, verr := s.o.Tickets.View(ctx, tc.p, id); verr == nil {
			return &reply{content: textReadOnly, outcome: OutcomeNotFound}, nil // 同事的工单：只读（REQ-004）
		}
		return &reply{content: textNotFound, outcome: OutcomeNotFound}, nil
	}
	if err != nil {
		return nil, err
	}
	payload, err := jsonRaw(CommentPayload{TicketID: t.ID, Body: body})
	if err != nil {
		return nil, err
	}
	return &reply{
		content: proposeText(t, body),
		outcome: OutcomeActionProposed,
		cites:   Citations{Tickets: []TicketRef{s.ticketRef(t)}},
		propose: &Action{Kind: ActionTicketComment, Payload: payload, Summary: actionSummary(t, body)},
	}, nil
}

// mentions 报告提问中以工单号的形式出现了 id：前后不是数字，也不是版本号的一段（如 v1.12.0 中的 12）。
func mentions(q string, id int64) bool {
	n := strconv.FormatInt(id, 10)
	for i := 0; ; {
		j := strings.Index(q[i:], n)
		if j < 0 {
			return false
		}
		j += i
		k := j + len(n)
		before := j == 0 || (!isDigit(q[j-1]) && q[j-1] != '.')
		after := k == len(q) || (!isDigit(q[k]) && (q[k] != '.' || k+1 == len(q) || !isDigit(q[k+1])))
		if before && after {
			return true
		}
		i = k
	}
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// lastUniqueTicket 返回最近一条助手消息唯一引用的工单。
func lastUniqueTicket(h []HistoryItem) (int64, bool) {
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == RoleAssistant {
			if len(h[i].Tickets) == 1 {
				return h[i].Tickets[0].ID, true
			}
			return 0, false
		}
	}
	return 0, false
}
