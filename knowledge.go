package lingo

import (
	"context"
	"errors"
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

	KnowledgeSourceManual       = knowledge.SourceManual       // reviewer / admin 手动新建
	KnowledgeSourceTicketAnswer = knowledge.SourceTicketAnswer // 后台直接答复工单时勾选入库
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
		Status: e.Status, CiteCount: e.CiteCount, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
	if full {
		out.SourceTicketID, out.CreatedBy, out.UpdatedBy = e.SourceTicketID, e.CreatedBy, e.UpdatedBy
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
			return nil, err
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
		d := knowledge.Draft{ProjectKey: t.ProjectKey, Repo: kd.Repo, Question: kd.Question, Answer: kd.Answer}
		if d.Question == "" {
			d.Question = textutil.ClipEllipsis(oneLineText(t.Title+"："+t.Body), knowledge.MaxQuestionRunes)
		}
		if d.Answer == "" {
			d.Answer = textutil.ClipEllipsis(answer, knowledge.MaxAnswerRunes)
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

// embedKnowledge 是 Run 中的向量生成循环：每 30 秒（或条目新建、编辑后）为待生成向量的条目生成向量，直到没有待处理的。
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
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-l.embedKick:
		}
	}
}
