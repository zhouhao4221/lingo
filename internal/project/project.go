package project

// Spec 是接入方在 lingo.Config.Projects 中声明的项目，由根包转换后同步入库。
type Spec struct {
	Key         string
	Name        string
	Description string
	DefaultRepo string // "owner/name"；为空取 Repos[0]
	Repos       []RepoSpec
}

// RepoSpec 是项目下的仓库声明。
type RepoSpec struct {
	Owner string
	Name  string
	Kind  string // frontend | backend | mobile | docs | other
}

// FullName 返回 "owner/name"。
func (r RepoSpec) FullName() string { return r.Owner + "/" + r.Name }
