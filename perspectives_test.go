package lingo

import (
	"strings"
	"testing"
)

func TestRequiredPerspectivesOverride(t *testing.T) {
	l := &Lingo{cfg: Config{RequiredPerspectives: []Perspective{PerspectiveBusiness, PerspectiveLeader}, Projects: []Project{
		{Key: "inherit"},
		{Key: "own", RequiredPerspectives: []Perspective{PerspectiveExpert}},
		{Key: "none", RequiredPerspectives: []Perspective{}},
	}}}
	for key, want := range map[string]string{"inherit": "business,leader", "own": "expert", "none": "", "undeclared": "business,leader"} {
		if got := strings.Join(l.requiredPerspectives(key), ","); got != want {
			t.Errorf("%s: %q, want %q", key, got, want)
		}
	}
	if got := (&Lingo{}).requiredPerspectives("x"); len(got) != 0 {
		t.Errorf("unconfigured: %v", got)
	}
	if err := validatePerspectives(Config{RequiredPerspectives: []Perspective{"boss"}}); err == nil {
		t.Error("invalid perspective accepted")
	}
	if err := validatePerspectives(Config{Projects: []Project{{Key: "p", RequiredPerspectives: []Perspective{""}}}}); err == nil {
		t.Error("empty perspective accepted")
	}
}
