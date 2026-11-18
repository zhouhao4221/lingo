// Package auth 固定角色与项目范围鉴权：Principal 是领域层的调用者身份（由根包从 lingo.User 转换），
// 提供项目范围与角色判断；各领域的可见性规则（PRD 3.2.0.6）在领域包中基于它实现。
package auth
