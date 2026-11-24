package assistant

import (
	"fmt"
	"github.com/zhouhao4221/lingo/internal/knowledge"
	"strings"
	"time"

	"github.com/zhouhao4221/lingo/internal/release"
	"github.com/zhouhao4221/lingo/internal/textutil"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

// 固定回复：这些情形不调用模型。
const (
	textNotFound    = "没有找到相关的工单。如果要反馈新问题，可以提交一个工单。"
	textNoRelease   = "暂无符合条件的发版记录。"
	textOutOfScope  = "目前我只能回答工单和版本相关的问题，比如工单进展、修复版本、最近发版。其他问题可以提交一个咨询工单，由研发人员答复。"
	textClarify     = "没太理解你的问题，能换个说法，或者说明是哪个工单、哪个项目吗？"
	textNoAction    = "当前没有待确认的操作。"
	textExpired     = "这个操作已经过期（超过 30 分钟未确认），没有执行。需要的话请重新发起。"
	textCancelled   = "好的，已取消，没有执行。"
	textNeedTarget  = "要在哪个工单留言？请带上工单号，例如：在 #12 留言：复现环境是 Chrome 120。"
	textNeedBody    = "请直接给出要留言的原文，例如：在 #12 留言：复现环境是 Chrome 120。"
	textNoKnowledge = "知识库里还没有相关内容。可以提交一个咨询工单，由后台人员答复。"
	textReadOnly    = "这是同事提交的工单，你可以查看进展，但只能对自己提交的工单留言。"
)

var statusNames = map[string]string{
	string(ticket.StatusPendingTriage): "待后台确认",
	string(ticket.StatusAnswered):      "已答复",
	string(ticket.StatusMerged):        "已合并",
	string(ticket.StatusRejected):      "已驳回",
	string(ticket.StatusSubmitted):     "已确认，待建 Issue",
	string(ticket.StatusOpen):          "已转研发",
	string(ticket.StatusInProgress):    "处理中",
	string(ticket.StatusNeedInfo):      "需补充信息",
	string(ticket.StatusFixed):         "已修复，待发版",
	string(ticket.StatusReleased):      "已发布",
	string(ticket.StatusDeployed):      "已上线",
	string(ticket.StatusClosed):        "已关闭（未修复）",
	string(ticket.StatusAbnormal):      "异常",
}

var typeNames = map[string]string{ticket.TypeConsult: "咨询", ticket.TypeBug: "Bug", ticket.TypeFeature: "需求"}

func statusName(s string) string {
	if n, ok := statusNames[s]; ok {
		return n
	}
	return s
}

func typeName(t string) string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return t
}

func ticketLine(t *ticket.Ticket) string {
	return fmt.Sprintf("- #%d %s（%s · %s）", t.ID, oneLine(t.Title), typeName(t.Type), statusName(string(t.Status)))
}

func candidatesText(ts []*ticket.Ticket, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "找到 %d 张可能相关的工单，你问的是哪一张？\n\n", total)
	for _, t := range ts {
		b.WriteString(ticketLine(t) + "\n")
	}
	if total > len(ts) {
		fmt.Fprintf(&b, "\n以上为最近的 %d 张，可以补充时间、类型或关键词缩小范围。", len(ts))
	}
	return strings.TrimSpace(b.String())
}

// knowledgeText 是知识问答的确定性回答（模型输出不可用时）：列出检索到的条目与同类工单。
func knowledgeText(hits []knowledge.Hit, ts []*ticket.Ticket, loc *time.Location) string {
	var b strings.Builder
	if len(hits) > 0 {
		b.WriteString("知识库中的相关内容：\n\n")
		for _, h := range hits {
			b.WriteString(fmt.Sprintf("- K%d %s：%s\n", h.ID, oneLine(h.Question), textutil.ClipEllipsis(oneLine(h.Answer), 200)))
		}
	}
	if len(ts) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("相关工单：\n\n" + ticketsText(ts, loc))
	}
	return strings.TrimSpace(b.String())
}

// ticketsText 是模型回答不可用时，按事实直接列出工单的回答。
func ticketsText(ts []*ticket.Ticket, loc *time.Location) string {
	var b strings.Builder
	for _, t := range ts {
		b.WriteString(ticketLine(t))
		if t.FixedTag != "" {
			b.WriteString("，修复版本 " + t.FixedTag)
		}
		if t.DeployedAt != nil {
			b.WriteString("，" + t.DeployedAt.In(loc).Format("2006-01-02") + " 上线")
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func releasesText(rs []*release.Detail, loc *time.Location) string {
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, "- %s %s（%s 发布）", r.Repo, r.Tag, r.PublishedAt.In(loc).Format("2006-01-02"))
		if len(r.Tickets) > 0 {
			ids := make([]string, 0, len(r.Tickets))
			for _, t := range r.Tickets {
				ids = append(ids, fmt.Sprintf("#%d %s", t.ID, oneLine(t.Title)))
			}
			b.WriteString("，修复：" + strings.Join(ids, "、"))
		}
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

func proposeText(t *ticket.Ticket, body string) string {
	return fmt.Sprintf("将在 #%d「%s」留言：\n\n> %s\n\n确认请回复“确认”或点击确认按钮（30 分钟内有效），取消请回复“取消”。",
		t.ID, oneLine(t.Title), strings.ReplaceAll(body, "\n", "\n> "))
}

func actionSummary(t *ticket.Ticket, body string) string {
	return fmt.Sprintf("在 #%d「%s」留言：%s", t.ID, textutil.Clip(oneLine(t.Title), 60), textutil.ClipEllipsis(oneLine(body), 200))
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
