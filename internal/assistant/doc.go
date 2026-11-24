// Package assistant 知识问答 Agent：问答会话（只有本人可见）、把自然语言提问解析为查询意图、按调用者权限查询工单与
// 版本记录、组织附来源的回答，以及对话中需用户确认后才执行的操作（待确认操作，本期为给工单留言）。
//
// 模型调用经 Brain 接口（由 internal/llm 实现），领域层不直接调用 lingo.Model；模型只输出意图，查询一律由本包按
// 调用者身份经 ticket / release 领域执行，回答引用由本包按本轮查询结果生成。
package assistant
