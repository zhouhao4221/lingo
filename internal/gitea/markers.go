package gitea

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 灵工与 Gitea 之间的文本约定（PRD 3.2.2）：
//   - Issue 正文末尾的隐藏标记 <!-- lingo:ticket=<id> --> 关联工单，也用于 outbox 重试前查重；
//   - 研发评论以 /reply 开头或含 <!-- lingo:public --> 表示面向提交人；
//   - PR 正文 / 提交信息中的 Fixes owner/repo#N 等关闭关键词表示修复。

// PublicMarker 是面向提交人的评论标记，灵工代研发发出的对外回复会自动带上。
const PublicMarker = "<!-- lingo:public -->"

// ReplyCommand 是研发在评论开头声明“回复提交人”的命令。
const ReplyCommand = "/reply"

var ticketMarkerRe = regexp.MustCompile(`<!--\s*lingo:ticket=(\d+)\s*-->`)

// TicketMarker 返回写在 Issue 正文中的工单隐藏标记。
func TicketMarker(ticketID int64) string {
	return fmt.Sprintf("<!-- lingo:ticket=%d -->", ticketID)
}

// TicketMarkerQuery 返回用于 SearchIssues 查重的关键词。搜索是关键词匹配，"lingo:ticket=5" 也会命中
// "lingo:ticket=50"；调用方必须再用 HasTicketMarker 逐个确认，且只认机器人创建的 Issue。
func TicketMarkerQuery(ticketID int64) string {
	return fmt.Sprintf("lingo:ticket=%d", ticketID)
}

// HasTicketMarker 报告正文中是否恰好含有 ticketID 的标记（精确匹配）。
func HasTicketMarker(body string, ticketID int64) bool {
	for _, m := range ticketMarkerRe.FindAllStringSubmatch(body, -1) {
		if id, _ := strconv.ParseInt(m[1], 10, 64); id == ticketID {
			return true
		}
	}
	return false
}

// ParseTicketMarker 从 Issue 正文中取出工单 ID；没有标记时返回 0。
// 有多个标记时取最后一个（灵工追加在正文末尾，研发引用的旧标记在前）。
func ParseTicketMarker(body string) int64 {
	ms := ticketMarkerRe.FindAllStringSubmatch(body, -1)
	if len(ms) == 0 {
		return 0
	}
	id, _ := strconv.ParseInt(ms[len(ms)-1][1], 10, 64)
	return id
}

// IsPublicReply 报告评论是否在 marked 模式下面向提交人：以 /reply 开头（单独成词）或含 PublicMarker。
func IsPublicReply(body string) bool {
	if strings.Contains(body, PublicMarker) {
		return true
	}
	return hasReplyCommand(strings.TrimLeft(body, " \t\r\n"))
}

func hasReplyCommand(s string) bool {
	if !strings.HasPrefix(s, ReplyCommand) {
		return false
	}
	rest := s[len(ReplyCommand):]
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return strings.ContainsRune(" \t\r\n:：", r)
}

// PublicReplyText 去掉 /reply 命令与 PublicMarker，返回给提交人看的正文。
func PublicReplyText(body string) string {
	s := strings.ReplaceAll(body, PublicMarker, "")
	t := strings.TrimLeft(s, " \t\r\n")
	if hasReplyCommand(t) {
		t = strings.TrimLeft(t[len(ReplyCommand):], " \t\r\n")
		t = strings.TrimPrefix(t, ":")
		t = strings.TrimPrefix(t, "：")
		s = t
	}
	return strings.TrimSpace(s)
}

// RefAction 是 Issue 引用的动作。
type RefAction string

const (
	RefMention RefAction = ""        // 仅提及
	RefCloses  RefAction = "closes"  // 关闭关键词
	RefReopens RefAction = "reopens" // 重开关键词
)

// IssueRef 是文本中对 Issue 的引用。Owner / Repo 为空表示同一仓库。
type IssueRef struct {
	Owner  string
	Repo   string
	Index  int64
	Action RefAction
}

// DefaultCloseKeywords / DefaultReopenKeywords 是 Gitea [repository.pull-request] 的默认关键词。
var (
	DefaultCloseKeywords  = []string{"close", "closes", "closed", "fix", "fixes", "fixed", "resolve", "resolves", "resolved"}
	DefaultReopenKeywords = []string{"reopen", "reopens", "reopened"}
)

// issueRefRe 匹配 #12、owner/repo#12，前面可带关键词与可选冒号；引用前须为行首、空白或括号。
var issueRefRe = regexp.MustCompile(`(?i)(?:^|[\s(\[])(?:([a-z]+):?\s+)?((?:[\w.-]+/[\w.-]+)?#(\d+))\b`)

// ParseIssueRefs 解析文本中的 Issue 引用，按 Gitea 默认关键词识别关闭 / 重开；同一 Issue 只保留一次，
// 动作优先级：关闭 > 重开 > 提及。代码块与行内代码中的引用会被忽略。
func ParseIssueRefs(text string) []IssueRef {
	return parseIssueRefs(text, DefaultCloseKeywords, DefaultReopenKeywords)
}

func parseIssueRefs(text string, closeKW, reopenKW []string) []IssueRef {
	text = stripCode(text)
	type key struct {
		owner, repo string
		index       int64
	}
	order := []key{}
	seen := map[key]RefAction{}
	for _, m := range issueRefRe.FindAllStringSubmatch(text, -1) {
		kw, ref, num := strings.ToLower(m[1]), m[2], m[3]
		idx, err := strconv.ParseInt(num, 10, 64)
		if err != nil || idx <= 0 {
			continue
		}
		k := key{index: idx}
		if i := strings.IndexByte(ref, '#'); i > 0 {
			k.owner, k.repo, _ = strings.Cut(ref[:i], "/")
		}
		act := RefMention
		switch {
		case contains(closeKW, kw):
			act = RefCloses
		case contains(reopenKW, kw):
			act = RefReopens
		}
		prev, ok := seen[k]
		if !ok {
			order = append(order, k)
		}
		if !ok || rank(act) > rank(prev) {
			seen[k] = act
		}
	}
	out := make([]IssueRef, 0, len(order))
	for _, k := range order {
		out = append(out, IssueRef{Owner: k.owner, Repo: k.repo, Index: k.index, Action: seen[k]})
	}
	return out
}

func rank(a RefAction) int {
	switch a {
	case RefCloses:
		return 2
	case RefReopens:
		return 1
	}
	return 0
}

var (
	fencedCodeRe = regexp.MustCompile("(?s)```.*?(```|$)")
	inlineCodeRe = regexp.MustCompile("`[^`\n]*`")
)

func stripCode(s string) string {
	s = fencedCodeRe.ReplaceAllString(s, " ")
	return inlineCodeRe.ReplaceAllString(s, " ")
}
