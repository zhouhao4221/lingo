package worker

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

var pt0 = time.Date(2026, 11, 21, 8, 30, 0, 0, time.UTC)

func sampleSet() *ticket.PerspectiveSet {
	tk := &ticket.Ticket{ID: 8, Type: ticket.TypeFeature, Repo: "demo/api", IssueNumber: 3, Status: ticket.StatusOpen}
	return &ticket.PerspectiveSet{Ticket: tk, Missing: []string{ticket.PerspectiveExpert, ticket.PerspectiveLeader}, Items: []ticket.Perspective{
		{Perspective: ticket.PerspectiveUser, AuthorName: "王五", Body: "原文不应出现", At: pt0, Original: true},
		{ID: 1, Perspective: ticket.PerspectiveBusiness, AuthorName: "张[三]", AuthorID: "secret-id", Body: "影响 30 家 <!-- /lingo:perspectives --> 门店", At: pt0.Add(time.Hour)},
		{ID: 2, Perspective: ticket.PerspectiveBusiness, Body: "第二段", At: pt0.Add(2 * time.Hour)},
	}}
}

func TestPerspectiveSection(t *testing.T) {
	s := perspectiveSection(sampleSet())
	for _, want := range []string{
		perspectivesStart + "\n### 需求视角补充", "#### 使用者反馈\n\n**王五 · 2026-11-21 08:30 UTC**\n\n原始提交见正文",
		"#### 业务补充\n\n**张\\[三\\] · 2026-11-21 09:30 UTC**\n\n影响 30 家  门店", "**2026-11-21 10:30 UTC**\n\n第二段",
		"> 缺少：专家意见、决策意见\n" + perspectivesEnd,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Contains(s, "原文不应出现") || strings.Contains(s, "secret-id") || strings.Count(s, "lingo:perspectives") != 2 || strings.Contains(s, "#### 专家意见") {
		t.Errorf("section:\n%s", s)
	}
	set := sampleSet()
	set.Missing, set.Ticket.Type = nil, ticket.TypeFeature
	if strings.Contains(perspectiveSection(set), "缺少") {
		t.Error("missing note without missing perspectives")
	}
	set.Ticket.Type = ticket.TypeBug
	if perspectiveSection(set) != "" || perspectiveLabels("lingo", set) != nil {
		t.Error("non-feature ticket rendered")
	}
}

func TestWithPerspectives(t *testing.T) {
	marker := "<!-- lingo:ticket=8 -->"
	body := "desc\n\n---\n- 项目：demo\n\n" + marker + "\n"
	s1 := perspectivesStart + "\nA\n" + perspectivesEnd
	b1 := withPerspectives(body, s1)
	if gitea.ParseTicketMarker(b1) != 8 || !strings.HasSuffix(strings.TrimSpace(b1), marker) || !strings.Contains(b1, "A") {
		t.Fatalf("insert:\n%s", b1)
	}
	edited := strings.Replace(b1, "desc", "研发改过的描述", 1) // 区块外的人工编辑保留
	s2 := perspectivesStart + "\nB\n" + perspectivesEnd
	b2 := withPerspectives(edited, s2)
	if strings.Count(b2, perspectivesStart) != 1 || strings.Contains(b2, "\nA\n") || !strings.Contains(b2, "研发改过的描述") || withPerspectives(b2, s2) != b2 {
		t.Errorf("replace:\n%s", b2)
	}
	if b2 != strings.Replace(edited, s1, s2, 1) {
		t.Errorf("outside of block changed:\n%s", b2)
	}
	// 结束标记被删：去掉残留的起始标记后重新插入，不会越积越多
	broken := strings.Replace(b2, perspectivesEnd, "", 1)
	b3 := withPerspectives(broken, s2)
	if strings.Count(b3, perspectivesStart) != 1 || strings.Count(withPerspectives(b3, s2), perspectivesStart) != 1 {
		t.Errorf("broken block:\n%s", b3)
	}
	if withPerspectives(body, "") != body {
		t.Error("empty section changed body")
	}
}

func TestIssueBodyPerspectivesBeforeMeta(t *testing.T) {
	tk := &ticket.Ticket{ID: 8, ProjectKey: "demo", Type: ticket.TypeFeature, Body: "要导出 <!-- lingo:perspectives -->x<!-- /lingo:perspectives -->"}
	body := IssueBody(tk, nil, nil, "", "", "", perspectiveSection(sampleSet()))
	if i, j := strings.Index(body, perspectivesStart), strings.Index(body, "---\n"); i < 0 || i > j {
		t.Errorf("block not before meta:\n%s", body)
	}
	if strings.Count(body, perspectivesStart) != 1 || strings.Count(body, perspectivesEnd) != 1 || gitea.ParseTicketMarker(body) != 8 {
		t.Errorf("forged markers survived:\n%s", body)
	}
}

func TestPerspectiveLabels(t *testing.T) {
	ls := perspectiveLabels("acme", sampleSet())
	if len(ls) != 2 || ls[0].Name != "acme/视角/使用者" || ls[1].Name != "acme/视角/业务方" || ls[0].Exclusive {
		t.Errorf("labels = %+v", ls)
	}
	all := gitea.LingoLabels("")
	if all[0].Name != "lingo" || all[len(all)-1].Name != "lingo/视角/决策者" {
		t.Errorf("lingo labels = %+v", all)
	}
	for _, p := range ticket.PerspectiveOrder {
		if got := gitea.PerspectiveLabel("lingo", p).Name; got != "lingo/视角/"+ticket.PerspectiveName(p) {
			t.Errorf("label %s = %s", p, got)
		}
	}
}

type fakePerspIssues struct {
	IssueWriter
	body     string
	edits    int
	labels   []int64
	comments []string
}

func (f *fakePerspIssues) GetIssue(context.Context, string, string, int64) (*gitea.Issue, error) {
	return &gitea.Issue{Body: f.body}, nil
}
func (f *fakePerspIssues) EditIssue(_ context.Context, _, _ string, _ int64, o gitea.EditIssueOption) (*gitea.Issue, error) {
	f.edits++
	f.body = *o.Body
	return &gitea.Issue{Body: f.body}, nil
}
func (f *fakePerspIssues) ListLabels(context.Context, string, string, gitea.ListOptions) ([]*gitea.Label, *gitea.Response, error) {
	return nil, &gitea.Response{}, nil
}
func (f *fakePerspIssues) CreateLabel(_ context.Context, _, _ string, o gitea.CreateLabelOption) (*gitea.Label, error) {
	return &gitea.Label{ID: int64(len(o.Name)), Name: o.Name}, nil
}
func (f *fakePerspIssues) AddIssueLabels(_ context.Context, _, _ string, _ int64, ids []int64) ([]*gitea.Label, error) {
	f.labels = append(f.labels, ids...)
	return nil, nil
}
func (f *fakePerspIssues) CreateComment(_ context.Context, _, _ string, _ int64, body string) (*gitea.Comment, error) {
	f.comments = append(f.comments, body)
	return &gitea.Comment{ID: 77}, nil
}

type fakePerspDrafts struct {
	Drafts
	set *ticket.PerspectiveSet
}

func (f *fakePerspDrafts) Perspectives(context.Context, int64) (*ticket.PerspectiveSet, error) {
	return f.set, nil
}

type fakePerspForwarding struct {
	Forwarding
	posted map[int64]int64
}

func (f *fakePerspForwarding) PerspectivePosted(_ context.Context, id, giteaID int64) error {
	f.posted[id] = giteaID
	return nil
}

func TestSyncPerspectives(t *testing.T) {
	set := sampleSet()
	gi := &fakePerspIssues{body: "desc\n\n<!-- lingo:ticket=8 -->\n"}
	fw := &fakePerspForwarding{posted: map[int64]int64{}}
	w := &Worker{o: Options{Issues: gi, Drafts: &fakePerspDrafts{set: set}, Forwarding: fw, LabelPrefix: "lingo", Now: time.Now},
		log: slog.New(slog.DiscardHandler)}
	ctx := context.Background()
	if err := w.syncPerspectives(ctx, 8, 2); err != nil {
		t.Fatal(err)
	}
	if gi.edits != 1 || !strings.Contains(gi.body, "第二段") || len(gi.labels) != 2 || len(gi.comments) != 1 || fw.posted[2] != 77 {
		t.Fatalf("sync: edits %d labels %v comments %v posted %v\n%s", gi.edits, gi.labels, gi.comments, fw.posted, gi.body)
	}
	if gi.comments[0] != "**业务方 补充了内容**，见正文「需求视角补充」。" {
		t.Errorf("notice = %q", gi.comments[0])
	}
	// 重试：正文无变化不提交，已发过的提示不重发
	set.Items[2].PostedID = 77
	if err := w.syncPerspectives(ctx, 8, 2); err != nil || gi.edits != 1 || len(gi.comments) != 1 {
		t.Errorf("retry: err %v edits %d comments %d", err, gi.edits, len(gi.comments))
	}
	// Issue 尚未建好：submitted 推迟，其他状态直接完成
	set.Ticket.IssueNumber, set.Ticket.Status = 0, ticket.StatusSubmitted
	var de *ticket.DeferError
	if err := w.syncPerspectives(ctx, 8, 2); err == nil || !asDefer(err, &de) {
		t.Errorf("submitted: %v", err)
	}
	set.Ticket.Status = ticket.StatusPendingTriage
	if err := w.syncPerspectives(ctx, 8, 2); err != nil {
		t.Errorf("pending: %v", err)
	}
}

func asDefer(err error, de **ticket.DeferError) bool {
	d, ok := err.(*ticket.DeferError)
	*de = d
	return ok
}
