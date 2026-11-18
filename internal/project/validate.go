package project

import (
	"fmt"
	"strings"
)

// Validate 校验声明：项目 key 唯一且非空；仓库 owner/name 非空、类型合法、在所有项目中唯一（不区分大小写）；
// 默认仓库须属于该项目。
func Validate(specs []Spec) error {
	keys := map[string]bool{}
	repos := map[string]string{}
	for _, p := range specs {
		if p.Key == "" {
			return fmt.Errorf("project key is required")
		}
		if keys[p.Key] {
			return fmt.Errorf("duplicate project key %q", p.Key)
		}
		keys[p.Key] = true
		found := p.DefaultRepo == ""
		for _, r := range p.Repos {
			if r.Owner == "" || r.Name == "" || strings.Contains(r.Owner, "/") || strings.Contains(r.Name, "/") {
				return fmt.Errorf("project %q: invalid repo %q", p.Key, r.FullName())
			}
			switch r.Kind {
			case "", "frontend", "backend", "mobile", "docs", "other":
			default:
				return fmt.Errorf("project %q: repo %s has invalid kind %q", p.Key, r.FullName(), r.Kind)
			}
			k := strings.ToLower(r.FullName())
			if other, ok := repos[k]; ok {
				return fmt.Errorf("repo %s is declared in both %q and %q", r.FullName(), other, p.Key)
			}
			repos[k] = p.Key
			if strings.EqualFold(p.DefaultRepo, r.FullName()) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("project %q: default repo %q is not one of its repos", p.Key, p.DefaultRepo)
		}
	}
	return nil
}
