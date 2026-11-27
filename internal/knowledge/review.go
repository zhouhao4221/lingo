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
	extractBatch       = 1 // 每次领取一张：租约只需覆盖一次模型调用
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
// modelOK 是该回答生成时的输入是否都在 ReplyMarked 内容范围内（REQ-012；nil 为未知，如升级前的回答），记入候选。
func (s *Service) Suggest(ctx context.Context, d Draft, messageID int64, modelOK *bool) error {
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
		Tokens: EntryTokens(d.Question, d.Answer), ModelOK: modelOK})
	if errors.Is(err, ticket.ErrConflict) {
		return nil // 已有该消息的候选
	}
	return err
}

// refRe 匹配回答中的工单号（#12）与知识编号（K12），入库前去掉。
var refRe = regexp.MustCompile(`#\d+|\bK\d+\b`)

// Miss 记录一次知识类问题没有命中（提问脱敏、截断，按项目与切词重合度归并）。
func (s *Service) Miss(ctx context.Context, p auth.Principal, project, question string) error {
	q := textutil.ClipEllipsis(Scrub(strings.Join(strings.Fields(question), " ")), maxMissRunes)
	toks := Tokens(q)
	if q == "" || len(toks) == 0 {
		return nil
	}
	// 按项目分别记录（只记提问人可访问的项目）：同一提问只对这些项目的 reviewer 可见，归并也不跨项目
	projects := p.Projects
	if project != "" && p.InProject(project) {
		projects = []string{project}
	}
	since := s.o.Now().Add(-MissRetention)
	for _, pr := range projects {
		if err := s.o.Store.RecordKnowledgeMiss(ctx, pr, q, toks, DuplicateRatio, since); err != nil {
			return err
		}
	}
	return nil
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
	// 每张工单只自动抽取一次：已有抽取条目（如手动重新抽取的、或上次入库后未能记下完成）时不再调用模型（REQ-012）
	if es, err := s.o.Store.TicketExtracts(ctx, id); err != nil {
		return "failed", err.Error()
	} else if len(es) > 0 {
		return "done", ""
	}
	if _, _, err := s.extractTicket(ctx, ex, t, cs, s.o.ModelMarked); err != nil && !errors.Is(err, ticket.ErrConflict) {
		return "failed", err.Error() // ErrConflict：其他实例已为该工单入库（每张工单只抽取一次）
	}
	return "done", ""
}

// extractInput 选出交给模型的评论：markedOnly 时只用 Comment.Model 的评论（REQ-012），否则用对外评论（升级前的行为）。
// trusted 报告选出的评论都属于 ReplyMarked 内容范围；excluded 是因不在范围内（或来源未知）而未选的对外评论数。
func extractInput(t *ticket.Ticket, cs []ticket.Comment, markedOnly bool) (in ExtractInput, trusted bool, excluded int) {
	in = ExtractInput{Type: t.Type, Title: t.Title, Body: t.Body, FixedVersion: t.FixedTag, Closed: t.Status == ticket.StatusClosed}
	trusted = true
	for _, c := range cs {
		if !c.Public || strings.TrimSpace(c.Body) == "" {
			continue
		}
		if !c.Model {
			if markedOnly {
				excluded++
				continue
			}
			trusted = false
		}
		in.Comments = append(in.Comments, c.Body)
	}
	return in, trusted, excluded
}

// extractTicket 调用模型抽取并以待审核状态入库，返回新条目 ID（模型认为没有可复用的知识或输出不可用时为 0）与送给模型的
// 评论数。条目的 ModelOK 记录送给模型的评论是否都在 ReplyMarked 内容范围内。同类候选已存在时返回 ticket.ErrConflict。
func (s *Service) extractTicket(ctx context.Context, ex Extractor, t *ticket.Ticket, cs []ticket.Comment, markedOnly bool) (int64, int, error) {
	in, trusted, _ := extractInput(t, cs, markedOnly)
	out, err := ex.ExtractKnowledge(ctx, in)
	if err != nil {
		return 0, len(in.Comments), err
	}
	if out == nil || out.Skip {
		return 0, len(in.Comments), nil
	}
	d, err := Prepare(Draft{ProjectKey: t.ProjectKey, Repo: t.Repo,
		Question: textutil.ClipEllipsis(out.Question, MaxQuestionRunes), Answer: textutil.ClipEllipsis(out.Answer, MaxAnswerRunes)})
	if err != nil {
		return 0, len(in.Comments), nil // 模型没给出可用内容：不入库，不重试
	}
	dup, err := s.DuplicateOf(ctx, d.ProjectKey, d.Question, d.Answer)
	if err != nil {
		return 0, len(in.Comments), err
	}
	e := NewEntry{ProjectKey: d.ProjectKey, Repo: d.Repo, Question: d.Question, Answer: d.Answer, Source: SourceTicketExtract,
		SourceTicketID: t.ID, Status: StatusPending, DuplicateOf: dup, Tokens: EntryTokens(d.Question, d.Answer), ModelOK: &trusted}
	id, err := s.o.Store.InsertKnowledge(ctx, e)
	if errors.Is(err, ticket.ErrInvalid) { // 仓库已不在项目中：不带仓库再试一次
		e.Repo = ""
		id, err = s.o.Store.InsertKnowledge(ctx, e)
	}
	return id, len(in.Comments), err
}

// 重新抽取的结果（ReextractResult.Outcome）。
const (
	ReextractCreated = "created" // 按新规则入库了一条待审核候选
	ReextractExists  = "exists"  // 该工单已有来源可信的抽取条目（返回它，不调用模型）
	ReextractSkipped = "skipped" // 模型认为没有可复用的知识（或输出不可用），未入库
)

// ReextractResult 是 Reextract 的结果。
type ReextractResult struct {
	Outcome  string
	Entry    *Entry // created / exists 时为该条目
	Comments int    // 送给模型的评论数
	Excluded int    // 未送给模型的对外评论数：不在 ReplyMarked 内容范围，或来源未知（如升级前同步、尚未重新同步）
}

// ErrNoExtractor 表示没有可用的抽取模型。
var ErrNoExtractor = errors.New("knowledge: no extraction model")

// reextractWait 是重新抽取等待同一工单上进行中的抽取（另一次重新抽取或自动抽取）的最长时间。
var reextractWait = 60 * time.Second

// Reextract 按新规则（只用 Comment.Model 的评论）为工单重新抽取一条待审核候选（reviewer / admin，REQ-012 方案 A）。
// 只适用于已有来源未知或不可信的抽取条目、且仍为已结案的 Bug / 需求工单（否则 ticket.ErrConflict）；旧条目原样保留
// （内容、状态与审核结果都不改），新候选走正常审核。已有来源可信的抽取条目时直接返回它（Outcome=exists，不调用模型）。
// 只由调用方手动触发，不会自动执行；每次调用（exists 除外）调用一次抽取模型。同一工单上的重新抽取与自动抽取经同一租约
// 串行：同时调用时后到的等待先到的完成后返回 exists（不重复调用模型），等待超过 reextractWait 返回 ticket.ErrConflict。
// 工单不存在或不在调用者项目内返回 ticket.ErrNotFound；没有抽取模型返回 ErrNoExtractor。
func (s *Service) Reextract(ctx context.Context, p auth.Principal, ticketID int64) (ReextractResult, error) {
	var res ReextractResult
	if !p.Staff() {
		return res, fmt.Errorf("%w: knowledge maintenance requires reviewer or admin", ticket.ErrForbidden)
	}
	if ticketID <= 0 {
		return res, fmt.Errorf("%w: ticket id is required", ticket.ErrInvalid)
	}
	if s.o.Tickets == nil {
		return res, ErrNoExtractor
	}
	t, cs, err := s.o.Tickets(ctx, ticketID)
	if err != nil {
		return res, err
	}
	if !p.InProject(t.ProjectKey) {
		return res, ticket.ErrNotFound
	}
	if r, done, err := s.reextractable(ctx, ticketID); done || err != nil {
		return r, err
	}
	switch t.Status {
	case ticket.StatusReleased, ticket.StatusDeployed, ticket.StatusClosed:
	default:
		return res, fmt.Errorf("%w: ticket is not closed", ticket.ErrConflict)
	}
	if t.Type != "bug" && t.Type != "feature" {
		return res, fmt.Errorf("%w: only bug and feature tickets are extracted", ticket.ErrConflict)
	}
	ex := s.o.Extractor()
	if ex == nil {
		return res, ErrNoExtractor
	}
	// 租约放在所有前置检查之后：取租约时可能为没有抽取登记的工单登记为已完成
	until, err := s.leaseExtraction(ctx, ticketID)
	if err != nil {
		return res, err
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = s.o.Store.ReleaseKnowledgeExtraction(rctx, ticketID, until)
	}()
	// 取得租约后重新判断：等待期间另一次抽取可能已入库
	if r, done, err := s.reextractable(ctx, ticketID); done || err != nil {
		return r, err
	}
	_, _, res.Excluded = extractInput(t, cs, true)
	id, n, err := s.extractTicket(ctx, ex, t, cs, true)
	res.Comments = n
	if errors.Is(err, ticket.ErrConflict) { // 同时有另一次重新抽取已入库
		trusted, _, err := s.ticketExtracts(ctx, ticketID)
		if err != nil {
			return res, err
		}
		res.Outcome, res.Entry = ReextractExists, trusted
		return res, nil
	}
	if err != nil {
		return res, err
	}
	if id == 0 {
		res.Outcome = ReextractSkipped
		return res, nil
	}
	if res.Entry, err = s.o.Store.GetKnowledge(ctx, id); err != nil {
		return res, err
	}
	res.Outcome = ReextractCreated
	return res, nil
}

// reextractable 判断工单能否重新抽取：已有可信抽取条目时 done 为真并返回 exists；没有来源未知或不可信的抽取条目时返回
// ticket.ErrConflict。
func (s *Service) reextractable(ctx context.Context, ticketID int64) (ReextractResult, bool, error) {
	trusted, unknown, err := s.ticketExtracts(ctx, ticketID)
	switch {
	case err != nil:
		return ReextractResult{}, true, err
	case trusted != nil:
		return ReextractResult{Outcome: ReextractExists, Entry: trusted}, true, nil
	case !unknown:
		return ReextractResult{}, true, fmt.Errorf("%w: ticket has no extracted knowledge of unknown provenance", ticket.ErrConflict)
	}
	return ReextractResult{}, false, nil
}

// leaseExtraction 取得工单抽取的租约，被占用时每 200ms 重试，最多等 reextractWait。
func (s *Service) leaseExtraction(ctx context.Context, ticketID int64) (time.Time, error) {
	deadline := time.Now().Add(reextractWait)
	for {
		until, ok, err := s.o.Store.LeaseKnowledgeExtraction(ctx, ticketID, extractLease)
		if err != nil || ok {
			return until, err
		}
		if time.Now().After(deadline) {
			return time.Time{}, fmt.Errorf("%w: knowledge extraction for this ticket is in progress, retry later", ticket.ErrConflict)
		}
		select {
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ticketExtracts 返回工单来源可信的抽取条目（没有时为 nil），并报告是否有来源未知或不可信的抽取条目。
func (s *Service) ticketExtracts(ctx context.Context, ticketID int64) (*Entry, bool, error) {
	es, err := s.o.Store.TicketExtracts(ctx, ticketID)
	if err != nil {
		return nil, false, err
	}
	var trusted *Entry
	unknown := false
	for _, e := range es {
		if e.ModelInput {
			trusted = e
		} else {
			unknown = true
		}
	}
	return trusted, unknown, nil
}

// NewFromComment 构造后台对外评论时入库的条目（已 Prepare，直接生效）。
func NewFromComment(d Draft, ticketID int64, by string) NewEntry {
	return NewEntry{ProjectKey: d.ProjectKey, Repo: d.Repo, Question: d.Question, Answer: d.Answer, Source: SourceTicketComment,
		SourceTicketID: ticketID, Tokens: EntryTokens(d.Question, d.Answer), By: by}
}
