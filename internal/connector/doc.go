// Package connector 业务数据工具（REQ-008）：校验接入方注册的只读工具，以调用人身份调用并做权限预校验、参数轻量校验、
// 脱敏、截断、超时、并发上限与熔断，每次调用先写审计再执行。原始结果只返回给调用方，不写入审计与日志。
// 领域包不依赖根包：调用人以 Caller 传入，接入方的 lingo.User 放在 Caller.User 中原样交回 Call。
package connector
