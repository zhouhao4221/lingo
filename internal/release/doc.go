// Package release 版本与上线：同步对接仓库的 Gitea Release（及只打 tag 的版本），按上一正式版本到本版本之间的
// 提交匹配工单的修复提交、再按版本说明中的 Issue 引用补漏，把修复已发布的工单推进为 released 并通知；
// 部署工作流成功、提交状态成功或接入方调用 MarkDeployed 时，把修复已随版本上线的工单推进为 deployed。
// 详见 PRD 3.2.0.4。
package release
