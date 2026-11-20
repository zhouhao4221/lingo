// Package mcp 是 MCP（Model Context Protocol）Streamable HTTP 服务端的协议层：JSON-RPC 2.0 解析、initialize 版本协商、
// tools/list 与 tools/call、工具错误回传与 Origin 校验。只接受 POST 并直接返回 JSON 响应（不开 SSE 流、无会话），
// 多实例部署安全。具体工具在根包 lingo 的 mcp_tools.go 中注册，只做参数适配；mcp 不依赖根包。
package mcp
