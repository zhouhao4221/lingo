package lingo

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/knowledge"
	"github.com/zhouhao4221/lingo/internal/llm"
	"github.com/zhouhao4221/lingo/internal/store"
	"github.com/zhouhao4221/lingo/internal/textutil"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// 知识条目状态与来源（REQ-006）。
const (
	KnowledgeActive   = knowledge.StatusActive
	KnowledgeDisabled = knowledge.StatusDisabled
	KnowledgePending  = knowledge.StatusPending  // 待审核（REQ-007）
	KnowledgeRejected = knowledge.StatusRejected // 审核驳回

	KnowledgeSourceManual            = knowledge.SourceManual            // reviewer / admin 手动新建
	KnowledgeSourceTicketAnswer      = knowledge.SourceTicketAnswer      // 后台直接答复工单时勾选入库
	KnowledgeSourceTicketExtract     = knowledge.SourceTicketExtract     // 结案工单由模型抽取（待审核）
	KnowledgeSourceAssistantFeedback = knowledge.SourceAssistantFeedback // 知识类回答被标为"有帮助"（待审核）
	KnowledgeSourceTicketComment     = knowledge.SourceTicketComment     // 后台对外评论时勾选入库
)

// KnowledgeEntry 是一条知识。SourceTicketID 只在调用者能查看来源工单时填写；CreatedBy / UpdatedBy 只在维护接口返回。
type KnowledgeEntry struct {
	ID             int64     `json:"id"`
	ProjectKey     string    `json:"project"`
	Repo           string    `json:"repo,omitempty"`
	Question       string    `json:"question"`
	Answer         string    `json:"answer"`
	Source         string    `json:"source"`
	SourceTicketID int64     `json:"source_ticket_id,omitempty"`
	Status         string    `json:"status"`
	CiteCount      int64     `json:"cite_count"`
	CreatedBy      string    `json:"created_by,omitempty"`
	UpdatedBy      string    `json:"updated_by,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Score          float64   `json:"score,omitempty"` // 检索得分（只在检索结果中）
	// 以下只在维护接口返回：可能重复的已生效条目（入库时检出）、引用该条目的回答被标为有帮助 / 没帮助的次数
	DuplicateOf    int64 `json:"duplicate_of,omitempty"`
	HelpfulCount   int64 `json:"helpful_count,omitempty"`
	UnhelpfulCount int64 `json:"unhelpful_count,omitempty"`
	// ModelInput 报告条目来源可信、可作为模型输入（REQ-012）：人工在灵工中填写的（manual、ticket_answer、ticket_comment），
	// 或按可信内容生成的派生条目。为 false 的（升级前生成、来源未知的抽取与回答候选，或来源含 ReplyMarked 范围外内容的）
	// 在启用 ModelContentMarked 时不参与问答检索与向量生成，但照常展示、审核与维护。
	ModelInput bool `json:"model_input"`
}

// KnowledgeReextract 是 ReextractTicketKnowledge 的结果。
type KnowledgeReextract struct {
	TicketID int64  `json:"ticket_id"`
	Outcome  string `json:"outcome"` // KnowledgeReextractCreated | KnowledgeReextractExists | KnowledgeReextractSkipped
	// Entry 是新入库的待审核候选（created），或该工单已有的来源可信的抽取条目（exists）
	Entry *KnowledgeEntry `json:"entry,omitempty"`
	// Comments 是送给抽取模型的评论数；Excluded 是未送给模型的对外评论数（不在 ReplyMarked 内容范围，或来源未知——
	// 升级前同步的 Gitea 评论需先经 ResyncTicketComments 重新判定）
	Comments int `json:"comments"`
	Excluded int `json:"excluded"`
}

// 重新抽取的结果（KnowledgeReextract.Outcome）。
const (
	KnowledgeReextractCreated = knowledge.ReextractCreated // 入库了一条新的待审核候选
	KnowledgeReextractExists  = knowledge.ReextractExists  // 已有来源可信的抽取条目，未调用模型
	KnowledgeReextractSkipped = knowledge.ReextractSkipped // 模型认为没有可复用的知识，未入库
)

// UnansweredQuestion 是知识类问题没有命中的提问（脱敏），近 90 天内同类提问归并计数。
type UnansweredQuestion struct {
	ID       int64     `json:"id"`
	Question string    `json:"question"`
	Hits     int       `json:"hits"`
	FirstAt  time.Time `json:"first_at"`
	LastAt   time.Time `json:"last_at"`
}

// UnansweredFilter 是 ListUnansweredQuestions 的条件。
type UnansweredFilter struct {
	Project string
	Since   time.Time // 零值或早于 90 天前时为 90 天前
	Offset  int
	Limit   int // 0 时默认 50，最多 200
}

// KnowledgeDraft 是新建或编辑知识条目的内容。问题最多 500 字、答案最多 10000 字；邮箱与手机号入库前替换为占位。
type KnowledgeDraft struct {
	Project  string `json:"project,omitempty"` // 新建时必填；编辑与答复入库时忽略
	Repo     string `json:"repo,omitempty"`    // 可选，"owner/name"，须属于该项目
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// KnowledgeFilter 是 ListKnowledge 的条件。
type KnowledgeFilter struct {
	Project string // 为空时为 User.Projects 内全部
	Status  string // KnowledgeActive | KnowledgeDisabled，为空表示全部
	Sort    string // "updated"（默认，最近更新在前）| "cites"（引用次数多的在前）
	Offset  int
	Limit   int // 0 时默认 50，最多 200
}

// TicketAnswer 是 AnswerTicketWith 的输入：Knowledge 非 nil 时把这次答复加入知识库（直接生效）。Knowledge 的问题与答案
// 为空时分别取工单标题与正文、答复原文；建议改写为通用的问题与答案，去掉只针对提交人的内容。
type TicketAnswer struct {
	Answer    string
	Knowledge *KnowledgeDraft
}

// EmbeddingModelNamer 是 Model 的可选接口：返回向量模型名（未配置向量模型时为空）。知识库用它区分不同模型生成的向量，
// 换模型后旧向量不参与排序并由后台重新生成；未实现时以向量维度作为标识。内置的 OpenAI / DeepSeek 适配器已实现。
type EmbeddingModelNamer interface {
	EmbeddingModelName() string
}

func publicKnowledge(e *knowledge.Entry, full bool) KnowledgeEntry {
	out := KnowledgeEntry{ID: e.ID, ProjectKey: e.ProjectKey, Repo: e.Repo, Question: e.Question, Answer: e.Answer, Source: e.Source,
		Status: e.Status, CiteCount: e.CiteCount, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt, ModelInput: e.ModelInput}
	if full {
		out.SourceTicketID, out.CreatedBy, out.UpdatedBy = e.SourceTicketID, e.CreatedBy, e.UpdatedBy
		out.DuplicateOf, out.HelpfulCount, out.UnhelpfulCount = e.DuplicateOf, e.HelpfulCount, e.UnhelpfulCount
	}
	return out
}

func (l *Lingo) knowledgeResult(e *knowledge.Entry, err error) (KnowledgeEntry, error) {
	if err != nil {
		return KnowledgeEntry{}, publicErr(err)
	}
	l.kickEmbed()
	return publicKnowledge(e, true), nil
}

// SearchKnowledge 在 u 可访问的项目（project 非空时只含该项目）中检索已生效的知识条目，按相关度排序，最多 limit 条
// （0 时为 5，最多 20）。所有角色可用；来源工单只在 u 能查看时返回。
func (l *Lingo) SearchKnowledge(ctx context.Context, u User, query, project string, limit int) ([]KnowledgeEntry, error) {
	p := l.viewer(ctx, u)
	hits, err := l.knowledge.Search(ctx, p, project, query, nil, limit)
	if err != nil {
		return nil, publicErr(err)
	}
	var srcs []int64
	for _, h := range hits {
		if h.SourceTicketID > 0 {
			srcs = append(srcs, h.SourceTicketID)
		}
	}
	readable := map[int64]bool{}
	if len(srcs) > 0 {
		ts, err := l.store.GetMany(ctx, srcs)
		if err != nil {
			return nil, publicErr(err)
		}
		for _, t := range ts {
			readable[t.ID] = ticket.Readable(p, t)
		}
	}
	out := make([]KnowledgeEntry, 0, len(hits))
	for _, h := range hits {
		e := publicKnowledge(h.Entry, false)
		e.Score = h.Score
		if readable[h.SourceTicketID] {
			e.SourceTicketID = h.SourceTicketID
		}
		out = append(out, e)
	}
	return out, nil
}

// ReextractTicketKnowledge 按新规则为工单重新抽取一条待审核的知识候选（仅 reviewer / admin，REQ-012）：只把 ReplyMarked
// 内容范围内的评论（同 ModelContentMarked）交给抽取模型，新候选记为来源可信、走正常审核（ApproveKnowledge /
// RejectKnowledge）。该工单原有的来源未知或不可信的抽取条目原样保留（内容、状态与审核结果都不改，也不参与模型输入）。
//
// 只适用于已有这样的抽取条目、且仍为已结案（released / deployed / closed）的 Bug / 需求工单，否则 ErrConflict；
// 已有来源可信的抽取条目时直接返回它（KnowledgeReextractExists，不调用模型）。工单不存在或不在 u.Projects 内返回
// ErrNotFound；没有可用的抽取模型（LightModel / Model）返回 ErrNoModel。灵工不会自动调用：每次调用（exists 除外）
// 调用一次抽取模型并计入用量。升级前同步的 Gitea 评论来源未知、不会交给模型，需要时先调用 ResyncTicketComments。
func (l *Lingo) ReextractTicketKnowledge(ctx context.Context, u User, ticketID int64) (KnowledgeReextract, error) {
	r, err := l.knowledge.Reextract(ctx, l.principal(u), ticketID)
	if errors.Is(err, knowledge.ErrNoExtractor) {
		return KnowledgeReextract{}, ErrNoModel
	}
	if err != nil {
		return KnowledgeReextract{}, publicErr(err)
	}
	out := KnowledgeReextract{TicketID: ticketID, Outcome: r.Outcome, Comments: r.Comments, Excluded: r.Excluded}
	if r.Entry != nil {
		e := publicKnowledge(r.Entry, true)
		out.Entry = &e
	}
	return out, nil
}

// ListKnowledge 列出 u 项目内的知识条目（仅 reviewer / admin），含已停用的。
func (l *Lingo) ListKnowledge(ctx context.Context, u User, f KnowledgeFilter) ([]KnowledgeEntry, error) {
	es, err := l.knowledge.List(ctx, l.principal(u), knowledge.ListQuery{Project: f.Project, Status: f.Status, Sort: f.Sort,
		Offset: f.Offset, Limit: f.Limit})
	if err != nil {
		return nil, publicErr(err)
	}
	out := make([]KnowledgeEntry, 0, len(es))
	for _, e := range es {
		out = append(out, publicKnowledge(e, true))
	}
	return out, nil
}

// CreateKnowledge 手动新建知识条目（仅 reviewer / admin，项目须在 u.Projects 内），直接生效。
func (l *Lingo) CreateKnowledge(ctx context.Context, u User, d KnowledgeDraft) (KnowledgeEntry, error) {
	if err := l.ensureProjects(ctx); err != nil {
		return KnowledgeEntry{}, err
	}
	return l.knowledgeResult(l.knowledge.Create(ctx, l.principal(u), knowledge.Draft{ProjectKey: d.Project, Repo: d.Repo,
		Question: d.Question, Answer: d.Answer}))
}

// UpdateKnowledge 编辑知识条目的问题、答案与仓库（仅 reviewer / admin）；向量由后台重新生成。
func (l *Lingo) UpdateKnowledge(ctx context.Context, u User, id int64, d KnowledgeDraft) (KnowledgeEntry, error) {
	return l.knowledgeResult(l.knowledge.Update(ctx, l.principal(u), id, knowledge.Draft{Repo: d.Repo, Question: d.Question, Answer: d.Answer}))
}

// DisableKnowledge 停用知识条目（仅 reviewer / admin），停用后不再参与检索与回答，可恢复。
func (l *Lingo) DisableKnowledge(ctx context.Context, u User, id int64) (KnowledgeEntry, error) {
	return l.knowledgeResult(l.knowledge.SetStatus(ctx, l.principal(u), id, false))
}

// EnableKnowledge 恢复已停用的知识条目（仅 reviewer / admin）。
func (l *Lingo) EnableKnowledge(ctx context.Context, u User, id int64) (KnowledgeEntry, error) {
	return l.knowledgeResult(l.knowledge.SetStatus(ctx, l.principal(u), id, true))
}

// ApproveKnowledge 审核通过待审核的知识条目（仅 reviewer / admin）；d 非空时以编辑后的问题、答案与仓库通过。
// 条目不是待审核状态时返回 ErrConflict。
func (l *Lingo) ApproveKnowledge(ctx context.Context, u User, id int64, d *KnowledgeDraft) (KnowledgeEntry, error) {
	var kd *knowledge.Draft
	if d != nil {
		kd = &knowledge.Draft{Repo: d.Repo, Question: d.Question, Answer: d.Answer}
	}
	return l.knowledgeResult(l.knowledge.Approve(ctx, l.principal(u), id, kd))
}

// RejectKnowledge 驳回待审核的知识条目（仅 reviewer / admin），驳回的不参与检索；不是待审核状态时返回 ErrConflict。
func (l *Lingo) RejectKnowledge(ctx context.Context, u User, id int64) (KnowledgeEntry, error) {
	return l.knowledgeResult(l.knowledge.Reject(ctx, l.principal(u), id))
}

// ListUnansweredQuestions 列出 u 项目内近 90 天知识类问题没有命中的提问（仅 reviewer / admin），次数多的在前。
func (l *Lingo) ListUnansweredQuestions(ctx context.Context, u User, f UnansweredFilter) ([]UnansweredQuestion, error) {
	ms, err := l.knowledge.ListMisses(ctx, l.principal(u), knowledge.MissQuery{Project: f.Project, Since: f.Since, Offset: f.Offset,
		Limit: f.Limit})
	if err != nil {
		return nil, publicErr(err)
	}
	out := make([]UnansweredQuestion, 0, len(ms))
	for _, m := range ms {
		out = append(out, UnansweredQuestion{ID: m.ID, Question: m.Question, Hits: m.Hits, FirstAt: m.FirstAt, LastAt: m.LastAt})
	}
	return out, nil
}

// DeleteKnowledge 删除知识条目（仅 reviewer / admin）。
func (l *Lingo) DeleteKnowledge(ctx context.Context, u User, id int64) error {
	return publicErr(l.knowledge.Delete(ctx, l.principal(u), id))
}

// DisableKnowledgeByTicket 停用 u 项目内来源为工单 ticketID 的知识条目（仅 reviewer / admin），返回停用条数；
// 用于提交人要求删除个人数据等情形。
func (l *Lingo) DisableKnowledgeByTicket(ctx context.Context, u User, ticketID int64) (int, error) {
	n, err := l.knowledge.DisableByTicket(ctx, l.principal(u), ticketID)
	return n, publicErr(err)
}

// AnswerTicketWith 同 AnswerTicket，另可在同一事务内把答复加入知识库（a.Knowledge 非 nil 时，直接生效，见 TicketAnswer）。
// 知识内容不合法（为空或超长）时返回 ErrInvalidInput，答复也不执行。
func (l *Lingo) AnswerTicketWith(ctx context.Context, u User, id int64, a TicketAnswer) (Ticket, error) {
	if a.Knowledge == nil {
		return l.AnswerTicket(ctx, u, id, a.Answer)
	}
	p := l.principal(u)
	kd := *a.Knowledge
	t, err := l.tickets.AnswerWith(ctx, p, id, a.Answer, func(ctx context.Context, tx ticket.Tx, t *ticket.Ticket, answer string) error {
		w, ok := tx.(knowledge.Writer)
		if !ok {
			return errors.New("lingo: store does not support knowledge")
		}
		d := knowledge.Draft{ProjectKey: t.ProjectKey, Repo: kd.Repo, Question: strings.TrimSpace(kd.Question), Answer: strings.TrimSpace(kd.Answer)}
		// 缺省值先脱敏再截断：截断点落在邮箱、手机号中间时也能清理掉
		if d.Question == "" {
			d.Question = textutil.ClipEllipsis(knowledge.Scrub(oneLineText(t.Title+"："+t.Body)), knowledge.MaxQuestionRunes)
		}
		if d.Answer == "" {
			d.Answer = textutil.ClipEllipsis(knowledge.Scrub(answer), knowledge.MaxAnswerRunes)
		}
		d, err := knowledge.Prepare(d)
		if err != nil {
			return err
		}
		_, err = w.InsertKnowledge(ctx, knowledge.NewFromAnswer(d, t.ID, p.ID))
		return err
	})
	if err == nil {
		l.kickEmbed()
	}
	return l.ticketResult(u, t, err)
}

func oneLineText(s string) string {
	out := make([]rune, 0, len(s))
	space := false
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			if !space && len(out) > 0 {
				out = append(out, ' ')
			}
			space = true
			continue
		}
		space = false
		out = append(out, r)
	}
	return string(out)
}

// knowledgeEmbedder 是当前主模型的向量接口；向量模型名为空（内置适配器未配置向量模型）时为 nil。
type knowledgeEmbedder struct {
	l *Lingo
	m Model
}

func (e knowledgeEmbedder) Name() string {
	if n, ok := e.m.(EmbeddingModelNamer); ok {
		return n.EmbeddingModelName()
	}
	return ""
}

func (e knowledgeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	start := time.Now()
	vs, err := e.m.Embed(ctx, texts)
	if !errors.Is(err, llm.ErrNoEmbedding) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if rerr := e.l.store.RecordLLMUsage(rctx, store.LLMUsage{Kind: "knowledge_embed", Model: e.Name(), OK: err == nil,
			Latency: time.Since(start)}); rerr != nil {
			e.l.log.Warn("lingo: record llm usage failed", "err", rerr)
		}
	}
	return vs, err
}

func (l *Lingo) knowledgeEmbedder() knowledge.Embedder {
	m := l.mainModel()
	if isNil(m) {
		return nil
	}
	if n, ok := m.(EmbeddingModelNamer); ok && n.EmbeddingModelName() == "" {
		return nil // 内置适配器未配置向量模型
	}
	return knowledgeEmbedder{l: l, m: m}
}

// kickEmbed 提醒 Run 尽快为新建或编辑的条目生成向量。
func (l *Lingo) kickEmbed() {
	select {
	case l.embedKick <- struct{}{}:
	default:
	}
}

// knowledgeExtractor 是当前的抽取模型（LightModel，未配置时为 Model）；都没有时为 nil（不抽取）。
func (l *Lingo) knowledgeExtractor() knowledge.Extractor {
	c := l.conn()
	if c == nil {
		return nil
	}
	m := c.light
	if isNil(m) {
		m = c.main
	}
	if isNil(m) {
		return nil
	}
	return &llm.Extractor{Chat: chatOf(m), Record: func(ctx context.Context, u llm.Usage, ok bool) {
		l.recordUsage(ctx, llm.KindKnowledgeExtract, u, ok, 0)
	}}
}

// knowledgeTicket 读取工单与对外评论（不鉴权，供抽取使用）。
func (l *Lingo) knowledgeTicket(ctx context.Context, id int64) (*ticket.Ticket, []ticket.Comment, error) {
	t, err := l.store.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	cs, err := l.store.Comments(ctx, id, true)
	return t, cs, err
}

// embedKnowledge 是 Run 中的知识库后台循环：每 30 秒（或条目新建、编辑后）为待生成向量的条目生成向量，为新结案的
// 工单抽取知识（REQ-007），并清理超过保留期的未答上记录。
func (l *Lingo) embedKnowledge(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		for i := 0; i < 20 && ctx.Err() == nil; i++ { // 每轮最多 20 批，避免长时间占用
			n, err := l.knowledge.EmbedPending(ctx)
			if err != nil {
				if !errors.Is(err, llm.ErrNoEmbedding) && ctx.Err() == nil {
					l.log.Warn("lingo: embed knowledge failed", "err", err)
				}
				break
			}
			if n == 0 {
				break
			}
		}
		// 向量在前：新建、编辑的条目尽快可用；抽取每轮最多 5 张（每次领取一张），避免长时间占用
		for i := 0; i < 5 && ctx.Err() == nil; i++ {
			n, err := l.knowledge.ExtractPending(ctx)
			if err != nil {
				if ctx.Err() == nil {
					l.log.Warn("lingo: extract knowledge failed", "err", err)
				}
				break
			}
			if n == 0 {
				break
			}
		}
		if _, err := l.knowledge.PurgeMisses(ctx); err != nil && ctx.Err() == nil {
			l.log.Warn("lingo: purge unanswered questions failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-l.embedKick:
		}
	}
}
