package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/store"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// 转研发后驳回（REQ-002，待办 #698）：驳回原因评论、补充评论外发仲裁、关闭失败原因脱敏。

// closePayload 是 close_issue 任务载荷中驳回原因与驳回人姓名（取自审计 ticket.rejected）；早期版本入队的任务没有。
type closePayload struct {
	Reason     string `json:"reason"`
	RejectedBy string `json:"rejected_by"`
}

// RejectMarker 是驳回原因评论的隐藏标记，用于去重：同一工单在其 Issue 上只发一条。
func RejectMarker(ticketID int64) string {
	return fmt.Sprintf("<!-- lingo:reject=%d -->", ticketID)
}

// rejectComment 是机器人发到 Issue 的驳回原因评论：只署驳回人姓名（未记录姓名时不署名，不以用户 ID 代替），
// 不写提交人的姓名、邮箱或用户 ID；原因中用户写入的灵工标记会被去掉。
func rejectComment(ticketID int64, p closePayload) string {
	head := "**灵工：该工单已驳回，关闭本 Issue。**"
	if by := strings.TrimSpace(p.RejectedBy); by != "" {
		head = "**灵工：该工单已由 " + escapeMarkdown(by) + " 驳回，关闭本 Issue。**"
	}
	return head + "\n\n驳回原因：\n\n" + quoteContent(p.Reason) + "\n\n" + RejectMarker(ticketID)
}

// postRejectComment 发驳回原因评论；该 Issue 上已有机器人发出的同一标记评论时不再发（评论接口不分页，返回全部评论）。
func (w *Worker) postRejectComment(ctx context.Context, t *ticket.Ticket, p closePayload) error {
	bot := w.o.BotLogin()
	if bot == "" {
		return &ticket.DeferError{Until: w.o.Now().Add(botRetry), Reason: "bot login not yet known"}
	}
	owner, name, _ := strings.Cut(t.Repo, "/")
	cs, err := w.o.Issues.ListIssueComments(ctx, owner, name, t.IssueNumber, time.Time{})
	if errors.Is(err, gitea.ErrNotFound) {
		return issueNotFound(t, err)
	}
	if err != nil {
		return giteaErr("list issue comments", err)
	}
	marker := RejectMarker(t.ID)
	for _, c := range cs {
		if c != nil && c.User != nil && strings.EqualFold(c.User.Login, bot) && strings.Contains(c.Body, marker) {
			return nil
		}
	}
	if _, err := w.o.Issues.CreateComment(ctx, owner, name, t.IssueNumber, rejectComment(t.ID, p)); err != nil {
		if errors.Is(err, gitea.ErrNotFound) {
			return issueNotFound(t, err)
		}
		return giteaErr("create comment", err)
	}
	return nil
}

// sendClaimMargin 是发送租约在任务截止时间之外的余量：覆盖截止前刚发出的请求与进程间的计时差。
const sendClaimMargin = 10 * time.Second

// claimCommentSend 为补充评论取得发送租约。sent 为 false 表示工单已驳回、评论不发（已留在灵工并记审计），任务记为完成；
// sent 为 true 时发送结束后须调用 release。
//
// 租约时长 = 本次任务 ctx 的剩余时限 + sendClaimMargin：任务内的所有 Gitea 请求（评论、附件上传、改评论）都用这个 ctx，
// 截止后不会再发出新请求，所以租约过期时本次发送必然已经结束（进程崩溃时租约到期自然失效）。驳回在租约有效期内不提交；
// 驳回先提交时这里返回 false，一个请求也不发。已经发出的请求不追撤。
func (w *Worker) claimCommentSend(ctx context.Context, ticketID, commentID int64) (sent bool, release func(), err error) {
	hold := w.o.TaskTimeout
	if dl, ok := ctx.Deadline(); ok {
		hold = time.Until(dl)
	}
	hold += sendClaimMargin
	if sent, err = w.o.Drafts.ClaimCommentSend(ctx, ticketID, commentID, hold); err != nil || !sent {
		if err == nil {
			w.log.Info("comment withheld: ticket rejected", "ticket", ticketID, "comment", commentID)
		}
		return false, nil, err
	}
	return true, func() {
		fctx, cancel := finishContext(ctx)
		defer cancel()
		if err := w.o.Drafts.ReleaseCommentSend(fctx, ticketID, commentID); err != nil {
			w.log.Warn("release comment send claim failed, it expires on its own", "ticket", ticketID, "comment", commentID, "err", err)
		}
	}, nil
}

// taskError 是写入 outbox last_error 的失败原因。close_issue 的原因经 TicketIssueClose 给后台人员查看，只写脱敏说明
// （类别与 HTTP 状态码，不含地址、令牌、响应正文），原始错误只记日志；其他任务原样保留。
func taskError(t store.Task, err error, final bool) string {
	if t.Op != ticket.OpCloseIssue {
		return err.Error()
	}
	return closeFailure(err, final)
}

// closeFailure 把关闭失败转成脱敏说明；final 为假表示还会自动重试。
func closeFailure(err error, final bool) string {
	var ae *gitea.APIError
	retrying := ""
	if !final {
		retrying = "，将自动重试"
	}
	switch {
	case errors.Is(err, gitea.ErrRedirect):
		code := 0
		var re *gitea.RedirectError
		if errors.As(err, &re) {
			code = re.StatusCode
		}
		return fmt.Sprintf("Gitea 返回了重定向（HTTP %d），已按配置拒绝跟随，请核对 Gitea 地址", code)
	case errors.Is(err, gitea.ErrNotFound):
		return "Issue 不存在或机器人无权访问（HTTP 404），无法确认已关闭"
	case errors.As(err, &ae) && (ae.StatusCode == http.StatusUnauthorized || ae.StatusCode == http.StatusForbidden):
		return fmt.Sprintf("机器人账号无权评论或关闭该 Issue（HTTP %d）", ae.StatusCode)
	case errors.As(err, &ae) && gitea.IsRetryable(err) && final:
		return fmt.Sprintf("Gitea 暂时不可用，自动重试已用尽（HTTP %d）", ae.StatusCode)
	case errors.As(err, &ae) && gitea.IsRetryable(err):
		return fmt.Sprintf("Gitea 暂时不可用（HTTP %d）%s", ae.StatusCode, retrying)
	case errors.As(err, &ae):
		return fmt.Sprintf("Gitea 拒绝了操作（HTTP %d）", ae.StatusCode)
	case gitea.IsRetryable(err) && final:
		return "无法连接 Gitea，自动重试已用尽"
	case gitea.IsRetryable(err):
		return "无法连接 Gitea" + retrying
	}
	return "关闭 Issue 失败" + retrying
}
