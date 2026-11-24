package lingo

import (
	"fmt"
	"slices"
	"testing"
)

func TestAudienceIDs(t *testing.T) {
	if got, cut := audienceIDs("me", []string{"a", "", "me", "b", "a"}); !slices.Equal(got, []string{"a", "b"}) || cut {
		t.Errorf("got %v %v", got, cut)
	}
	var many []string
	for i := range MaxTicketAudience {
		many = append(many, fmt.Sprint(i))
	}
	// 恰好上限，另有本人与重复项：未丢弃任何人，不算截断
	if got, cut := audienceIDs("me", append(slices.Clone(many), "me", "0")); len(got) != MaxTicketAudience || cut {
		t.Errorf("at cap: len %d cut %v", len(got), cut)
	}
	if got, cut := audienceIDs("me", append(many, "extra")); len(got) != MaxTicketAudience || !cut || got[0] != "0" {
		t.Errorf("over cap: len %d cut %v", len(got), cut)
	}
}
