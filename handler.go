package lingo

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/zhouhao4221/lingo/internal/httpapi"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// Handler 返回 REST JSON 接口，路径为相对路径，由接入方挂载到任意前缀（配合 http.StripPrefix）。身份由
// Config.Authenticate 解析，失败返回 401；错误响应为 {"error":{"code","message"}}，code 取
// bad_request / invalid_input（400）、forbidden（403）、not_found（404）、conflict（409）、internal（500）。
//
// 工单：GET /ticket-types、POST /tickets、GET /tickets/mine、GET /tickets/{id}、POST /tickets/{id}/comments；
// 需求视角：POST / GET /tickets/{id}/perspectives；
// 项目与版本：GET /projects、GET /releases、GET /releases/{id}；
// 后台：POST /tickets/{id}/forward|answer|merge|reject、POST /tickets/{id}/reject-forwarded（驳回已转研发工单）、
// GET /tickets/{id}/issue-close、POST /tickets/{id}/issue-close/retry（关闭关联 Issue 的进度与重试）、
// GET /triage、GET /triage/summary、
// GET / PUT /settings/auto-forward、GET /usage（模型用量，admin）；变更流：GET /events?after=。
// 连接配置（模型与 Gitea，权限见 Config.AuthorizeSettings）：GET / PUT /settings/connections、
// POST /settings/connections/test。
//
// 知识库（REQ-006）：GET /knowledge/search?q=&project=&limit=（所有角色）；GET /knowledge、POST /knowledge、
// PUT / DELETE /knowledge/{id}、POST /knowledge/{id}/disable|enable、POST /knowledge/disable-by-ticket（reviewer / admin）；
// POST /tickets/{id}/answer 与 POST /tickets/{id}/comments 可带 {"knowledge":{"question","answer"}} 把答复 / 评论加入知识库；
// 审核：POST /knowledge/{id}/approve（可带编辑后的内容）|reject；GET /knowledge/unanswered（REQ-007）。
// 回答反馈：POST /conversations/{id}/messages/{mid}/feedback（{"helpful":true}）。
// 知识问答（会话只有本人可见）：POST /conversations、GET /conversations、DELETE /conversations/{id}、
// GET /conversations/{id}/messages、POST /conversations/{id}/messages（{"content","time_zone","stream"}）、
// POST /conversations/{id}/messages/{mid}/retry、POST /conversations/{id}/actions/{aid}/confirm|cancel。
// stream 为 true 时以 SSE 返回：若干 event: delta（data {"text"}），最后恰好一个 event: message（data 为完整的助手消息，
// 以它的 content 为准）；开始输出后出错发送 event: error（data {"code","message"}）。开始输出前的错误仍按 JSON 错误响应。
// 未配置 Config.Model 时发送与重试返回 501 model_not_configured。
func (l *Lingo) Handler() http.Handler { return l.rest }

func (l *Lingo) newRESTHandler() http.Handler {
	rt := httpapi.New(httpapi.Options{
		Authenticate: func(r *http.Request) (any, error) { return l.cfg.Authenticate(r) },
		MapError:     restError,
		Logger:       l.log,
	})
	user := func(r *httpapi.Request) User { return r.User.(User) }
	ctx := func(r *httpapi.Request) context.Context { return ticket.WithSource(r.Context(), "api") }

	rt.Handle("GET /ticket-types", func(r *httpapi.Request) (any, error) {
		return l.TicketTypes(ctx(r), user(r), r.Query("project"), r.Query("repo"))
	})
	rt.Handle("POST /tickets", func(r *httpapi.Request) (any, error) {
		var in struct {
			Project     string              `json:"project"`
			Repo        string              `json:"repo"`
			Type        TicketType          `json:"type"`
			Title       string              `json:"title"`
			Content     string              `json:"content"`
			Fields      map[string][]string `json:"fields"`
			Context     map[string]string   `json:"context"`
			Version     string              `json:"version"`
			Attachments []Attachment        `json:"attachments"`
		}
		if err := r.Decode(&in); err != nil {
			return nil, err
		}
		t, err := l.CreateTicket(ctx(r), user(r), TicketInput{Project: in.Project, Repo: in.Repo, Type: in.Type, Title: in.Title,
			Content: in.Content, Fields: in.Fields, Context: in.Context, Version: in.Version,
			Attachments: in.Attachments})
		if err != nil {
			return nil, err
		}
		return httpapi.Created{Value: t}, nil
	})
	rt.Handle("GET /tickets/mine", func(r *httpapi.Request) (any, error) {
		before, err := r.QueryInt("before")
		if err != nil {
			return nil, err
		}
		limit, err := r.QueryInt("limit")
		if err != nil {
			return nil, err
		}
		ts, err := l.ListMyTickets(ctx(r), user(r), r.Query("status"), before, int(limit))
		return list(ts, err)
	})
	rt.Handle("GET /tickets/visible", func(r *httpapi.Request) (any, error) {
		before, err := r.QueryInt("before")
		if err != nil {
			return nil, err
		}
		limit, err := r.QueryInt("limit")
		if err != nil {
			return nil, err
		}
		ts, err := l.ListVisibleTickets(ctx(r), user(r), VisibleTicketFilter{Project: r.Query("project"), Status: r.Query("status"),
			BeforeID: before, Limit: int(limit)})
		return list(ts, err)
	})
	rt.Handle("GET /tickets/{id}", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		t, cs, as, err := l.ticketDetail(ctx(r), user(r), id)
		if err != nil {
			return nil, err
		}
		return struct {
			Ticket
			Comments    []Comment    `json:"comments"`
			Attachments []Attachment `json:"attachments"`
		}{t, cs, as}, nil
	})
	rt.Handle("POST /tickets/{id}/comments", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		var in struct {
			Body        string          `json:"body"`
			Attachments []Attachment    `json:"attachments"`
			Knowledge   *KnowledgeDraft `json:"knowledge"` // 非空时加入知识库（reviewer / admin，REQ-007）
		}
		if err := r.Decode(&in); err != nil {
			return nil, err
		}
		c, err := l.AddTicketCommentWith(ctx(r), user(r), id, CommentInput{Body: in.Body, Attachments: in.Attachments, Knowledge: in.Knowledge})
		if err != nil {
			return nil, err
		}
		return httpapi.Created{Value: c}, nil
	})
	rt.Handle("POST /tickets/{id}/perspectives", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		var in struct {
			Content string `json:"content"`
		}
		if err := r.Decode(&in); err != nil {
			return nil, err
		}
		p, err := l.AddPerspective(ctx(r), user(r), id, in.Content)
		if err != nil {
			return nil, err
		}
		return httpapi.Created{Value: p}, nil
	})
	rt.Handle("GET /tickets/{id}/perspectives", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		return l.ListPerspectives(ctx(r), user(r), id)
	})
	triage := func(name string, do func(r *httpapi.Request, id int64) (Ticket, error)) {
		rt.Handle("POST /tickets/{id}/"+name, func(r *httpapi.Request) (any, error) {
			id, err := r.PathID("id")
			if err != nil {
				return nil, err
			}
			return do(r, id)
		})
	}
	triage("forward", func(r *httpapi.Request, id int64) (Ticket, error) {
		var in struct {
			Title         string     `json:"title"`
			Type          TicketType `json:"type"`
			Repo          string     `json:"repo"`
			Note          string     `json:"note"`
			AttachmentIDs *[]string  `json:"attachment_ids"` // 省略或 null 表示全部
		}
		if err := r.DecodeOptional(&in); err != nil {
			return Ticket{}, err
		}
		return l.ForwardTicket(ctx(r), user(r), id, ForwardOptions{Title: in.Title, Type: in.Type, Repo: in.Repo, Note: in.Note,
			AttachmentIDs: in.AttachmentIDs})
	})
	triage("answer", func(r *httpapi.Request, id int64) (Ticket, error) {
		var in struct {
			Answer    string          `json:"answer"`
			Knowledge *KnowledgeDraft `json:"knowledge"` // 非空时加入知识库（REQ-006）
		}
		if err := r.Decode(&in); err != nil {
			return Ticket{}, err
		}
		return l.AnswerTicketWith(ctx(r), user(r), id, TicketAnswer{Answer: in.Answer, Knowledge: in.Knowledge})
	})
	triage("merge", func(r *httpapi.Request, id int64) (Ticket, error) {
		var in struct {
			Target int64  `json:"target_ticket_id"`
			Note   string `json:"note"`
		}
		if err := r.Decode(&in); err != nil {
			return Ticket{}, err
		}
		return l.MergeTicket(ctx(r), user(r), id, in.Target, in.Note)
	})
	triage("reject", func(r *httpapi.Request, id int64) (Ticket, error) {
		var in struct {
			Reason string `json:"reason"`
		}
		if err := r.Decode(&in); err != nil {
			return Ticket{}, err
		}
		return l.RejectTicket(ctx(r), user(r), id, in.Reason)
	})
	triage("reject-forwarded", func(r *httpapi.Request, id int64) (Ticket, error) {
		var in struct {
			Reason string `json:"reason"`
		}
		if err := r.Decode(&in); err != nil {
			return Ticket{}, err
		}
		return l.RejectForwardedTicket(ctx(r), user(r), id, in.Reason)
	})
	rt.Handle("GET /tickets/{id}/issue-close", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		return l.TicketIssueClose(ctx(r), user(r), id)
	})
	rt.Handle("POST /tickets/{id}/issue-close/retry", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		return l.RetryTicketIssueClose(ctx(r), user(r), id)
	})
	rt.Handle("GET /triage", func(r *httpapi.Request) (any, error) {
		after, err := r.QueryInt("after")
		if err != nil {
			return nil, err
		}
		limit, err := r.QueryInt("limit")
		if err != nil {
			return nil, err
		}
		before, err := r.QueryTime("pending_before")
		if err != nil {
			return nil, err
		}
		ts, err := l.ListTriage(ctx(r), user(r), TriageFilter{ProjectKey: r.Query("project"), Type: TicketType(r.Query("type")),
			PendingBefore: before, AfterID: after, Limit: int(limit)})
		return list(ts, err)
	})
	rt.Handle("GET /triage/summary", func(r *httpapi.Request) (any, error) {
		cs, err := l.TriageSummary(ctx(r), user(r))
		return list(cs, err)
	})
	rt.Handle("GET /events", func(r *httpapi.Request) (any, error) {
		after, err := r.QueryInt("after")
		if err != nil {
			return nil, err
		}
		limit, err := r.QueryInt("limit")
		if err != nil {
			return nil, err
		}
		es, err := l.ListEvents(ctx(r), user(r), after, int(limit))
		return list(es, err)
	})
	rt.Handle("GET /projects", func(r *httpapi.Request) (any, error) {
		ps, err := l.ListProjects(ctx(r), user(r))
		return list(ps, err)
	})
	rt.Handle("GET /releases", func(r *httpapi.Request) (any, error) {
		before, err := r.QueryInt("before")
		if err != nil {
			return nil, err
		}
		limit, err := r.QueryInt("limit")
		if err != nil {
			return nil, err
		}
		rs, err := l.ListReleases(ctx(r), user(r), r.Query("project"), r.Query("repo"), before, int(limit))
		return list(rs, err)
	})
	rt.Handle("GET /releases/{id}", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		return l.GetRelease(ctx(r), user(r), id)
	})
	rt.Handle("GET /usage", func(r *httpapi.Request) (any, error) {
		from, err := r.QueryTime("from")
		if err != nil {
			return nil, err
		}
		to, err := r.QueryTime("to")
		if err != nil {
			return nil, err
		}
		us, err := l.LLMUsage(ctx(r), user(r), from, to)
		return list(us, err)
	})
	rt.Handle("POST /conversations", func(r *httpapi.Request) (any, error) {
		var in struct {
			Title string `json:"title"`
		}
		if err := r.DecodeOptional(&in); err != nil {
			return nil, err
		}
		c, err := l.CreateConversation(ctx(r), user(r), in.Title)
		if err != nil {
			return nil, err
		}
		return httpapi.Created{Value: c}, nil
	})
	rt.Handle("GET /conversations", func(r *httpapi.Request) (any, error) {
		before, err := r.QueryInt("before")
		if err != nil {
			return nil, err
		}
		limit, err := r.QueryInt("limit")
		if err != nil {
			return nil, err
		}
		return list(l.ListConversations(ctx(r), user(r), before, int(limit)))
	})
	rt.Handle("GET /knowledge/search", func(r *httpapi.Request) (any, error) {
		limit, err := r.QueryInt("limit")
		if err != nil {
			return nil, err
		}
		es, err := l.SearchKnowledge(ctx(r), user(r), r.Query("q"), r.Query("project"), int(limit))
		return list(es, err)
	})
	rt.Handle("GET /knowledge", func(r *httpapi.Request) (any, error) {
		offset, err := r.QueryInt("offset")
		if err != nil {
			return nil, err
		}
		limit, err := r.QueryInt("limit")
		if err != nil {
			return nil, err
		}
		es, err := l.ListKnowledge(ctx(r), user(r), KnowledgeFilter{Project: r.Query("project"), Status: r.Query("status"),
			Sort: r.Query("sort"), Offset: int(offset), Limit: int(limit)})
		return list(es, err)
	})
	rt.Handle("POST /knowledge", func(r *httpapi.Request) (any, error) {
		var in KnowledgeDraft
		if err := r.Decode(&in); err != nil {
			return nil, err
		}
		e, err := l.CreateKnowledge(ctx(r), user(r), in)
		if err != nil {
			return nil, err
		}
		return httpapi.Created{Value: e}, nil
	})
	rt.Handle("GET /knowledge/unanswered", func(r *httpapi.Request) (any, error) {
		since, err := r.QueryTime("since")
		if err != nil {
			return nil, err
		}
		offset, err := r.QueryInt("offset")
		if err != nil {
			return nil, err
		}
		limit, err := r.QueryInt("limit")
		if err != nil {
			return nil, err
		}
		qs, err := l.ListUnansweredQuestions(ctx(r), user(r), UnansweredFilter{Project: r.Query("project"), Since: since,
			Offset: int(offset), Limit: int(limit)})
		return list(qs, err)
	})
	rt.Handle("POST /knowledge/{id}/approve", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		var in *KnowledgeDraft
		if err := r.DecodeOptional(&in); err != nil {
			return nil, err
		}
		return l.ApproveKnowledge(ctx(r), user(r), id, in)
	})
	rt.Handle("POST /knowledge/{id}/reject", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		return l.RejectKnowledge(ctx(r), user(r), id)
	})
	rt.Handle("POST /knowledge/disable-by-ticket", func(r *httpapi.Request) (any, error) {
		var in struct {
			TicketID int64 `json:"ticket_id"`
		}
		if err := r.Decode(&in); err != nil {
			return nil, err
		}
		n, err := l.DisableKnowledgeByTicket(ctx(r), user(r), in.TicketID)
		if err != nil {
			return nil, err
		}
		return struct {
			Disabled int `json:"disabled"`
		}{n}, nil
	})
	rt.Handle("PUT /knowledge/{id}", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		var in KnowledgeDraft
		if err := r.Decode(&in); err != nil {
			return nil, err
		}
		return l.UpdateKnowledge(ctx(r), user(r), id, in)
	})
	rt.Handle("DELETE /knowledge/{id}", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		return nil, l.DeleteKnowledge(ctx(r), user(r), id)
	})
	for name, active := range map[string]bool{"disable": false, "enable": true} {
		rt.Handle("POST /knowledge/{id}/"+name, func(r *httpapi.Request) (any, error) {
			id, err := r.PathID("id")
			if err != nil {
				return nil, err
			}
			if active {
				return l.EnableKnowledge(ctx(r), user(r), id)
			}
			return l.DisableKnowledge(ctx(r), user(r), id)
		})
	}
	rt.Handle("DELETE /conversations/{id}", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		return nil, l.DeleteConversation(ctx(r), user(r), id)
	})
	rt.Handle("GET /conversations/{id}/messages", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		before, err := r.QueryInt("before")
		if err != nil {
			return nil, err
		}
		limit, err := r.QueryInt("limit")
		if err != nil {
			return nil, err
		}
		return list(l.ListConversationMessages(ctx(r), user(r), id, before, int(limit)))
	})
	// reply 返回助手消息；stream 时以 SSE 输出
	reply := func(r *httpapi.Request, stream bool, do func(onDelta func(string)) (ConversationMessage, error)) (any, error) {
		if !stream {
			return do(nil)
		}
		sse := r.SSE()
		m, err := do(func(d string) {
			_ = sse.Send("delta", struct {
				Text string `json:"text"`
			}{d})
		})
		if err != nil {
			return nil, err
		}
		if err := sse.Send("message", m); err != nil {
			return nil, nil //nolint:nilerr // 客户端已断开：消息已落库，下次读取历史即可
		}
		return httpapi.Streamed{}, nil
	}
	rt.Handle("POST /conversations/{id}/messages", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		var in struct {
			Content  string `json:"content"`
			TimeZone string `json:"time_zone"`
			Stream   bool   `json:"stream"`
		}
		if err := r.Decode(&in); err != nil {
			return nil, err
		}
		return reply(r, in.Stream, func(onDelta func(string)) (ConversationMessage, error) {
			return l.SendMessageStream(ctx(r), user(r), id, MessageInput{Content: in.Content, TimeZone: in.TimeZone}, onDelta)
		})
	})
	rt.Handle("POST /conversations/{id}/messages/{mid}/feedback", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		mid, err := r.PathID("mid")
		if err != nil {
			return nil, err
		}
		var in struct {
			Helpful *bool `json:"helpful"`
		}
		if err := r.Decode(&in); err != nil {
			return nil, err
		}
		if in.Helpful == nil {
			return nil, fmt.Errorf("%w: helpful is required", ErrInvalidInput)
		}
		return nil, l.RateMessage(ctx(r), user(r), id, mid, *in.Helpful)
	})
	rt.Handle("POST /conversations/{id}/messages/{mid}/retry", func(r *httpapi.Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		mid, err := r.PathID("mid")
		if err != nil {
			return nil, err
		}
		var in struct {
			TimeZone string `json:"time_zone"`
			Stream   bool   `json:"stream"`
		}
		if err := r.DecodeOptional(&in); err != nil {
			return nil, err
		}
		return reply(r, in.Stream, func(onDelta func(string)) (ConversationMessage, error) {
			return l.RetryMessageStream(ctx(r), user(r), id, mid, in.TimeZone, onDelta)
		})
	})
	action := func(name string, do func(r *httpapi.Request, id, aid int64) (ActionResult, error)) {
		rt.Handle("POST /conversations/{id}/actions/{aid}/"+name, func(r *httpapi.Request) (any, error) {
			id, err := r.PathID("id")
			if err != nil {
				return nil, err
			}
			aid, err := r.PathID("aid")
			if err != nil {
				return nil, err
			}
			return do(r, id, aid)
		})
	}
	action("confirm", func(r *httpapi.Request, id, aid int64) (ActionResult, error) {
		return l.ConfirmPendingAction(ctx(r), user(r), id, aid)
	})
	action("cancel", func(r *httpapi.Request, id, aid int64) (ActionResult, error) {
		return l.CancelPendingAction(ctx(r), user(r), id, aid)
	})
	rt.Handle("GET /settings/connections", func(r *httpapi.Request) (any, error) {
		return l.ConnectionSettings(ctx(r), user(r))
	})
	rt.Handle("PUT /settings/connections", func(r *httpapi.Request) (any, error) {
		var in ConnectionSettingsUpdate
		if err := r.Decode(&in); err != nil {
			return nil, err
		}
		return l.UpdateConnectionSettings(ctx(r), user(r), in)
	})
	rt.Handle("POST /settings/connections/test", func(r *httpapi.Request) (any, error) {
		var in ConnectionSettingsUpdate
		if err := r.DecodeOptional(&in); err != nil {
			return nil, err
		}
		return l.TestConnectionSettings(ctx(r), user(r), in)
	})
	rt.Handle("GET /settings/auto-forward", func(r *httpapi.Request) (any, error) {
		ss, err := l.AutoForwardSettings(ctx(r), user(r))
		return list(ss, err)
	})
	rt.Handle("PUT /settings/auto-forward", func(r *httpapi.Request) (any, error) {
		var in struct {
			Project string     `json:"project"`
			Type    TicketType `json:"type"`
			On      *bool      `json:"on"` // null 表示恢复代码默认值
		}
		if err := r.Decode(&in); err != nil {
			return nil, err
		}
		var err error
		if in.On == nil {
			err = l.ResetAutoForward(ctx(r), user(r), in.Project, in.Type)
		} else {
			err = l.SetAutoForward(ctx(r), user(r), in.Project, in.Type, *in.On)
		}
		return nil, err
	})
	return rt
}

// list 保证空列表编码为 [] 而不是 null。
func list[T any](v []T, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	if v == nil {
		v = []T{}
	}
	return v, nil
}

func restError(err error) (int, string) {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound, "not_found"
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest, "invalid_input"
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden, "forbidden"
	case errors.Is(err, ErrConflict):
		return http.StatusConflict, "conflict"
	case errors.Is(err, ErrNoModel):
		return http.StatusNotImplemented, "model_not_configured"
	}
	return 0, ""
}
