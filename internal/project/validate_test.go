package project

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	ok := []Spec{{Key: "a", DefaultRepo: "O/API", Repos: []RepoSpec{{Owner: "o", Name: "api", Kind: "backend"}, {Owner: "o", Name: "web"}}}}
	if err := Validate(ok); err != nil {
		t.Fatal(err)
	}
	for want, specs := range map[string][]Spec{
		"key is required":       {{}},
		"duplicate project key": {{Key: "a"}, {Key: "a"}},
		"invalid repo":          {{Key: "a", Repos: []RepoSpec{{Owner: "o"}}}},
		"invalid kind":          {{Key: "a", Repos: []RepoSpec{{Owner: "o", Name: "x", Kind: "db"}}}},
		"declared in both":      {{Key: "a", Repos: []RepoSpec{{Owner: "o", Name: "x"}}}, {Key: "b", Repos: []RepoSpec{{Owner: "O", Name: "X"}}}},
		"default repo":          {{Key: "a", DefaultRepo: "o/y", Repos: []RepoSpec{{Owner: "o", Name: "x"}}}},
	} {
		if err := Validate(specs); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}
