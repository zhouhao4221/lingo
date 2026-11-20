// Package llm 模型调用：
//   - OpenAI：OpenAI 兼容接口（Chat Completions、Embeddings）的标准库 HTTP 客户端，供根包 NewOpenAIModel /
//     NewDeepSeekModel 使用，不引入 SDK；
//   - Drafter：用轻量模型把工单整理为结构清晰的 Gitea Issue（标题、模板字段、描述），输出逐项校验，失败时由调用方
//     退回原内容。
//
// 领域与 worker 不直接调用 lingo.Model，经本包的类型调用。
package llm
