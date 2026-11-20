package perspective

import "slices"

// 视角取值，与 lingo.Perspective 一致。
const (
	User     = "user"     // 使用者
	Business = "business" // 业务方
	Expert   = "expert"   // 专家
	Leader   = "leader"   // 决策者
)

// Order 是视角在 Issue 区块、标签与查询结果中的固定顺序。
var Order = []string{User, Business, Expert, Leader}

// Normalize 把视角规范为固定取值：为空视为 user，非法值 ok 为 false。
func Normalize(p string) (string, bool) {
	if p == "" {
		return User, true
	}
	return p, slices.Contains(Order, p)
}

// Name 是视角的中文名（使用者 / 业务方 / 专家 / 决策者），用于标签与提示；未知值按使用者。
func Name(p string) string {
	switch p {
	case Business:
		return "业务方"
	case Expert:
		return "专家"
	case Leader:
		return "决策者"
	}
	return "使用者"
}

// Section 是视角在 Issue 区块中的段落标题（使用者反馈 / 业务补充 / 专家意见 / 决策意见）。
func Section(p string) string {
	switch p {
	case Business:
		return "业务补充"
	case Expert:
		return "专家意见"
	case Leader:
		return "决策意见"
	}
	return "使用者反馈"
}
