package gitea

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// rawRedirectServer 用原始 TCP 写出带任意（含非法）Location 的 3xx 响应，绕开 net/http 服务端对头的校验。
// 只监听本机回环地址；令牌为虚构值。
func rawRedirectServer(t *testing.T, code int, location string) (url string, hits *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	hits = &atomic.Int32{}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, req.Body) // 像正常服务一样读完请求体（含流式上传）再回应
				hits.Add(1)
				fmt.Fprintf(conn, "HTTP/1.1 %d Redirect\r\nLocation: %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", code, location)
			}(conn)
		}
	}()
	return "http://" + ln.Addr().String(), hits
}

// 无法解析的 Location：Go 在调用 CheckRedirect 之前解析 Location，失败时返回的错误含原始 Location。
// RejectRedirects 时必须仍报告为脱敏、不可重试的 RedirectError，且不再发出任何请求。
var badLocations = []string{
	"http://[leak-secret-701",           // 非法 IPv6 字面量
	"http://leak-secret-701.test:port/", // 非法端口
	"/leak-secret-701/%zz",              // 非法转义
	"http://leak-secret-701.test/%",
}

func TestRejectRedirectsUnparseableLocation(t *testing.T) {
	for _, loc := range badLocations {
		for _, code := range []int{301, 302, 303, 307, 308} {
			for name, call := range calls {
				t.Run(fmt.Sprintf("%d/%s/%s", code, loc, name), func(t *testing.T) {
					base, hits := rawRedirectServer(t, code, loc)
					c, err := New(Options{BaseURL: base, Token: fakeToken, RejectRedirects: true})
					if err != nil {
						t.Fatal(err)
					}
					err = call(c)
					var re *RedirectError
					if !errors.Is(err, ErrRedirect) || !errors.As(err, &re) {
						t.Fatalf("err = %v, want RedirectError", err)
					}
					if re.StatusCode != code || re.Kind != "unknown" {
						t.Errorf("RedirectError = %+v, want status %d kind unknown", re, code)
					}
					if IsRetryable(err) {
						t.Error("must not be retryable")
					}
					if msg := err.Error(); strings.Contains(msg, "leak-secret-701") || strings.Contains(msg, fakeToken) || strings.Contains(msg, "Location") {
						t.Errorf("error leaks Location or token: %s", msg)
					}
					if n := hits.Load(); n != 1 {
						t.Errorf("server got %d requests, want 1 (no follow-up)", n)
					}
				})
			}
		}
	}
}

// 默认（RejectRedirects 为假）保持 Go 原有行为：非法 Location 仍是 Go 自己的解析错误，不变成 RedirectError。
func TestDefaultUnparseableLocationUnchanged(t *testing.T) {
	base, hits := rawRedirectServer(t, http.StatusFound, badLocations[0])
	c, err := New(Options{BaseURL: base, Token: fakeToken})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.GetVersion(context.Background())
	if err == nil || errors.Is(err, ErrRedirect) || !strings.Contains(err.Error(), "failed to parse Location header") {
		t.Fatalf("default behavior changed: err = %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("hits = %d", hits.Load())
	}
}
