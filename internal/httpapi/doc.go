// Package httpapi 是 REST JSON 的通用路由层：认证、JSON 请求解析、错误响应（{"error":{"code","message"}}）与编码。
// 具体路由在根包 lingo 的 handler.go 中注册，处理器只做参数适配，业务与鉴权在领域层；httpapi 不依赖根包，
// 避免循环引用。路径为相对路径，由接入方挂载到任意前缀。
package httpapi
