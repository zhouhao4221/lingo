package worker

import (
	"strings"
	"testing"

	"github.com/zhouhao4221/lingo/internal/gitea"
	"github.com/zhouhao4221/lingo/internal/ticket"
)

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
	body := IssueBody(tk, tmpl, "", "")
	if gitea.ParseTicketMarker(body) != 8 || strings.Count(body, "lingo:") != 1 {
		t.Errorf("markers not stripped:\n%s", body)
	}
	for _, want := range []string{"### 现象\n\n白屏", "### steps\n\n点击登录", "### 补充说明"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
}
