package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// 支持的 MCP 协议版本，第一个为最新。
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Tool 是一个 MCP 工具。Call 返回的结果编码为 JSON 文本（同时作为 structuredContent）；
// 返回错误时作为工具执行错误（isError=true）回给客户端，错误信息由 Options.ErrorText 生成。
type Tool struct {
	Name        string
	Title       string
	Description string
	InputSchema json.RawMessage // JSON Schema（type: object）
	Call        func(ctx context.Context, user any, args json.RawMessage) (any, error)
}

// Options 是 Server 配置。mcp 不依赖根包：身份以 any 传递。
type Options struct {
	Name, Version string
	Instructions  string
	// Authenticate 解析请求身份；失败时响应 401。
	Authenticate func(r *http.Request) (any, error)
	// ErrorText 把工具执行错误转为给客户端看的文本；返回 ok=false 表示内部错误（只回“internal error”并记日志）。
	ErrorText func(err error) (text string, ok bool)
	Tools     []Tool
	Logger    *slog.Logger
	MaxBody   int64 // 默认 1 MB
}

// Server 是 MCP Streamable HTTP 服务端：只接受 POST 的 JSON-RPC 请求并直接返回 JSON 响应，不开 SSE 流、不维护会话
// （无状态，可多实例部署）。每个请求都经 Authenticate 鉴权，并校验 Origin 防 DNS 重绑定。
type Server struct {
	o     Options
	tools map[string]Tool
}

// New 创建 Server。
func New(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.MaxBody <= 0 {
		o.MaxBody = 1 << 20
	}
	if o.ErrorText == nil {
		o.ErrorText = func(error) (string, bool) { return "", false }
	}
	s := &Server{o: o, tools: map[string]Tool{}}
	for _, t := range o.Tools {
		s.tools[t.Name] = t
	}
	return s
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC 错误码。
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
)

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodGet, http.MethodDelete:
		// 不提供服务端推送流，也没有会话可终止
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	default:
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user, err := s.o.Authenticate(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.o.MaxBody))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) > 0 && trimmed[0] == '[' {
		writeJSON(w, response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeInvalidRequest, "batch requests are not supported"}})
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeParse, "parse error"}})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeJSON(w, response{JSONRPC: "2.0", ID: idOrNull(req.ID), Error: &rpcError{codeInvalidRequest, "invalid request"}})
		return
	}
	if len(req.ID) == 0 {
		// 通知（如 notifications/initialized）与客户端发来的响应：无需回复
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rerr := s.dispatch(r.Context(), user, &req)
	resp := response{JSONRPC: "2.0", ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	writeJSON(w, resp)
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// sameOrigin 校验浏览器请求的 Origin 与 Host 一致（MCP 规范要求防 DNS 重绑定）；非浏览器客户端不带 Origin，放行。
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func (s *Server) dispatch(ctx context.Context, user any, req *request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := protocolVersions[0]
		if slices.Contains(protocolVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		res := map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": s.o.Name, "version": s.o.Version},
		}
		if s.o.Instructions != "" {
			res["instructions"] = s.o.Instructions
		}
		return res, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		list := make([]map[string]any, 0, len(s.o.Tools))
		for _, t := range s.o.Tools {
			m := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
			if t.Title != "" {
				m["title"] = t.Title
			}
			list = append(list, m)
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{codeInvalidParams, "invalid params"}
		}
		t, ok := s.tools[p.Name]
		if !ok {
			return nil, &rpcError{codeInvalidParams, "unknown tool: " + p.Name}
		}
		if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
			p.Arguments = json.RawMessage("{}")
		}
		return s.call(ctx, user, t, p.Arguments), nil
	}
	return nil, &rpcError{codeMethodNotFound, "method not found: " + req.Method}
}

func (s *Server) call(ctx context.Context, user any, t Tool, args json.RawMessage) (res map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			s.o.Logger.Error("lingo: mcp tool panic", "tool", t.Name, "panic", r)
			res = toolError("internal error")
		}
	}()
	out, err := t.Call(ctx, user, args)
	if err != nil {
		var ae *ArgError
		if errors.As(err, &ae) {
			return toolError(ae.Error())
		}
		text, ok := s.o.ErrorText(err)
		if !ok {
			s.o.Logger.Error("lingo: mcp tool failed", "tool", t.Name, "err", err)
			text = "internal error"
		}
		return toolError(text)
	}
	b, err := json.Marshal(out)
	if err != nil {
		s.o.Logger.Error("lingo: mcp encode result", "tool", t.Name, "err", err)
		return toolError("internal error")
	}
	res = map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}}
	// structuredContent 须为对象；列表包一层
	if len(b) > 0 && b[0] == '{' {
		res["structuredContent"] = json.RawMessage(b)
	} else {
		res["structuredContent"] = map[string]json.RawMessage{"items": b}
	}
	return res
}

func toolError(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": true}
}

// ArgError 表示工具参数不合法，原样回给客户端。
type ArgError struct{ Msg string }

func (e *ArgError) Error() string { return "invalid arguments: " + e.Msg }

// Args 把 args 解析到 v；失败时返回 ArgError。
func Args(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return &ArgError{Msg: err.Error()}
	}
	return nil
}
