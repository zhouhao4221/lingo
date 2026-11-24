// Package settings 是连接配置（模型与 Gitea）的领域逻辑：把代码中的 lingo.Config 与数据库中保存的值合并为生效配置
// 并标出每项来源，按字段修改、校验、生成掩码。不依赖 store、gitea、llm；持久化经 Store 接口由 internal/store 实现。
package settings
