package lingo

import (
	"fmt"
	"slices"
	"testing"
)

func TestAudienceIDs(t *testing.T) {
	if got := audienceIDs("me", []string{"a", "", "me", "b", "a"}); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("got %v", got)
	}
	var many []string
	for i := range MaxTicketAudience + 5 {
		many = append(many, fmt.Sprint(i))
	}
	if got := audienceIDs("me", many); len(got) != MaxTicketAudience || got[0] != "0" {
		t.Errorf("len = %d", len(got))
	}
}
