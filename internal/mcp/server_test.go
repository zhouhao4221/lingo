package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var errNotFound = errors.New("not found")

func newServer() *Server {
	return New(Options{Name: "t", Version: "1",
		Authenticate: func(r *http.Request) (any, error) {
			if r.Header.Get("Authorization") == "" {
				return nil, errors.New("no")
			}
			return r.Header.Get("Authorization"), nil
		},
		ErrorText: func(err error) (string, bool) {
			if errors.Is(err, errNotFound) {
				return "not found", true
			}
			return "", false
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Tools: []Tool{
			{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`), Call: func(_ context.Context, u any, args json.RawMessage) (any, error) {
				var a struct{ Msg string }
				if err := Args(args, &a); err != nil {
					return nil, err
				}
				return map[string]any{"msg": a.Msg, "user": u}, nil
			}},
			{Name: "list", InputSchema: json.RawMessage(`{"type":"object"}`), Call: func(context.Context, any, json.RawMessage) (any, error) {
				return []int{1, 2}, nil
			}},
			{Name: "missing", InputSchema: json.RawMessage(`{"type":"object"}`), Call: func(context.Context, any, json.RawMessage) (any, error) {
				return nil, errNotFound
			}},
			{Name: "broken", InputSchema: json.RawMessage(`{"type":"object"}`), Call: func(context.Context, any, json.RawMessage) (any, error) {
				panic("boom")
			}},
		}})
}

func post(t *testing.T, s *Server, body string, hdr map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
	r.Header.Set("Authorization", "u1")
	for k, v := range hdr {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

func TestProtocol(t *testing.T) {
	s := newServer()
	_, init := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`, nil)
	if res := init["result"].(map[string]any); res["protocolVersion"] != "2025-03-26" || res["capabilities"].(map[string]any)["tools"] == nil {
		t.Errorf("initialize = %v", init)
	}
	_, init2 := post(t, s, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`, nil)
	if init2["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Errorf("unknown version not negotiated to latest: %v", init2)
	}
	if w, _ := post(t, s, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); w.Code != 202 {
		t.Errorf("notification: %d", w.Code)
	}
	_, list := post(t, s, `{"jsonrpc":"2.0","id":"a","method":"tools/list"}`, nil)
	if tools := list["result"].(map[string]any)["tools"].([]any); len(tools) != 4 || list["id"] != "a" {
		t.Errorf("tools/list = %v", list)
	}
	_, echo := post(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"hi"}}}`, nil)
	res := echo["result"].(map[string]any)
	if res["isError"] != nil || res["structuredContent"].(map[string]any)["user"] != "u1" {
		t.Errorf("echo = %v", echo)
	}
	_, lst := post(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"list"}}`, nil)
	if lst["result"].(map[string]any)["structuredContent"].(map[string]any)["items"] == nil {
		t.Errorf("list result = %v", lst)
	}
	for name, want := range map[string]string{"missing": "not found", "broken": "internal error"} {
		_, out := post(t, s, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"`+name+`"}}`, nil)
		res := out["result"].(map[string]any)
		if res["isError"] != true || res["content"].([]any)[0].(map[string]any)["text"] != want {
			t.Errorf("%s = %v", name, out)
		}
	}
	_, bad := post(t, s, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"echo","arguments":{"msg":5}}}`, nil)
	if !strings.HasPrefix(bad["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), "invalid arguments") {
		t.Errorf("bad args = %v", bad)
	}
	for body, code := range map[string]float64{
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"nope"}}`: codeInvalidParams,
		`{"jsonrpc":"2.0","id":8,"method":"resources/list"}`:                      codeMethodNotFound,
		`[{"jsonrpc":"2.0","id":9,"method":"ping"}]`:                              codeInvalidRequest,
		`{bad`: codeParse,
	} {
		if _, out := post(t, s, body, nil); out["error"].(map[string]any)["code"] != code {
			t.Errorf("%s: %v", body, out)
		}
	}
}

func TestTransportChecks(t *testing.T) {
	s := newServer()
	if w, _ := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Authorization": ""}); w.Code != 401 {
		t.Errorf("unauthenticated: %d", w.Code)
	}
	if w, _ := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Origin": "https://evil.example"}); w.Code != 403 {
		t.Errorf("foreign origin: %d", w.Code)
	}
	if w, _ := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Origin": "http://example.com"}); w.Code != 200 {
		t.Errorf("same origin: %d", w.Code) // httptest 的 Host 为 example.com
	}
	r := httptest.NewRequest("GET", "/mcp", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Errorf("GET: %d", w.Code)
	}
}
