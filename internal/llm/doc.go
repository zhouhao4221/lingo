// Package llm 经 lingo.Model 调用模型的封装：领域与 worker 不直接调用 Model。目前提供 Drafter——用轻量模型把工单
// 整理为结构清晰的 Gitea Issue（标题、模板字段、描述），输出逐项校验，失败时由调用方退回原内容。
package llm
