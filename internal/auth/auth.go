package auth

import "slices"

// 固定角色，取值与 lingo.Role 一致。
const (
	RoleExternal = "external"
	RoleAgent    = "agent"
	RoleReviewer = "reviewer"
	RoleAdmin    = "admin"
)

// Principal 是领域层使用的调用者身份，由根包从 lingo.User 转换。
type Principal struct {
	ID       string
	Name     string
	Email    string
	Role     string
	Projects []string // 可访问的项目 key；为空表示不限制
}

// InProject 报告调用者能否访问项目 key（User.Projects 为空时不限）。
func (p Principal) InProject(key string) bool {
	return len(p.Projects) == 0 || slices.Contains(p.Projects, key)
}

// Staff 报告调用者是否为后台人员（reviewer / admin），可确认转出、查看项目内全部工单。
func (p Principal) Staff() bool { return p.Role == RoleReviewer || p.Role == RoleAdmin }

// Admin 报告调用者是否为 admin。
func (p Principal) Admin() bool { return p.Role == RoleAdmin }

// Known 报告角色是否为固定角色之一。
func (p Principal) Known() bool {
	switch p.Role {
	case RoleExternal, RoleAgent, RoleReviewer, RoleAdmin:
		return true
	}
	return false
}
