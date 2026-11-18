package webhook

import (
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
)

const repoJSON = `"repository":{"id":7,"name":"api","full_name":"demo/api","owner":{"login":"demo"}}`

func delivery(eventType, body string) *gitea.Delivery {
	return &gitea.Delivery{ID: "d-1", Event: eventType, EventType: eventType, Body: []byte(body), ReceivedAt: time.Unix(0, 0)}
}

func mustParse(t *testing.T, eventType, body string, o ParseOptions) *Event {
	t.Helper()
	e, err := Parse(delivery(eventType, body), o)
	if err != nil {
		t.Fatalf("Parse(%s): %v", eventType, err)
	}
	return e
}

func issueBody(action, labels, extra string) string {
	return `{"action":"` + action + `","number":12,"issue":{"id":100,"number":12,"title":"导出失败",
	  "body":"### 现象\n\n导出失败\n\n<!-- lingo:ticket=42 -->","state":"open","user":{"login":"lingo-bot"},"labels":[` + labels + `],
	  "updated_at":"2026-11-18T08:00:00+08:00"` + extra + `},` + repoJSON + `,"sender":{"login":"Dev-Agent"}}`
}

func TestParseIssueEvents(t *testing.T) {
	cases := []struct {
		eventType, action string
		want              Kind
	}{
		{gitea.EventIssues, "opened", IssueOpened},
		{gitea.EventIssues, "closed", IssueClosed},
		{gitea.EventIssues, "reopened", IssueReopened},
		{gitea.EventIssues, "deleted", IssueDeleted},
		{gitea.EventIssueAssign, "assigned", IssueAssigned},
		{gitea.EventIssueLabel, "label_updated", IssueLabeled},
		{gitea.EventIssueLabel, "label_cleared", IssueLabeled},
		{gitea.EventIssueMilestone, "milestoned", IssueMilestoned},
		{gitea.EventIssues, "synchronized", Ignored},
	}
	for _, c := range cases {
		e := mustParse(t, c.eventType, issueBody(c.action, "", ""), ParseOptions{BotLogin: "lingo-bot"})
		if e.Kind != c.want {
			t.Errorf("%s/%s: Kind = %s, want %s", c.eventType, c.action, e.Kind, c.want)
			continue
		}
		if c.want == Ignored {
			continue
		}
		if e.Repo != "demo/api" || e.IssueNumber != 12 || e.MarkerTicketID != 42 || e.Sender != "Dev-Agent" || e.FromBot {
			t.Errorf("%s: event = %+v", c.action, e)
		}
		if !e.At.Equal(time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("%s: At = %v", c.action, e.At)
		}
		wantKey := "issue:100:" + string(c.want)
		if c.want != IssueOpened && c.want != IssueDeleted {
			wantKey += ":2026-11-18T00:00:00Z"
		}
		if !strings.HasPrefix(e.DedupKey, wantKey) || (c.want == IssueOpened && e.DedupKey != wantKey) {
			t.Errorf("%s: DedupKey = %q, want prefix %q", c.action, e.DedupKey, wantKey)
		}
	}
}

func TestIssueDedupKeyDistinguishesSameSecondChanges(t *testing.T) {
	a := mustParse(t, gitea.EventIssueLabel, issueBody("label_updated", `{"id":3,"name":"type/bug"}`, ""), ParseOptions{})
	b := mustParse(t, gitea.EventIssueLabel, issueBody("label_updated", `{"id":3,"name":"type/bug"},{"id":9,"name":"review/approved"}`, ""), ParseOptions{})
	if a.DedupKey == b.DedupKey {
		t.Errorf("different label sets share key %q", a.DedupKey)
	}
	replay := mustParse(t, gitea.EventIssueLabel, issueBody("label_updated", `{"id":9},{"id":3}`, ""), ParseOptions{})
	if replay.DedupKey != b.DedupKey {
		t.Errorf("label order should not matter: %q vs %q", replay.DedupKey, b.DedupKey)
	}
	m1 := mustParse(t, gitea.EventIssueMilestone, issueBody("milestoned", "", `,"milestone":{"id":1}`), ParseOptions{})
	m2 := mustParse(t, gitea.EventIssueMilestone, issueBody("milestoned", "", `,"milestone":{"id":2}`), ParseOptions{})
	if m1.DedupKey == m2.DedupKey {
		t.Error("different milestones share key")
	}
}

func commentBody(action, body, eventExtra string) string {
	return `{"action":"` + action + `","issue":{"id":100,"number":12,"user":{"login":"lingo-bot"},"body":"x <!-- lingo:ticket=42 -->"},
	  "comment":{"id":99,"body":"` + body + `","updated_at":"2026-11-18T01:00:00Z","issue_url":"https://g/api/v1/repos/demo/api/issues/12"},
	  ` + repoJSON + `,"sender":{"login":"dev-agent"}` + eventExtra + `}`
}

func TestParseCommentPublicity(t *testing.T) {
	pub := mustParse(t, gitea.EventIssueComment, commentBody("created", "/reply 已修复", ""), ParseOptions{BotLogin: "lingo-bot"})
	if pub.Kind != CommentCreated || !pub.Public || pub.MarkerTicketID != 42 || pub.Comment.ID != 99 || pub.IsPull {
		t.Errorf("public comment = %+v", pub)
	}
	internal := mustParse(t, gitea.EventIssueComment, commentBody("created", "先看下日志", ""), ParseOptions{})
	if internal.Public {
		t.Error("internal comment marked public in marked mode")
	}
	all := mustParse(t, gitea.EventIssueComment, commentBody("created", "先看下日志", ""), ParseOptions{ReplyMode: ReplyAll})
	if !all.Public {
		t.Error("ReplyAll should make every comment public")
	}
	if pub.DedupKey != "comment:99:comment.created" {
		t.Errorf("DedupKey = %q", pub.DedupKey)
	}
	del := mustParse(t, gitea.EventIssueComment, commentBody("deleted", "/reply 已修复", ""), ParseOptions{})
	if del.Kind != CommentDeleted || del.DedupKey == pub.DedupKey {
		t.Errorf("deleted = %s %q", del.Kind, del.DedupKey)
	}
}

func TestParsePullComment(t *testing.T) {
	e := mustParse(t, gitea.EventPullComment, commentBody("created", "LGTM", `,"is_pull":true`), ParseOptions{})
	if e.Kind != CommentCreated || !e.IsPull {
		t.Errorf("pull comment = %+v", e)
	}
}

func TestParseFromBot(t *testing.T) {
	e := mustParse(t, gitea.EventIssueComment, commentBody("created", "来自提交人的补充", ""), ParseOptions{BotLogin: "DEV-AGENT"})
	if !e.FromBot {
		t.Error("bot login should match case-insensitively")
	}
}

func pullBody(action string, merged bool) string {
	m := "false"
	if merged {
		m = "true"
	}
	return `{"action":"` + action + `","number":3,"pull_request":{"id":300,"number":3,"title":"修复导出",
	  "body":"Fixes #12\nRelated demo/web#4","merged":` + m + `,"merge_commit_sha":"abc","updated_at":"2026-11-18T02:00:00Z"},
	  ` + repoJSON + `,"sender":{"login":"dev-agent"}}`
}

func TestParsePull(t *testing.T) {
	merged := mustParse(t, gitea.EventPullRequest, pullBody("closed", true), ParseOptions{})
	if merged.Kind != PullMerged || merged.PullRequest.MergeCommitSHA != "abc" || !merged.IsPull {
		t.Fatalf("merged = %+v", merged)
	}
	if len(merged.Refs) != 2 || merged.Refs[0].Index != 12 || merged.Refs[0].Action != gitea.RefCloses ||
		merged.Refs[1].Repo != "web" || merged.Refs[1].Action != gitea.RefMention {
		t.Errorf("refs = %+v", merged.Refs)
	}
	if k := mustParse(t, gitea.EventPullRequest, pullBody("closed", false), ParseOptions{}).Kind; k != PullClosed {
		t.Errorf("unmerged close = %s", k)
	}
	if k := mustParse(t, gitea.EventPullRequest, pullBody("opened", false), ParseOptions{}).Kind; k != PullOpened {
		t.Errorf("opened = %s", k)
	}
	if k := mustParse(t, gitea.EventPullRequest, pullBody("synchronized", false), ParseOptions{}).Kind; k != Ignored {
		t.Errorf("synchronized = %s", k)
	}
}

func releaseBody(action, notes string) string {
	return `{"action":"` + action + `","release":{"id":5,"tag_name":"v1.3.0","name":"v1.3.0","body":"` + notes + `",
	  "published_at":"2026-11-18T03:00:00Z"},` + repoJSON + `,"sender":{"login":"rel"}}`
}

func TestParseRelease(t *testing.T) {
	pub := mustParse(t, gitea.EventRelease, releaseBody("published", "修复导出"), ParseOptions{})
	if pub.Kind != ReleasePublished || pub.Release.TagName != "v1.3.0" || pub.Repo != "demo/api" {
		t.Fatalf("release = %+v", pub)
	}
	if pub.DedupKey != "release:5:release.published" {
		t.Errorf("published key = %q", pub.DedupKey)
	}
	// 更新无法与重投区分，只按 delivery 去重
	u := mustParse(t, gitea.EventRelease, releaseBody("updated", "修复导出"), ParseOptions{})
	if u.Kind != ReleaseUpdated || u.DedupKey != "delivery:d-1" {
		t.Errorf("updated key = %q", u.DedupKey)
	}
}

func TestParseOtherEvents(t *testing.T) {
	tag := mustParse(t, gitea.EventCreate, `{"ref":"v1.3.0","ref_type":"tag","sha":"abc",`+repoJSON+`}`, ParseOptions{})
	if tag.Kind != TagCreated || tag.Ref != "v1.3.0" || tag.DedupKey != "tag:demo/api:v1.3.0:abc" {
		t.Errorf("tag = %+v", tag)
	}
	if k := mustParse(t, gitea.EventCreate, `{"ref":"feat/x","ref_type":"branch",`+repoJSON+`}`, ParseOptions{}).Kind; k != Ignored {
		t.Errorf("branch create = %s", k)
	}
	st := mustParse(t, gitea.EventStatus, `{"id":8,"sha":"abc","context":"deploy/production","state":"success",`+repoJSON+`}`, ParseOptions{})
	if st.Kind != StatusChanged || st.Status.Context != "deploy/production" || st.SHA != "abc" {
		t.Errorf("status = %+v", st)
	}
	run := `{"action":"%s","workflow_run":{"id":11,"path":".gitea/workflows/deploy-prod.yml@refs/tags/v1.3.0","conclusion":"success",
	  "head_sha":"abc","run_attempt":1,"completed_at":"2026-11-18T04:00:00Z"},` + repoJSON + `}`
	done := mustParse(t, gitea.EventWorkflowRun, strings.Replace(run, "%s", "completed", 1), ParseOptions{})
	if done.Kind != WorkflowRunCompleted || done.WorkflowRun.WorkflowFile() != "deploy-prod.yml" || done.SHA != "abc" {
		t.Errorf("run = %+v", done)
	}
	if k := mustParse(t, gitea.EventWorkflowRun, strings.Replace(run, "%s", "in_progress", 1), ParseOptions{}).Kind; k != Ignored {
		t.Errorf("in_progress = %s", k)
	}
	ping := mustParse(t, gitea.EventPush, `{"ref":"refs/heads/main",`+repoJSON+`}`, ParseOptions{})
	if ping.Kind != Ping || ping.Repo != "demo/api" {
		t.Errorf("ping = %+v", ping)
	}
	if k := mustParse(t, "wiki", `{}`, ParseOptions{}).Kind; k != Ignored {
		t.Errorf("wiki = %s", k)
	}
}

func TestParseErrors(t *testing.T) {
	for _, c := range []struct{ eventType, body string }{
		{gitea.EventIssues, `not json`},
		{gitea.EventIssues, `{"action":"opened"}`},
		{gitea.EventIssueComment, `{"action":"created","issue":{"number":1}}`},
		{gitea.EventPullRequest, `{"action":"opened"}`},
		{gitea.EventRelease, `{"action":"published"}`},
		{gitea.EventWorkflowRun, `{"action":"completed"}`},
	} {
		if _, err := Parse(delivery(c.eventType, c.body), ParseOptions{}); err == nil {
			t.Errorf("%s %s: want error", c.eventType, c.body)
		}
	}
}

func TestMarkerOnlyTrustedOnBotIssues(t *testing.T) {
	forged := strings.Replace(issueBody("edited", "", ""), `"user":{"login":"lingo-bot"}`, `"user":{"login":"mallory"}`, 1)
	if e := mustParse(t, gitea.EventIssues, forged, ParseOptions{BotLogin: "lingo-bot"}); e.MarkerTicketID != 0 {
		t.Errorf("marker on non-bot issue trusted: %d", e.MarkerTicketID)
	}
	if e := mustParse(t, gitea.EventIssues, issueBody("edited", "", ""), ParseOptions{}); e.MarkerTicketID != 0 {
		t.Errorf("marker trusted without bot login: %d", e.MarkerTicketID)
	}
}

func TestCreateKeyStableAcrossEdits(t *testing.T) {
	// 评论创建后被编辑：对账拿到的是新的 updated_at，重建的创建事件键必须与原来一致
	a := mustParse(t, gitea.EventIssueComment, commentBody("created", "x", ""), ParseOptions{})
	b := mustParse(t, gitea.EventIssueComment, strings.Replace(commentBody("created", "x 已编辑", ""), "2026-11-18T01:00:00Z", "2026-11-18T05:00:00Z", 1), ParseOptions{})
	if a.DedupKey != b.DedupKey {
		t.Errorf("create keys differ: %q vs %q", a.DedupKey, b.DedupKey)
	}
	e1 := mustParse(t, gitea.EventIssueComment, commentBody("edited", "v1", ""), ParseOptions{})
	e2 := mustParse(t, gitea.EventIssueComment, commentBody("edited", "v2", ""), ParseOptions{})
	if e1.DedupKey == e2.DedupKey {
		t.Error("different edits share key")
	}
}

func TestZeroUpdatedAtFallsBackToDelivery(t *testing.T) {
	body := strings.Replace(issueBody("closed", "", ""), `,
	  "updated_at":"2026-11-18T08:00:00+08:00"`, "", 1)
	e := mustParse(t, gitea.EventIssues, body, ParseOptions{})
	if e.DedupKey != "delivery:d-1" {
		t.Errorf("DedupKey = %q, want delivery fallback", e.DedupKey)
	}
}
