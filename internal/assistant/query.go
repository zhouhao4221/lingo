package assistant

import (
	"context"
	"encoding/json"
	"github.com/zhouhao4221/lingo/internal/knowledge"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/auth"
	"github.com/zhouhao4221/lingo/internal/release"
	"github.com/zhouhao4221/lingo/internal/textutil"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// 交给模型的文本长度上限（字符）。
const (
	listBodyRunes    = 300
	detailBodyRunes  = 1000
	commentRunes     = 300
	detailComments   = 5
	releaseNoteRunes = 600
	maxCandidates    = 10
)

// reply 是一轮提问的处理结果，由 finalize 落库。
type reply struct {
	content string
	intent  string
	outcome string
	cites   Citations
	title   string
	propose *Action // 新提出的待确认操作
	execute *Action // 用户确认执行的操作
	cancel  *Action // 作废的操作（取消，或用户转而问了别的）
	// streamed 表示回答已由模型逐片段回调过；固定回复由 run 在落库后整段回调一次
	streamed bool
	// fallback 表示模型回答引用了数据之外的编号，已改用确定性回答
	fallback bool
}

func jsonRaw(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	return json.RawMessage(b), err
}

// turnCtx 是一轮提问的上下文。
type turnCtx struct {
	p        auth.Principal
	loc      *time.Location
	now      time.Time
	history  []HistoryItem
	question string
	onDelta  func(string)
}

// ---- 工单 ----

// factTicket 是交给模型的工单事实（白名单字段，不含提交人与评论作者的身份）。
type factTicket struct {
	ID           int64         `json:"id"`
	Project      string        `json:"project"`
	Repo         string        `json:"repo,omitempty"`
	Type         string        `json:"type"`
	Title        string        `json:"title"`
	Status       string        `json:"status"`
	CreatedAt    string        `json:"created_at"`
	UpdatedAt    string        `json:"updated_at"`
	FixedVersion string        `json:"fixed_version,omitempty"`
	DeployedAt   string        `json:"deployed_at,omitempty"`
	Milestone    string        `json:"milestone,omitempty"`
	DueDate      string        `json:"due_date,omitempty"`
	Summary      string        `json:"summary,omitempty"`
	Mine         bool          `json:"mine,omitempty"`      // 提问人本人提交
	Submitter    string        `json:"submitter,omitempty"` // 同事的工单（external 经 TicketAudience 放宽可见，REQ-004）的提交人姓名；其他情况不发送
	Comments     []factComment `json:"recent_comments,omitempty"`
}

type factComment struct {
	From string `json:"from"` // 提交人 / 后台 / 研发
	At   string `json:"at"`
	Body string `json:"body"`
}

type factStat struct {
	Project string `json:"project"`
	Type    string `json:"type"`
	Status  string `json:"status"`
	Count   int    `json:"count"`
}

type facts struct {
	Kind     string        `json:"kind"`
	Total    int           `json:"total"`
	Shown    int           `json:"shown"`
	Stats    []factStat    `json:"stats,omitempty"`
	Tickets  []factTicket  `json:"tickets,omitempty"`
	Releases []factRelease `json:"releases,omitempty"`
	// Knowledge 是检索到的知识条目（REQ-006），回答中写作 K编号
	Knowledge []factKnowledge `json:"knowledge,omitempty"`
}

// factKnowledge 是交给模型的知识条目。
type factKnowledge struct {
	ID        string `json:"id"` // "K12"
	Question  string `json:"question"`
	Answer    string `json:"answer"`
	UpdatedAt string `json:"updated_at"`
}

// factRelease 是交给模型的版本事实。
type factRelease struct {
	Project     string           `json:"project"`
	Repo        string           `json:"repo"`
	Tag         string           `json:"tag"`
	Name        string           `json:"name,omitempty"`
	PublishedAt string           `json:"published_at"`
	Prerelease  bool             `json:"prerelease,omitempty"`
	Notes       string           `json:"notes,omitempty"`
	Deployments []factDeployment `json:"deployments,omitempty"`
	Tickets     []factRelTicket  `json:"fixed_tickets,omitempty"`
}

type factDeployment struct {
	Env string `json:"env"`
	At  string `json:"at"`
}

type factRelTicket struct {
	ID     int64  `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

func fmtTime(t time.Time, loc *time.Location) string { return t.In(loc).Format("2006-01-02 15:04") }

// issueRefRe 匹配文本中的 "#数字"：工单正文、评论与版本说明里的通常是 Gitea Issue / PR 编号，不是灵工工单号。
var issueRefRe = regexp.MustCompile(`#(\d+)`)

// plainRefs 把文本中的 "#N" 改写为 "Issue N"，交给模型的数据里 "#N" 只表示灵工工单号（见 grounded）。
func plainRefs(s string) string { return issueRefRe.ReplaceAllString(s, "Issue $1") }

func (s *Service) factOf(p auth.Principal, t *ticket.Ticket, loc *time.Location, bodyRunes int) factTicket {
	f := factTicket{Mine: p.ID != "" && t.UserID == p.ID, ID: t.ID, Project: t.ProjectKey, Repo: t.Repo, Type: typeName(t.Type), Title: plainRefs(oneLine(t.Title)),
		Status: statusName(string(t.Status)), CreatedAt: fmtTime(t.CreatedAt, loc), UpdatedAt: fmtTime(t.UpdatedAt, loc),
		FixedVersion: t.FixedTag, Milestone: t.Milestone, Summary: plainRefs(textutil.ClipEllipsis(t.Body, bodyRunes))}
	if !f.Mine && p.Role == auth.RoleExternal && p.Peer(t.UserID) {
		f.Submitter = oneLine(t.UserName) // 只对同事的工单发送姓名，其他角色、其他工单不发送提交人身份
	}
	if t.DeployedAt != nil {
		f.DeployedAt = fmtTime(*t.DeployedAt, loc)
	}
	if t.DueDate != nil {
		f.DueDate = t.DueDate.In(loc).Format("2006-01-02")
	}
	return f
}

func (s *Service) ticketRef(t *ticket.Ticket) TicketRef {
	r := TicketRef{ID: t.ID, Title: oneLine(t.Title), Status: string(t.Status)}
	if t.IssueNumber > 0 && t.Repo != "" && t.Status != ticket.StatusAbnormal {
		r.IssueURL = s.o.IssueURL(t.Repo, t.IssueNumber)
	}
	return r
}

func releaseRef(r *release.Release, project string) ReleaseRef {
	return ReleaseRef{ID: r.ID, Project: project, Repo: r.Repo, Tag: r.Tag, PublishedAt: r.PublishedAt, URL: r.URL}
}

var commentFrom = map[string]string{"user": "提交人", "staff": "后台", "agent": "研发", "gitea": "研发"}

// searchQuery 把意图换算为工单检索条件；时间范围不合法时 ok 为假。
func searchQuery(in *Intent, now time.Time, loc *time.Location) (ticket.SearchQuery, bool) {
	q := ticket.SearchQuery{IDs: in.TicketIDs, Repo: in.Repo, Type: in.Type, Statuses: in.Statuses, Keywords: in.Keywords,
		TimeField: in.Time.Field}
	if in.IssueExplicit {
		q.IssueNumber = in.IssueNumber
	}
	from, to, ok := timeRange(in.Time, now, loc)
	q.From, q.To = from, to
	return q, ok
}

// findTickets 回答查找类问题（工单进展、修复版本）。
func (s *Service) findTickets(ctx context.Context, tc *turnCtx, in *Intent) (*reply, error) {
	q, ok := searchQuery(in, tc.now, tc.loc)
	if !ok {
		return &reply{content: textClarify, outcome: OutcomeClarify}, nil
	}
	limit := maxCandidates + 1
	if len(in.TicketIDs) > 0 {
		limit = MaxTickets
	}
	q.Limit = limit
	ts, err := s.o.Tickets.Search(ctx, tc.p, in.Project, in.Mine, q)
	if err != nil {
		return nil, err
	}
	switch {
	case len(ts) == 0:
		return &reply{content: textNotFound, outcome: OutcomeNotFound}, nil
	case len(ts) > 1 && len(in.TicketIDs) == 0:
		total := len(ts)
		if total > maxCandidates {
			ts = ts[:maxCandidates]
			if total, err = s.count(ctx, tc.p, in, q); err != nil {
				return nil, err
			}
		}
		r := &reply{content: candidatesText(ts, total), outcome: OutcomeCandidates}
		for _, t := range ts {
			r.cites.Tickets = append(r.cites.Tickets, s.ticketRef(t))
		}
		return r, nil
	}
	f := facts{Kind: "ticket_detail", Total: len(ts), Shown: len(ts)}
	var cites Citations
	for _, t := range ts {
		ft := s.factOf(tc.p, t, tc.loc, detailBodyRunes)
		var cs []ticket.Comment
		if len(ts) == 1 { // 只在回答单张工单时带上最近的评论（见 README 中送给模型的内容）
			if cs, err = s.o.Tickets.Comments(ctx, tc.p, t.ID); err != nil {
				return nil, err
			}
		}
		if n := len(cs); n > detailComments {
			cs = cs[n-detailComments:]
		}
		for _, c := range cs {
			from := commentFrom[c.Source]
			if from == "" {
				from = "研发"
			}
			ft.Comments = append(ft.Comments, factComment{From: from, At: fmtTime(c.At, tc.loc), Body: plainRefs(textutil.ClipEllipsis(c.Body, commentRunes))})
		}
		f.Tickets = append(f.Tickets, ft)
		cites.Tickets = append(cites.Tickets, s.ticketRef(t))
		if t.FixedReleaseID > 0 {
			d, err := s.o.Releases.Get(ctx, tc.p, t.FixedReleaseID, s.o.TicketsByID)
			if err == nil {
				cites.Releases = append(cites.Releases, releaseRef(d.Release, t.ProjectKey))
			}
		}
	}
	return s.answer(ctx, tc, f, cites, func() string { return ticketsText(ts, tc.loc) })
}

// count 返回检索条件下的工单总数。
func (s *Service) count(ctx context.Context, p auth.Principal, in *Intent, q ticket.SearchQuery) (int, error) {
	rows, err := s.o.Tickets.Stats(ctx, p, in.Project, in.Mine, q)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		n += r.Count
	}
	return n, nil
}

// summarize 回答汇总类问题（数量、列表）。
func (s *Service) summarize(ctx context.Context, tc *turnCtx, in *Intent) (*reply, error) {
	q, ok := searchQuery(in, tc.now, tc.loc)
	if !ok {
		return &reply{content: textClarify, outcome: OutcomeClarify}, nil
	}
	rows, err := s.o.Tickets.Stats(ctx, tc.p, in.Project, in.Mine, q)
	if err != nil {
		return nil, err
	}
	f := facts{Kind: "ticket_summary"}
	for _, r := range rows {
		f.Total += r.Count
		f.Stats = append(f.Stats, factStat{Project: r.ProjectKey, Type: typeName(r.Type), Status: statusName(r.Status), Count: r.Count})
	}
	if f.Total == 0 {
		return &reply{content: textNotFound, outcome: OutcomeNotFound}, nil
	}
	q.Limit = MaxTickets
	ts, err := s.o.Tickets.Search(ctx, tc.p, in.Project, in.Mine, q)
	if err != nil {
		return nil, err
	}
	var cites Citations
	for _, t := range ts {
		f.Tickets = append(f.Tickets, s.factOf(tc.p, t, tc.loc, listBodyRunes))
		cites.Tickets = append(cites.Tickets, s.ticketRef(t))
	}
	f.Shown = len(ts)
	return s.answer(ctx, tc, f, cites, func() string {
		txt := "共 " + strconv.Itoa(f.Total) + " 张：\n\n" + ticketsText(ts, tc.loc)
		if f.Total > len(ts) {
			txt += "\n\n以上为最近的 " + strconv.Itoa(len(ts)) + " 张。"
		}
		return txt
	})
}

// ---- 版本 ----

// listReleases 回答发版类问题：指定 tag 时只看该版本；未指定项目且可访问多个项目时，各项目平均分配名额（合计最多 10 个）。
func (s *Service) listReleases(ctx context.Context, tc *turnCtx, in *Intent) (*reply, error) {
	from, to, ok := timeRange(in.Time, tc.now, tc.loc)
	if !ok {
		return &reply{content: textClarify, outcome: OutcomeClarify}, nil
	}
	base := release.Query{Repo: in.Release.Repo, Tag: in.Release.Tag, Since: from, Until: to}
	if base.Repo == "" {
		base.Repo = in.Repo
	}
	projects := []string{in.Project}
	if in.Project == "" && base.Repo == "" {
		if ps := s.o.Projects(tc.p); len(ps) > 1 {
			projects = projects[:0]
			for _, p := range ps {
				projects = append(projects, p.Key)
			}
		}
	}
	quota := max(1, MaxReleases/len(projects))
	var rs []*release.Release
	total := 0
	for _, key := range projects {
		q := base
		q.Project, q.Limit = key, quota
		got, err := s.o.Releases.List(ctx, tc.p, q)
		if err != nil {
			return nil, err
		}
		n, err := s.o.Releases.Count(ctx, tc.p, q)
		if err != nil {
			return nil, err
		}
		rs, total = append(rs, got...), total+n
	}
	// 各项目的候选合在一起按发布时间取最新的（项目多于 10 个时每个项目只取 1 个，不能按配置顺序截断）
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].PublishedAt.After(rs[j].PublishedAt) })
	if len(rs) > MaxReleases {
		rs = rs[:MaxReleases]
	}
	if len(rs) == 0 {
		return &reply{content: textNoRelease, outcome: OutcomeNotFound}, nil
	}
	f := facts{Kind: "releases", Total: total, Shown: len(rs)}
	var cites Citations
	var details []*release.Detail
	seen := map[int64]bool{}
	for _, r := range rs {
		d, err := s.o.Releases.Get(ctx, tc.p, r.ID, s.o.TicketsByID)
		if err != nil {
			return nil, err
		}
		details = append(details, d)
		project := s.o.ProjectOfRepo(r.Repo)
		fr := factRelease{Project: project, Repo: r.Repo, Tag: r.Tag, Name: plainRefs(r.Name), PublishedAt: fmtTime(r.PublishedAt, tc.loc),
			Prerelease: r.Prerelease, Notes: plainRefs(textutil.ClipEllipsis(r.Body, releaseNoteRunes))}
		for _, dp := range d.Deployments {
			fr.Deployments = append(fr.Deployments, factDeployment{Env: dp.Env, At: fmtTime(dp.At, tc.loc)})
		}
		for _, t := range d.Tickets {
			if len(fr.Tickets) >= MaxTickets {
				break
			}
			fr.Tickets = append(fr.Tickets, factRelTicket{ID: t.ID, Title: plainRefs(oneLine(t.Title)), Status: statusName(string(t.Status))})
			if !seen[t.ID] {
				seen[t.ID] = true
				cites.Tickets = append(cites.Tickets, s.ticketRef(t))
			}
		}
		f.Releases = append(f.Releases, fr)
		cites.Releases = append(cites.Releases, releaseRef(r, project))
	}
	return s.answer(ctx, tc, f, cites, func() string { return releasesText(details, tc.loc) })
}

// ---- 组织回答 ----

var ticketNumRe = regexp.MustCompile(`#(\d+)`)

// answer 让模型依据事实组织回答。回答中出现事实之外的工单号（编造）时丢弃模型输出，改用 fallback 的确定性回答；
// 流式输出经 groundedStream 过滤，编造的工单号不会推给用户（之后的片段也不再推送，以最终消息为准）。
func (s *Service) answer(ctx context.Context, tc *turnCtx, f facts, cites Citations, fallback func() string) (*reply, error) {
	data, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	// 允许的工单号：本轮数据中的，加上提问与历史引用中出现过的（如"没有找到 #13""和之前的 #5 一样"）
	ids := append(factIDs(f), tc.knownIDs()...)
	// 知识回答另校验 K 编号：本轮数据中的，加上提问与历史引用中出现过的；其他回答中的 "K3" 之类是普通文字（如产品型号）
	var kids []int64
	checkK := f.Kind == "knowledge"
	if checkK {
		kids = append(knowledgeFactIDs(f), tc.knownKnowledgeIDs()...)
	}
	var gs *groundedStream
	var onDelta func(string)
	if tc.onDelta != nil {
		gs = &groundedStream{ids: ids, kids: kids, checkK: checkK, out: tc.onDelta}
		onDelta = gs.write
	}
	out, err := s.brainOf(ctx).Answer(ctx, AnswerInput{History: tc.history, Question: tc.question, Facts: data}, onDelta)
	if err != nil {
		return nil, modelError{err}
	}
	ok := grounded(out, ids) && (!checkK || groundedKnowledge(out, kids))
	if !ok {
		out = fallback()
	}
	r := &reply{content: out, outcome: OutcomeAnswered, cites: cites, fallback: !ok}
	switch {
	case gs == nil:
	case ok && !gs.bad:
		gs.flush()
		r.streamed = true
	case gs.sent == 0:
		r.streamed = false // 还没推送过：由 run 整段推送确定性回答
	default:
		r.streamed = true // 已推送了一部分：以最终消息为准
	}
	return r, nil
}

// knownIDs 是提问中写到的工单号与历史回答引用过的工单号。
func (tc *turnCtx) knownIDs() []int64 {
	var ids []int64
	for _, m := range ticketNumRe.FindAllStringSubmatch(tc.question, -1) {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			ids = append(ids, n)
		}
	}
	for _, h := range tc.history {
		for _, t := range h.Tickets {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

func factIDs(f facts) []int64 {
	var ids []int64
	for _, t := range f.Tickets {
		ids = append(ids, t.ID)
	}
	for _, r := range f.Releases {
		for _, t := range r.Tickets {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

// grounded 报告回答中出现的 "#数字" 都是事实中的工单。
func grounded(out string, ids []int64) bool {
	for _, m := range ticketNumRe.FindAllStringSubmatch(out, -1) {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil || !slices.Contains(ids, n) {
			return false
		}
	}
	return true
}

var knowledgeNumRe = regexp.MustCompile(`\bK(\d+)\b`)

// knowledgeRefs 返回文本中的知识条目编号（K12 → 12）。
func knowledgeRefs(s string) []int64 {
	var out []int64
	for _, m := range knowledgeNumRe.FindAllStringSubmatch(s, -1) {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// groundedKnowledge 报告回答中的知识条目编号（K12）都在允许的编号中。
func groundedKnowledge(out string, ids []int64) bool {
	for _, n := range knowledgeRefs(out) {
		if !slices.Contains(ids, n) {
			return false
		}
	}
	return true
}

func knowledgeFactIDs(f facts) []int64 {
	var ids []int64
	for _, k := range f.Knowledge {
		if n, err := strconv.ParseInt(strings.TrimPrefix(k.ID, "K"), 10, 64); err == nil {
			ids = append(ids, n)
		}
	}
	return ids
}

// knownKnowledgeIDs 是提问中写到的与历史回答引用过的知识条目编号。
func (tc *turnCtx) knownKnowledgeIDs() []int64 {
	ids := knowledgeRefs(tc.question)
	for _, h := range tc.history {
		for _, k := range h.Knowledge {
			ids = append(ids, k.ID)
		}
	}
	return ids
}

// 知识问答的数量与截断。
const (
	knowledgeHits    = 5
	knowledgeTickets = 5
	knowledgeRunes   = 1500
)

// askKnowledge 回答知识类问题：检索调用者项目内已生效的知识条目，并检索调用者可见的同类工单，一并交给模型作答；
// 都没有命中时引导提交咨询工单。来源工单只在调用者能查看时出现在引用中。
func (s *Service) askKnowledge(ctx context.Context, tc *turnCtx, in *Intent) (*reply, error) {
	hits, err := s.o.Knowledge.Search(ctx, tc.p, in.Project, tc.question, in.Keywords, knowledgeHits)
	if err != nil {
		return nil, err
	}
	var ts []*ticket.Ticket
	if len(in.Keywords) > 0 {
		q := ticket.SearchQuery{Keywords: in.Keywords, Type: in.Type, Limit: knowledgeTickets}
		if ts, err = s.o.Tickets.Search(ctx, tc.p, in.Project, false, q); err != nil {
			return nil, err
		}
	}
	if len(hits) == 0 && len(ts) == 0 {
		return &reply{content: textNoKnowledge, outcome: OutcomeNotFound}, nil
	}
	readable, err := s.readableSources(ctx, tc.p, hits)
	if err != nil {
		return nil, err
	}
	f := facts{Kind: "knowledge", Total: len(hits) + len(ts), Shown: len(hits) + len(ts)}
	var cites Citations
	for _, h := range hits {
		f.Knowledge = append(f.Knowledge, factKnowledge{ID: "K" + strconv.FormatInt(h.ID, 10), Question: plainRefs(oneLine(h.Question)),
			Answer: plainRefs(textutil.ClipEllipsis(h.Answer, knowledgeRunes)), UpdatedAt: fmtTime(h.UpdatedAt, tc.loc)})
		ref := KnowledgeRef{ID: h.ID, Question: oneLine(h.Question)}
		if readable[h.SourceTicketID] {
			ref.SourceTicketID = h.SourceTicketID
		}
		cites.Knowledge = append(cites.Knowledge, ref)
	}
	for _, t := range ts {
		f.Tickets = append(f.Tickets, s.factOf(tc.p, t, tc.loc, listBodyRunes))
		cites.Tickets = append(cites.Tickets, s.ticketRef(t))
	}
	r, err := s.answer(ctx, tc, f, cites, func() string { return knowledgeText(hits, ts, tc.loc) })
	if err != nil {
		return nil, err
	}
	// 引用与引用次数只算回答中实际写到的条目（检索可能因"怎么""为什么"等常用词命中无关条目）；回答没有写编号时引用
	// 全部检索结果但不计数；改用确定性回答时同样不计数
	used := knowledgeRefs(r.content)
	var refs []KnowledgeRef
	var counted []int64
	for _, k := range r.cites.Knowledge {
		if slices.Contains(used, k.ID) {
			refs = append(refs, k)
			counted = append(counted, k.ID)
		}
	}
	if len(refs) > 0 {
		r.cites.Knowledge = refs
	}
	if !r.fallback && len(counted) > 0 {
		_ = s.o.Knowledge.Cite(ctx, counted) // 引用次数是统计数据：失败不影响回答
	}
	return r, nil
}

// readableSources 返回调用者能查看的来源工单（ticket.Readable）。
func (s *Service) readableSources(ctx context.Context, p auth.Principal, hits []knowledge.Hit) (map[int64]bool, error) {
	var ids []int64
	for _, h := range hits {
		if h.SourceTicketID > 0 {
			ids = append(ids, h.SourceTicketID)
		}
	}
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	ts, err := s.o.TicketsByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, t := range ts {
		out[t.ID] = ticket.Readable(p, t)
	}
	return out, nil
}

// groundedStream 转发模型的流式片段：末尾可能还没写完的 "#数字" 先扣住，完整的工单号在事实之外时停止转发。
type groundedStream struct {
	ids    []int64
	kids   []int64 // 允许的知识条目编号（checkK 时）
	checkK bool
	out    func(string)
	buf    strings.Builder
	sent   int // 已转发的字节数
	bad    bool
}

func (g *groundedStream) write(d string) {
	if g.bad {
		return
	}
	g.buf.WriteString(d)
	s := g.buf.String()
	safe := len(s)
	if i := strings.LastIndexByte(s, '#'); i >= 0 && strings.Trim(s[i+1:], "0123456789") == "" {
		safe = i // "#" 后面可能还有数字没到
	}
	if g.checkK {
		if i := strings.LastIndexByte(s, 'K'); i >= 0 && i < safe && strings.Trim(s[i+1:], "0123456789") == "" {
			safe = i // "K" 后面可能还有数字没到
		}
	}
	if !grounded(s[:safe], g.ids) || (g.checkK && !groundedKnowledge(s[:safe], g.kids)) {
		g.bad = true
		return
	}
	if safe > g.sent {
		g.out(s[g.sent:safe])
		g.sent = safe
	}
}

// flush 转发扣住的末尾（整段回答已确认无编造）。
func (g *groundedStream) flush() {
	if s := g.buf.String(); !g.bad && len(s) > g.sent {
		g.out(s[g.sent:])
		g.sent = len(s)
	}
}
