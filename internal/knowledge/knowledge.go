package knowledge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// 条目状态与来源。
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
	StatusPending  = "pending"  // 待审核（REQ-007：模型抽取、用户认可的回答）
	StatusRejected = "rejected" // 审核驳回

	SourceManual            = "manual"             // reviewer / admin 手动新建
	SourceTicketAnswer      = "ticket_answer"      // 后台直接答复工单时勾选入库
	SourceTicketExtract     = "ticket_extract"     // 结案工单由模型抽取（待审核）
	SourceAssistantFeedback = "assistant_feedback" // 知识类回答被标为"有帮助"（待审核）
	SourceTicketComment     = "ticket_comment"     // 后台对外评论时勾选入库
)

// DuplicateRatio 是"可能重复"的阈值：新条目的检索词中与已生效条目重合的比例不低于它时标记。未答上的提问归并用同一阈值。
const DuplicateRatio = 0.6

// MinMatchRatio 是检索时的最低重合比例（相对提问与条目中词数较少的一方）。
const MinMatchRatio = 0.2

// MissRetention 是未答上提问的保留期。
const MissRetention = 90 * 24 * time.Hour

// 长度上限（字符）。
const (
	MaxQuestionRunes = 500
	MaxAnswerRunes   = 10000
)

// 检索参数。
const (
	DefaultLimit   = 5
	MaxSearchLimit = 20
	rerankPool     = 50 // 向量只重排全文检索的前 rerankPool 条
	embedBatch     = 16
	embedLease     = 2 * time.Minute
)

// Entry 是一条知识。
type Entry struct {
	ID             int64
	ProjectKey     string
	Repo           string // "owner/name"，为空表示不限仓库
	Question       string
	Answer         string
	Source         string
	SourceTicketID int64 // 来源工单；为 0 表示无（手动新建或工单已删除）
	Status         string
	CiteCount      int64
	CreatedBy      string
	UpdatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DuplicateOf    int64 // 入库时检出的可能重复条目，0 表示无
	HelpfulCount   int64
	UnhelpfulCount int64
}

// Hit 是一条检索结果。
type Hit struct {
	*Entry
	Score float64
}

// Draft 是新建或编辑的内容。
type Draft struct {
	ProjectKey string // 新建时必填（入库时取工单所在项目）
	Repo       string
	Question   string
	Answer     string
}

// ListQuery 是维护列表的条件。
type ListQuery struct {
	Projects []string // 由 Service 按调用者填写
	Project  string
	Status   string // 为空表示全部
	Sort     string // "updated"（默认）| "cites"
	Offset   int
	Limit    int
}

// 列表排序。
const (
	SortUpdated = "updated"
	SortCites   = "cites"
)

// NewEntry 是入库的一条，由 store 写入（Tokens 已切好）。
type NewEntry struct {
	ProjectKey      string
	Repo            string
	Question        string
	Answer          string
	Source          string
	SourceTicketID  int64
	SourceMessageID int64  // 来源 d 的助手消息
	Status          string // 为空表示 StatusActive
	DuplicateOf     int64
	Tokens          []string
	By              string
}

// Pending 是待生成向量的条目。
type Pending struct {
	ID        int64
	Text      string
	UpdatedAt time.Time
}

// Writer 在调用方的事务内写入条目（答复工单与入库同一事务）。
type Writer interface {
	InsertKnowledge(ctx context.Context, e NewEntry) (int64, error)
}

// Store 由 internal/store 实现。
type Store interface {
	InsertKnowledge(ctx context.Context, e NewEntry) (int64, error)
	GetKnowledge(ctx context.Context, id int64) (*Entry, error)
	KnowledgeByIDs(ctx context.Context, ids []int64) ([]*Entry, error)
	// UpdateKnowledge 改写问题、答案与仓库，并清空向量（待重新生成）。
	UpdateKnowledge(ctx context.Context, id int64, d Draft, tokens []string, by string) error
	SetKnowledgeStatus(ctx context.Context, id int64, status, by string) error
	// TransitionKnowledge 把状态为 from 的条目改为 to；d 非空时同时改写问题、答案与仓库（清空向量）。条目不存在返回
	// ticket.ErrNotFound，状态不是 from 返回 ticket.ErrConflict。
	TransitionKnowledge(ctx context.Context, id int64, from, to string, d *Draft, tokens []string, by string) error
	// SimilarKnowledge 返回项目内已生效条目中与 tokens 重合比例（重合词数 / len(tokens)）最高的一条及其比例。
	SimilarKnowledge(ctx context.Context, project string, tokens []string) (int64, float64, error)
	// RecordKnowledgeMiss 记录项目内未答上的提问：90 天内同项目有重合比例不低于 ratio 的记录时次数加一，否则新增。
	RecordKnowledgeMiss(ctx context.Context, project, question string, tokens []string, ratio float64, since time.Time) error
	ListKnowledgeMisses(ctx context.Context, q MissQuery) ([]Miss, error)
	PurgeKnowledgeMisses(ctx context.Context, before time.Time) (int, error)
	// ClaimKnowledgeExtractions 登记新结案的工单并领取一批待抽取的工单（加租约）。
	ClaimKnowledgeExtractions(ctx context.Context, limit int, lease time.Duration) ([]int64, error)
	// FinishKnowledgeExtraction 记录抽取结果：done / skipped 结束；失败时 attempts 加一，达到 maxAttempts 记 failed，
	// 否则 retryAt 后再领取。
	FinishKnowledgeExtraction(ctx context.Context, ticketID int64, status, errMsg string, maxAttempts int, retryAt time.Time) error
	DeleteKnowledge(ctx context.Context, id int64) error
	// DisableKnowledgeByTicket 停用项目 projects 内来源为该工单的已生效条目、驳回其待审核候选，返回处理条数。
	DisableKnowledgeByTicket(ctx context.Context, ticketID int64, projects []string, by string) (int, error)
	ListKnowledge(ctx context.Context, q ListQuery) ([]*Entry, error)
	// SearchKnowledgeTokens 返回已生效、在 projects（及 project，非空时）内的条目：重合词数不少于 len(tokens) 与条目词数
	// 中较小者的 minRatio（向上取整，至少 1），按重合度降序。
	SearchKnowledgeTokens(ctx context.Context, projects []string, project string, tokens []string, minRatio float64, limit int) ([]Hit, error)
	// SearchKnowledgeWords 关键词兜底：问题或答案包含任一片段（不区分大小写）。
	SearchKnowledgeWords(ctx context.Context, projects []string, project string, words []string, limit int) ([]Hit, error)
	// KnowledgeEmbeddings 返回 ids 中向量模型为 model 的条目向量。
	KnowledgeEmbeddings(ctx context.Context, ids []int64, model string) (map[int64][]float32, error)
	AddKnowledgeCites(ctx context.Context, ids []int64) error
	// ClaimKnowledgeEmbeds 领取待生成向量的已生效条目（向量为空，或 model 非空且与之不同），加租约。
	ClaimKnowledgeEmbeds(ctx context.Context, model string, limit int, lease time.Duration) ([]Pending, error)
	// SaveKnowledgeEmbedding 保存向量；条目在领取后被编辑过（updated_at 变化）时不保存。
	SaveKnowledgeEmbedding(ctx context.Context, id int64, updatedAt time.Time, vec []float32, model string) error
}

// Miss 是一条未答上的提问。
type Miss struct {
	ID       int64
	Question string
	Hits     int
	FirstAt  time.Time
	LastAt   time.Time
}

// MissQuery 是未答上提问列表的条件。
type MissQuery struct {
	Projects []string // 由 Service 按调用者填写
	Project  string
	Since    time.Time
	Offset   int
	Limit    int
}

// ExtractInput 是交给模型抽取知识的工单内容（不含提交人身份）。
type ExtractInput struct {
	Type         string // bug | feature
	Title        string
	Body         string
	Comments     []string // 对外评论（时间顺序）
	FixedVersion string
	Closed       bool // 已关闭未修复
}

// Extracted 是抽取结果；Skip 表示没有可复用的知识。
type Extracted struct {
	Skip     bool
	Question string
	Answer   string
}

// Extractor 从结案工单抽取一条问答（模型调用）。
type Extractor interface {
	ExtractKnowledge(ctx context.Context, in ExtractInput) (*Extracted, error)
}

// TicketSource 读取工单与对外评论（不鉴权，供抽取使用）。
type TicketSource func(ctx context.Context, id int64) (*ticket.Ticket, []ticket.Comment, error)

// Embedder 生成向量；Name 返回模型标识（未知时为空，以维度代替）。
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Name() string
}

// Options 是 Service 的配置。
type Options struct {
	Store Store
	// Embedder 返回当前的向量模型，nil 表示不可用（只用切词检索）。
	Embedder func() Embedder
	// Extractor 返回当前的抽取模型，nil 表示不抽取（REQ-007）。
	Extractor func() Extractor
	// Tickets 读取工单与对外评论，抽取时使用。
	Tickets TicketSource
	Now     func() time.Time
}

// Service 是知识库。
type Service struct{ o Options }

// New 创建 Service。
func New(o Options) (*Service, error) {
	if o.Store == nil {
		return nil, errors.New("knowledge: Store is required")
	}
	if o.Embedder == nil {
		o.Embedder = func() Embedder { return nil }
	}
	if o.Extractor == nil {
		o.Extractor = func() Extractor { return nil }
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{o: o}, nil
}

// Prepare 校验并清理内容：去空白，问题与答案必填且不超长，邮箱与手机号替换为占位。
func Prepare(d Draft) (Draft, error) {
	d.Question, d.Answer, d.Repo = Scrub(strings.TrimSpace(d.Question)), Scrub(strings.TrimSpace(d.Answer)), strings.TrimSpace(d.Repo)
	switch {
	case d.Question == "" || d.Answer == "":
		return d, fmt.Errorf("%w: knowledge question and answer are required", ticket.ErrInvalid)
	case utf8.RuneCountInString(d.Question) > MaxQuestionRunes:
		return d, fmt.Errorf("%w: knowledge question exceeds %d characters", ticket.ErrInvalid, MaxQuestionRunes)
	case utf8.RuneCountInString(d.Answer) > MaxAnswerRunes:
		return d, fmt.Errorf("%w: knowledge answer exceeds %d characters", ticket.ErrInvalid, MaxAnswerRunes)
	}
	return d, nil
}

// EntryTokens 是条目的检索词：问题与答案的词，问题在前。
func EntryTokens(question, answer string) []string {
	return Tokens(question + "\n" + answer)
}

// NewFromAnswer 构造答复工单时入库的条目（已 Prepare）；by 是答复人。
func NewFromAnswer(d Draft, ticketID int64, by string) NewEntry {
	return NewEntry{ProjectKey: d.ProjectKey, Repo: d.Repo, Question: d.Question, Answer: d.Answer, Source: SourceTicketAnswer,
		SourceTicketID: ticketID, Tokens: EntryTokens(d.Question, d.Answer), By: by}
}

func staff(p auth.Principal, project string) error {
	if !p.Staff() {
		return fmt.Errorf("%w: knowledge maintenance requires reviewer or admin", ticket.ErrForbidden)
	}
	if !p.InProject(project) {
		return ticket.ErrNotFound
	}
	return nil
}

// Create 手动新建条目（reviewer / admin），直接生效。
func (s *Service) Create(ctx context.Context, p auth.Principal, d Draft) (*Entry, error) {
	if err := staff(p, d.ProjectKey); err != nil {
		return nil, err
	}
	d, err := Prepare(d)
	if err != nil {
		return nil, err
	}
	id, err := s.o.Store.InsertKnowledge(ctx, NewEntry{ProjectKey: d.ProjectKey, Repo: d.Repo, Question: d.Question, Answer: d.Answer,
		Source: SourceManual, Tokens: EntryTokens(d.Question, d.Answer), By: p.ID})
	if err != nil {
		return nil, err
	}
	return s.o.Store.GetKnowledge(ctx, id)
}

// get 读取条目并校验维护权限。
func (s *Service) get(ctx context.Context, p auth.Principal, id int64) (*Entry, error) {
	e, err := s.o.Store.GetKnowledge(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := staff(p, e.ProjectKey); err != nil {
		return nil, err
	}
	return e, nil
}

// Update 编辑条目的问题、答案与仓库（reviewer / admin）；向量清空后由后台重新生成。
func (s *Service) Update(ctx context.Context, p auth.Principal, id int64, d Draft) (*Entry, error) {
	e, err := s.get(ctx, p, id)
	if err != nil {
		return nil, err
	}
	d.ProjectKey = e.ProjectKey
	if d, err = Prepare(d); err != nil {
		return nil, err
	}
	if err := s.o.Store.UpdateKnowledge(ctx, id, d, EntryTokens(d.Question, d.Answer), p.ID); err != nil {
		return nil, err
	}
	return s.o.Store.GetKnowledge(ctx, id)
}

// SetStatus 停用或恢复条目（reviewer / admin）。
func (s *Service) SetStatus(ctx context.Context, p auth.Principal, id int64, active bool) (*Entry, error) {
	e, err := s.get(ctx, p, id)
	if err != nil {
		return nil, err
	}
	from, to := StatusActive, StatusDisabled
	if active {
		from, to = StatusDisabled, StatusActive
	}
	if e.Status == to {
		return e, nil // 幂等：已是目标状态
	}
	if err := s.o.Store.TransitionKnowledge(ctx, id, from, to, nil, nil, p.ID); err != nil {
		return nil, err
	}
	return s.o.Store.GetKnowledge(ctx, id)
}

// Delete 删除条目（reviewer / admin）。
func (s *Service) Delete(ctx context.Context, p auth.Principal, id int64) error {
	if _, err := s.get(ctx, p, id); err != nil {
		return err
	}
	return s.o.Store.DeleteKnowledge(ctx, id)
}

// DisableByTicket 停用调用者项目内来源为该工单的条目（reviewer / admin），如用户要求删除个人数据时。
func (s *Service) DisableByTicket(ctx context.Context, p auth.Principal, ticketID int64) (int, error) {
	if !p.Staff() {
		return 0, fmt.Errorf("%w: knowledge maintenance requires reviewer or admin", ticket.ErrForbidden)
	}
	if ticketID <= 0 {
		return 0, fmt.Errorf("%w: ticket id is required", ticket.ErrInvalid)
	}
	return s.o.Store.DisableKnowledgeByTicket(ctx, ticketID, p.Projects, p.ID)
}

// List 列出调用者项目内的条目（reviewer / admin）。
func (s *Service) List(ctx context.Context, p auth.Principal, q ListQuery) ([]*Entry, error) {
	if !p.Staff() {
		return nil, fmt.Errorf("%w: knowledge maintenance requires reviewer or admin", ticket.ErrForbidden)
	}
	switch q.Status {
	case "", StatusActive, StatusDisabled, StatusPending, StatusRejected:
	default:
		return nil, fmt.Errorf("%w: status %q", ticket.ErrInvalid, q.Status)
	}
	switch q.Sort {
	case "":
		q.Sort = SortUpdated
	case SortUpdated, SortCites:
	default:
		return nil, fmt.Errorf("%w: sort %q", ticket.ErrInvalid, q.Sort)
	}
	if q.Project != "" && !p.InProject(q.Project) {
		return nil, nil
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	q.Projects, q.Limit = p.Projects, ticket.Limit(q.Limit, ticket.DefaultLimit)
	return s.o.Store.ListKnowledge(ctx, q)
}

// ByIDs 一次读取多条（不鉴权，供问答重新判断历史引用）。
func (s *Service) ByIDs(ctx context.Context, ids []int64) ([]*Entry, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return s.o.Store.KnowledgeByIDs(ctx, ids)
}

// Cite 记录条目被问答引用一次。
func (s *Service) Cite(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return s.o.Store.AddKnowledgeCites(ctx, ids)
}

// Search 在调用者可访问的项目（project 非空时只含该项目）中检索已生效条目：按切词重合度取前 50 条，向量可用时按
// 相似度重排，返回前 limit 条；切词无命中时用 keywords（为空时取自 query）做包含匹配兜底。
func (s *Service) Search(ctx context.Context, p auth.Principal, project, query string, keywords []string, limit int) ([]Hit, error) {
	if !p.Known() {
		return nil, fmt.Errorf("%w: unknown role", ticket.ErrForbidden)
	}
	if project != "" && !p.InProject(project) {
		return nil, nil
	}
	switch {
	case limit <= 0:
		limit = DefaultLimit
	case limit > MaxSearchLimit:
		limit = MaxSearchLimit
	}
	toks := Tokens(strings.Join(keywords, " ") + " " + query) // 关键词在前：截断时保留意图给出的关键词
	if len(toks) > MaxQueryTokens {
		toks = toks[:MaxQueryTokens]
	}
	if len(toks) == 0 {
		return nil, nil
	}
	// 重合词数至少为提问与条目中词数较少一方的 MinMatchRatio：只因"怎么""为什么"等常用词重合的条目不算相关，长问题
	// 也能找到短条目
	hits, err := s.o.Store.SearchKnowledgeTokens(ctx, p.Projects, project, toks, MinMatchRatio, rerankPool)
	if err != nil {
		return nil, err
	}
	if len(hits) == 0 {
		ws := keywords
		if len(ws) == 0 {
			ws = words(query)
		}
		if len(ws) == 0 {
			return nil, nil
		}
		if hits, err = s.o.Store.SearchKnowledgeWords(ctx, p.Projects, project, ws, limit); err != nil {
			return nil, err
		}
	}
	s.rerank(ctx, query, hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// rerank 按向量相似度重排：综合分 = 0.5×归一化重合度 + 0.5×余弦相似度（没有可用向量的条目以归一化重合度代替）。
// 向量模型不可用或出错时保持原顺序。
func (s *Service) rerank(ctx context.Context, query string, hits []Hit) {
	if len(hits) < 2 {
		return
	}
	em := s.o.Embedder()
	if em == nil {
		return
	}
	vs, err := em.Embed(ctx, []string{query})
	if err != nil || len(vs) != 1 || len(vs[0]) == 0 {
		return
	}
	qv := vs[0]
	ids := make([]int64, len(hits))
	maxScore := 0.0
	for i, h := range hits {
		ids[i] = h.ID
		maxScore = math.Max(maxScore, h.Score)
	}
	embs, err := s.o.Store.KnowledgeEmbeddings(ctx, ids, ModelName(em, len(qv)))
	if err != nil {
		return
	}
	for i := range hits {
		lex := 1.0
		if maxScore > 0 {
			lex = hits[i].Score / maxScore
		}
		sem := lex
		if v, ok := embs[hits[i].ID]; ok && len(v) == len(qv) {
			sem = math.Max(0, cosine(qv, v))
		}
		hits[i].Score = 0.5*lex + 0.5*sem
	}
	slices.SortStableFunc(hits, func(a, b Hit) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return 0
	})
}

// ModelName 是向量模型标识：Embedder 给出名称时用名称，否则为 "dim:<维度>"。
func ModelName(em Embedder, dim int) string {
	if n := strings.TrimSpace(em.Name()); n != "" {
		return n
	}
	return fmt.Sprintf("dim:%d", dim)
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// EmbedPending 为待生成向量的条目生成一批向量（多实例经租约互斥），返回生成的条数；向量模型不可用时返回 0。
// 模型有名称时，向量模型与之不同的条目也重新生成；没有名称时只处理向量为空的条目。
func (s *Service) EmbedPending(ctx context.Context) (int, error) {
	em := s.o.Embedder()
	if em == nil {
		return 0, nil
	}
	name := strings.TrimSpace(em.Name())
	ps, err := s.o.Store.ClaimKnowledgeEmbeds(ctx, name, embedBatch, embedLease)
	if err != nil || len(ps) == 0 {
		return 0, err
	}
	texts := make([]string, len(ps))
	for i, p := range ps {
		texts[i] = p.Text
	}
	vs, err := em.Embed(ctx, texts)
	if err != nil {
		return 0, err // 租约过期后再领取
	}
	if len(vs) != len(ps) {
		return 0, fmt.Errorf("knowledge: embed returned %d vectors for %d texts", len(vs), len(ps))
	}
	n := 0
	for i, p := range ps {
		if len(vs[i]) == 0 {
			continue
		}
		if err := s.o.Store.SaveKnowledgeEmbedding(ctx, p.ID, p.UpdatedAt, vs[i], ModelName(em, len(vs[i]))); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
