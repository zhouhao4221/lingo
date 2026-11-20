package worker

import (
	"strings"
	"testing"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

func TestWithSectionReplacesAndKeepsMarkerLast(t *testing.T) {
	body := "desc\n\n<!-- lingo:ticket=8 -->\n"
	s1 := sectionStart + "\n- a\n" + sectionEnd
	b1 := withSection(body, s1)
	if gitea.ParseTicketMarker(b1) != 8 || !strings.HasSuffix(strings.TrimSpace(b1), "<!-- lingo:ticket=8 -->") || !strings.Contains(b1, "- a") {
		t.Fatalf("insert:\n%s", b1)
	}
	s2 := sectionStart + "\n- a\n- b\n" + sectionEnd
	b2 := withSection(b1, s2)
	if strings.Count(b2, sectionStart) != 1 || !strings.Contains(b2, "- b") || withSection(b2, s2) != b2 {
		t.Errorf("replace:\n%s", b2)
	}
}

func TestStripMarkersNested(t *testing.T) {
	if got := stripMarkers("a <!-<!-- lingo:x -->- lingo:public --> b"); strings.Contains(got, "lingo:") {
		t.Errorf("nested marker survived: %q", got)
	}
}

func TestIssueBodyStripsMarkersAndKeepsUnknownFields(t *testing.T) {
	tmpl := &gitea.IssueTemplate{Fields: []*gitea.IssueFormField{{Type: gitea.FieldInput, ID: "what", Attributes: map[string]any{"label": "现象"}}}}
	tk := &ticket.Ticket{ID: 8, ProjectKey: "demo", Body: "补充 <!-- lingo:ticket=7 --> 说明",
		Fields:  map[string][]string{"what": {"白屏<!-- lingo:public -->"}, "steps": {"点击登录"}},
		Context: map[string]string{"page": "/x <!--lingo:ticket=7-->"}}
	body := IssueBody(tk, tmpl, nil, "", "", "")
	if gitea.ParseTicketMarker(body) != 8 || strings.Count(body, "lingo:") != 1 {
		t.Errorf("markers not stripped:\n%s", body)
	}
	for _, want := range []string{"### 现象\n\n白屏", "### steps\n\n点击登录", "### 补充说明"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
}
