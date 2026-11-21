package worker

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// Issue 正文中需求视角区块的起止标记：区块由灵问单向维护，每次按库中数据整体重写，区块外的内容不动。
const (
	perspectivesStart = "<!-- lingo:perspectives -->"
	perspectivesEnd   = "<!-- /lingo:perspectives -->"
)

// perspectiveSection 按视角分组渲染需求视角区块（PRD 3.2.2.1）：每段注明补充人姓名与时间（姓名是"个人信息不外传"的
// 例外，邮箱与用户 ID 不写），首段原始提交已在正文中，只写"原始提交见正文"；缺少必填视角时在末尾标注。非需求工单为空。
func perspectiveSection(set *ticket.PerspectiveSet) string {
	if set == nil || set.Ticket == nil || set.Ticket.Type != ticket.TypeFeature {
		return ""
	}
	var b strings.Builder
	b.WriteString(perspectivesStart + "\n### 需求视角补充\n\n")
	for _, g := range set.Groups() {
		b.WriteString("#### " + ticket.PerspectiveSection(g.Perspective) + "\n\n")
		for _, it := range g.Items {
			at := it.At.UTC().Format("2006-01-02 15:04 UTC")
			if name := strings.TrimSpace(it.AuthorName); name != "" {
				at = escapeMarkdown(name) + " · " + at
			}
			b.WriteString("**" + at + "**\n\n")
			if it.Original {
				b.WriteString("原始提交见正文\n\n")
			} else {
				b.WriteString(quoteContent(it.Body) + "\n\n")
			}
		}
	}
	if len(set.Missing) > 0 {
		names := make([]string, len(set.Missing))
		for i, m := range set.Missing {
			names[i] = ticket.PerspectiveSection(m)
		}
		b.WriteString("> 缺少：" + strings.Join(names, "、") + "\n")
	}
	b.WriteString(perspectivesEnd)
	return b.String()
}

// quoteContent 把补充内容渲染为引用块，并转义行首的 #：补充人写的标题、署名或"缺少"行都留在引用里，
// 不能伪装成区块本身的视角标题与署名（区块结构只由灵问生成）。
func quoteContent(body string) string {
	lines := strings.Split(strings.TrimSpace(stripMarkers(body)), "\n")
	for i, l := range lines {
		l = strings.TrimRight(l, "\r")
		if t := strings.TrimLeft(l, " \t"); strings.HasPrefix(t, "#") {
			l = l[:len(l)-len(t)] + "\\" + t
		}
		if l == "" {
			lines[i] = ">"
		} else {
			lines[i] = "> " + l
		}
	}
	return strings.Join(lines, "\n")
}

// withPerspectives 把视角区块写进 Issue 正文：替换已有区块；没有时（含被人删掉）插在工单标记之前（工单标记须是正文最后一个）。
// 只剩起始标记（结束标记被删）时去掉残留的起始标记再插入，避免每次重写都多出一段。
func withPerspectives(body, section string) string {
	if section == "" {
		return body
	}
	return withBlock(body, perspectivesStart, perspectivesEnd, section)
}

// withBlock 用 section 替换正文中 start…end 之间的区块（含标记）；没有完整区块时插在最后一个工单标记之前，
// 保证工单标记仍是正文最后一个。
func withBlock(body, start, end, section string) string {
	if i := strings.Index(body, start); i >= 0 {
		if j := strings.Index(body[i:], end); j >= 0 {
			return body[:i] + section + body[i+j+len(end):]
		}
		body = body[:i] + body[i+len(start):]
	}
	if i := strings.LastIndex(body, "<!-- lingo:ticket="); i >= 0 {
		return strings.TrimRight(body[:i], "\n") + "\n\n" + section + "\n\n" + body[i:]
	}
	return strings.TrimRight(body, "\n") + "\n\n" + section
}

// perspectiveLabels 是 Issue 上应有的视角标签：出现过的视角各一个。
func perspectiveLabels(prefix string, set *ticket.PerspectiveSet) []gitea.CreateLabelOption {
	if set == nil || set.Ticket == nil || set.Ticket.Type != ticket.TypeFeature {
		return nil
	}
	var out []gitea.CreateLabelOption
	for _, p := range set.Present() {
		out = append(out, gitea.PerspectiveLabel(prefix, p))
	}
	return out
}

// missingLabels 返回 want 中 Issue 上还没有的标签（按名称，不区分大小写）；都有时不必再调用加标签接口。
func missingLabels(is *gitea.Issue, want []gitea.CreateLabelOption) []gitea.CreateLabelOption {
	var out []gitea.CreateLabelOption
	for _, l := range want {
		if !slices.ContainsFunc(is.Labels, func(have *gitea.Label) bool { return have != nil && strings.EqualFold(have.Name, l.Name) }) {
			out = append(out, l)
		}
	}
	return out
}

// perspectiveNotice 是转出后追加补充时机器人发到 Issue 的提示评论。
func perspectiveNotice(it ticket.Perspective) string {
	return "**" + escapeMarkdown(ticket.PerspectiveNotice(it.Perspective, it.AuthorName)) + "**，见正文「需求视角补充」。"
}

// syncPerspectives 把需求视角补充同步到 Issue：按库中最新数据重写正文区块（无变化不提交）、补打视角标签，
// 并为这段补充发一条提示评论（已发过的不重发）。正文只由灵问单向写入，不回读，Webhook 的 edited 事件不会触发本任务。
func (w *Worker) syncPerspectives(ctx context.Context, ticketID, perspectiveID int64) error {
	set, err := w.o.Drafts.Perspectives(ctx, ticketID)
	if errors.Is(err, ticket.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	t := set.Ticket
	if t.Status == ticket.StatusRejected {
		return nil // 已驳回：驳回前排队的视角同步不再改 Issue 正文与标签，补充留在灵问
	}
	if t.Type != ticket.TypeFeature {
		return nil // 转出时改成了其他类型：补充留在灵问
	}
	if t.IssueNumber == 0 {
		if t.Status == ticket.StatusSubmitted {
			return &ticket.DeferError{Until: w.o.Now().Add(commentWait), Reason: "issue not created yet"}
		}
		return nil
	}
	owner, name, _ := strings.Cut(t.Repo, "/")
	is, err := w.o.Issues.GetIssue(ctx, owner, name, t.IssueNumber)
	if errors.Is(err, gitea.ErrNotFound) {
		return nil // Issue 已被删除：工单由 Webhook / 对账标记为 abnormal
	}
	if err != nil {
		return giteaErr("get issue", err)
	}
	if body := withPerspectives(is.Body, perspectiveSection(set)); body != is.Body {
		if _, err := w.o.Issues.EditIssue(ctx, owner, name, t.IssueNumber, gitea.EditIssueOption{Body: &body}); err != nil {
			return giteaErr("edit issue body", err)
		}
	}
	if err := w.addLabels(ctx, owner, name, t.IssueNumber, missingLabels(is, perspectiveLabels(w.o.LabelPrefix, set))); err != nil {
		return err
	}
	for _, it := range set.Items {
		if it.ID != perspectiveID || it.PostedID != 0 {
			continue
		}
		gc, err := w.o.Issues.CreateComment(ctx, owner, name, t.IssueNumber, perspectiveNotice(it))
		if err != nil {
			return giteaErr("create comment", err)
		}
		return w.o.Forwarding.PerspectivePosted(ctx, it.ID, gc.ID)
	}
	return nil
}

// addLabels 给 Issue 追加标签（缺的先创建）；缓存的标签 ID 失效（标签被删除重建）时刷新后再试一次。
func (w *Worker) addLabels(ctx context.Context, owner, name string, number int64, want []gitea.CreateLabelOption) error {
	if len(want) == 0 {
		return nil
	}
	for attempt := 0; ; attempt++ {
		ids, err := w.labels.ensure(ctx, w.o.Issues, owner, name, want)
		if err != nil {
			return err
		}
		_, err = w.o.Issues.AddIssueLabels(ctx, owner, name, number, ids)
		var ae *gitea.APIError
		stale := errors.Is(err, gitea.ErrNotFound) || (errors.As(err, &ae) && ae.StatusCode == http.StatusUnprocessableEntity)
		switch {
		case err == nil:
			return nil
		case stale && attempt == 0:
			w.labels.forget(owner, name)
		default:
			return giteaErr("add issue labels", err)
		}
	}
}
