package knowledge

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/textutil"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// 抽取任务参数。
const (
	extractBatch       = 4
	extractLease       = 5 * time.Minute
	extractMaxAttempts = 3
	extractRetry       = 10 * time.Minute
	maxMissRunes       = 500
)

// Approve 审核通过待审核条目（reviewer / admin）；d 非空时以编辑后的内容通过。
func (s *Service) Approve(ctx context.Context, p auth.Principal, id int64, d *Draft) (*Entry, error) {
	e, err := s.get(ctx, p, id)
	if err != nil {
		return nil, err
	}
	var toks []string
	if d != nil {
		nd := *d
		nd.ProjectKey = e.ProjectKey
		if nd, err = Prepare(nd); err != nil {
			return nil, err
		}
		d, toks = &nd, EntryTokens(nd.Question, nd.Answer)
	}
	if err := s.o.Store.TransitionKnowledge(ctx, id, StatusPending, StatusActive, d, toks, p.ID); err != nil {
		return nil, err
	}
	return s.o.Store.GetKnowledge(ctx, id)
}

// Reject 驳回待审核条目（reviewer / admin），驳回的不参与检索。
func (s *Service) Reject(ctx context.Context, p auth.Principal, id int64) (*Entry, error) {
	if _, err := s.get(ctx, p, id); err != nil {
		return nil, err
	}
	if err := s.o.Store.TransitionKnowledge(ctx, id, StatusPending, StatusRejected, nil, nil, p.ID); err != nil {
		return nil, err
	}
	return s.o.Store.GetKnowledge(ctx, id)
}

// DuplicateOf 返回项目内与内容可能重复的已生效条目（重合比例不低于 DuplicateRatio），没有时为 0。
func (s *Service) DuplicateOf(ctx context.Context, project, question, answer string) (int64, error) {
	toks := EntryTokens(question, answer)
	if len(toks) == 0 {
		return 0, nil
	}
	id, ratio, err := s.o.Store.SimilarKnowledge(ctx, project, toks)
	if err != nil || ratio < DuplicateRatio {
		return 0, err
	}
	return id, nil
}

// Suggest 以待审核状态入库一条候选（来源 d：知识类回答被标为"有帮助"）；同一消息只入库一次（重复时忽略）。
func (s *Service) Suggest(ctx context.Context, d Draft, messageID int64) error {
	d.Answer = refRe.ReplaceAllString(d.Answer, "")
	d.Question = textutil.ClipEllipsis(strings.TrimSpace(d.Question), MaxQuestionRunes)
	d.Answer = textutil.ClipEllipsis(strings.TrimSpace(d.Answer), MaxAnswerRunes)
	d, err := Prepare(d)
	if err != nil {
		return nil // 内容不可用（如为空）：不入库
	}
	dup, err := s.DuplicateOf(ctx, d.ProjectKey, d.Question, d.Answer)
	if err != nil {
		return err
	}
	_, err = s.o.Store.InsertKnowledge(ctx, NewEntry{ProjectKey: d.ProjectKey, Question: d.Question, Answer: d.Answer,
		Source: SourceAssistantFeedback, SourceMessageID: messageID, Status: StatusPending, DuplicateOf: dup,
		Tokens: EntryTokens(d.Question, d.Answer)})
	if errors.Is(err, ticket.ErrConflict) {
		return nil // 已有该消息的候选
	}
	return err
}

// refRe 匹配回答中的工单号（#12）与知识编号（K12），入库前去掉。
var refRe = regexp.MustCompile(`#\d+|\bK\d+\b`)

// Feedback 给回答引用的条目累加有帮助 / 没帮助次数。
func (s *Service) Feedback(ctx context.Context, ids []int64, helpful, unhelpful int) error {
	if len(ids) == 0 || (helpful == 0 && unhelpful == 0) {
		return nil
	}
	return s.o.Store.AddKnowledgeFeedback(ctx, ids, helpful, unhelpful)
}

// Miss 记录一次知识类问题没有命中（提问脱敏、截断，按项目与切词重合度归并）。
func (s *Service) Miss(ctx context.Context, p auth.Principal, project, question string) error {
	q := textutil.ClipEllipsis(Scrub(strings.Join(strings.Fields(question), " ")), maxMissRunes)
	toks := Tokens(q)
	if q == "" || len(toks) == 0 {
		return nil
	}
	projects := p.Projects
	if project != "" {
		projects = []string{project}
	}
	return s.o.Store.RecordKnowledgeMiss(ctx, projects, q, toks, DuplicateRatio, s.o.Now().Add(-MissRetention))
}

// ListMisses 列出调用者项目内近 90 天未答上的提问（reviewer / admin），次数多的在前。
func (s *Service) ListMisses(ctx context.Context, p auth.Principal, q MissQuery) ([]Miss, error) {
	if !p.Staff() {
		return nil, fmt.Errorf("%w: knowledge maintenance requires reviewer or admin", ticket.ErrForbidden)
	}
	if q.Project != "" && !p.InProject(q.Project) {
		return nil, nil
	}
	if floor := s.o.Now().Add(-MissRetention); q.Since.Before(floor) {
		q.Since = floor
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	q.Projects, q.Limit = p.Projects, ticket.Limit(q.Limit, ticket.DefaultLimit)
	return s.o.Store.ListKnowledgeMisses(ctx, q)
}

// PurgeMisses 清理超过保留期的未答上记录。
func (s *Service) PurgeMisses(ctx context.Context) (int, error) {
	return s.o.Store.PurgeKnowledgeMisses(ctx, s.o.Now().Add(-MissRetention))
}

// ExtractPending 为一批新结案的 Bug / 需求工单抽取知识（待审核），返回处理的工单数；抽取模型不可用时不领取。
func (s *Service) ExtractPending(ctx context.Context) (int, error) {
	ex := s.o.Extractor()
	if ex == nil || s.o.Tickets == nil {
		return 0, nil
	}
	ids, err := s.o.Store.ClaimKnowledgeExtractions(ctx, extractBatch, extractLease)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	for _, id := range ids {
		status, msg := s.extractOne(ctx, ex, id)
		if err := s.o.Store.FinishKnowledgeExtraction(ctx, id, status, msg, extractMaxAttempts, s.o.Now().Add(extractRetry)); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// extractOne 抽取一张工单，返回结束状态（done / skipped / failed）与失败原因。
func (s *Service) extractOne(ctx context.Context, ex Extractor, id int64) (string, string) {
	t, cs, err := s.o.Tickets(ctx, id)
	if errors.Is(err, ticket.ErrNotFound) {
		return "skipped", ""
	}
	if err != nil {
		return "failed", err.Error()
	}
	in := ExtractInput{Type: t.Type, Title: t.Title, Body: t.Body, FixedVersion: t.FixedTag, Closed: t.Status == ticket.StatusClosed}
	for _, c := range cs {
		if c.Public && strings.TrimSpace(c.Body) != "" {
			in.Comments = append(in.Comments, c.Body)
		}
	}
	out, err := ex.ExtractKnowledge(ctx, in)
	if err != nil {
		return "failed", err.Error()
	}
	if out == nil || out.Skip {
		return "done", ""
	}
	d, err := Prepare(Draft{ProjectKey: t.ProjectKey, Repo: t.Repo,
		Question: textutil.ClipEllipsis(out.Question, MaxQuestionRunes), Answer: textutil.ClipEllipsis(out.Answer, MaxAnswerRunes)})
	if err != nil {
		return "done", "" // 模型没给出可用内容：不入库，不重试
	}
	dup, err := s.DuplicateOf(ctx, d.ProjectKey, d.Question, d.Answer)
	if err != nil {
		return "failed", err.Error()
	}
	if _, err := s.o.Store.InsertKnowledge(ctx, NewEntry{ProjectKey: d.ProjectKey, Repo: d.Repo, Question: d.Question, Answer: d.Answer,
		Source: SourceTicketExtract, SourceTicketID: t.ID, Status: StatusPending, DuplicateOf: dup,
		Tokens: EntryTokens(d.Question, d.Answer)}); err != nil {
		if errors.Is(err, ticket.ErrInvalid) { // 仓库已不在项目中：不带仓库再试一次
			d.Repo = ""
			_, err = s.o.Store.InsertKnowledge(ctx, NewEntry{ProjectKey: d.ProjectKey, Question: d.Question, Answer: d.Answer,
				Source: SourceTicketExtract, SourceTicketID: t.ID, Status: StatusPending, DuplicateOf: dup,
				Tokens: EntryTokens(d.Question, d.Answer)})
		}
		if err != nil {
			return "failed", err.Error()
		}
	}
	return "done", ""
}

// NewFromComment 构造后台对外评论时入库的条目（已 Prepare，直接生效）。
func NewFromComment(d Draft, ticketID int64, by string) NewEntry {
	return NewEntry{ProjectKey: d.ProjectKey, Repo: d.Repo, Question: d.Question, Answer: d.Answer, Source: SourceTicketComment,
		SourceTicketID: ticketID, Tokens: EntryTokens(d.Question, d.Answer), By: by}
}
