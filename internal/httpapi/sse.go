package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// sseWriteTimeout 是单次 SSE 写入的期限。
const sseWriteTimeout = 30 * time.Second

// Streamed 表示处理器已经自行写出响应（如 SSE），Router 不再编码结果。
type Streamed struct{}

// SSE 是 Server-Sent Events 输出。响应头在第一次 Send 时才写出：在此之前返回的错误仍按普通 JSON 错误响应；
// 之后返回的错误改为发送一个 error 事件。
type SSE struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
}

// SSE 返回本请求的 SSE 输出（同一请求多次调用返回同一个）。
func (r *Request) SSE() *SSE {
	if r.sse == nil {
		r.sse = &SSE{w: r.w, rc: http.NewResponseController(r.w)}
	}
	return r.sse
}

// Started 报告是否已经开始输出。
func (s *SSE) Started() bool { return s.started }

// Send 发送一个事件，data 编码为 JSON；写失败（如客户端已断开）时返回错误。
func (s *SSE) Send(event string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if !s.started {
		h := s.w.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no") // 关闭 nginx 等反向代理的缓冲
		s.w.WriteHeader(http.StatusOK)
		s.started = true
	}
	// 每次写之前设滚动的写期限：流式回答可能超过服务器的 WriteTimeout，但客户端不读时不能让写一直阻塞
	// （否则请求与会话租约一直被占用）；不支持时忽略
	_ = s.rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	return s.rc.Flush()
}
