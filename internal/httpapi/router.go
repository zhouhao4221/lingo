package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"time"
)

// Options 是 Router 的配置。httpapi 不依赖根包：用户身份以 any 传递，错误到状态码的映射由根包提供。
type Options struct {
	// Authenticate 解析请求身份；返回错误时响应 401。
	Authenticate func(r *http.Request) (any, error)
	// MapError 把业务错误映射为 HTTP 状态码与错误码；返回 0 表示未知错误（500，不回显原因）。
	MapError func(err error) (status int, code string)
	Logger   *slog.Logger
	MaxBody  int64 // JSON 请求体上限，默认 1 MB
}

// Router 是 REST JSON 路由：认证、请求解析、错误与响应编码，处理器只做参数适配。
type Router struct {
	mux *http.ServeMux
	o   Options
}

// New 创建 Router。
func New(o Options) *Router {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.MaxBody <= 0 {
		o.MaxBody = 1 << 20
	}
	if o.MapError == nil {
		o.MapError = func(error) (int, string) { return 0, "" }
	}
	return &Router{mux: http.NewServeMux(), o: o}
}

// Request 是处理器收到的请求与已认证的用户。
type Request struct {
	*http.Request
	User any
	w    http.ResponseWriter
	max  int64
}

// HandlerFunc 返回要编码为 JSON 的结果；错误经 MapError 转为错误响应。
type HandlerFunc func(r *Request) (any, error)

// Created 包装结果，响应 201。
type Created struct{ Value any }

// ErrBadRequest 表示请求格式错误（JSON 无法解析、参数类型不对），响应 400。
var ErrBadRequest = errors.New("bad request")

// Blob 是二进制响应（附件下载），以附件形式返回，禁止浏览器嗅探与内联执行。
type Blob struct {
	Name        string
	ContentType string
	Data        []byte
}

// Handle 注册路由，pattern 同 http.ServeMux（如 "GET /tickets/{id}"）。写操作要求 JSON 请求体。
func (rt *Router) Handle(pattern string, h HandlerFunc) { rt.handle(pattern, h, false) }

// HandleUpload 注册接收 multipart/form-data 的写路由（文件上传），其他与 Handle 相同。
func (rt *Router) HandleUpload(pattern string, h HandlerFunc) { rt.handle(pattern, h, true) }

func (rt *Router) handle(pattern string, h HandlerFunc, multipart bool) {
	rt.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		u, err := rt.o.Authenticate(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "authentication required")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !multipart && !isJSON(r.Header.Get("Content-Type")) {
			// 写操作一律要求 Content-Type: application/json，没有请求体的 POST 也一样：跨站表单与无请求体的
			// no-cors fetch 都属于“简单请求”，可借 cookie 伪造写操作；application/json 会触发 CORS 预检，
			// 由接入方的 CORS 策略把关
			writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
			return
		}
		res, err := h(&Request{Request: r, User: u, w: w, max: rt.o.MaxBody})
		if err != nil {
			rt.fail(w, r, err)
			return
		}
		if b, ok := res.(Blob); ok {
			writeBlob(w, b)
			return
		}
		status := http.StatusOK
		if c, ok := res.(Created); ok {
			status, res = http.StatusCreated, c.Value
		}
		if res == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, status, res)
	})
}

func writeBlob(w http.ResponseWriter, b Blob) {
	h := w.Header()
	h.Set("Content-Type", b.ContentType)
	h.Set("Content-Length", strconv.Itoa(len(b.Data)))
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": b.Name}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
	h.Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b.Data)
}

// FormFile 读取 multipart 表单中名为 field 的文件，最多 max 字节；超出时返回 ErrBadRequest。
func (r *Request) FormFile(field string, max int64) (name string, data []byte, err error) {
	r.Body = http.MaxBytesReader(r.w, r.Body, max+1<<20) // 预留表单开销
	f, hdr, err := r.Request.FormFile(field)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return "", nil, fmt.Errorf("%w: file exceeds %d bytes", ErrBadRequest, max)
		}
		return "", nil, fmt.Errorf("%w: multipart field %q: %v", ErrBadRequest, field, err)
	}
	defer f.Close()
	data, err = io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return "", nil, fmt.Errorf("%w: read file: %v", ErrBadRequest, err)
	}
	return hdr.Filename, data, nil
}

func isJSON(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/json"
}

// ServeHTTP 实现 http.Handler。
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) { rt.mux.ServeHTTP(w, r) }

func (rt *Router) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, ErrBadRequest) {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	status, code := rt.o.MapError(err)
	if status == 0 {
		rt.o.Logger.Error("lingo: request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeError(w, status, code, err.Error())
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var b errorBody
	b.Error.Code, b.Error.Message = code, msg
	writeJSON(w, status, b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Decode 解析 JSON 请求体到 v；未知字段忽略（便于前端先于库升级）。
func (r *Request) Decode(v any) error {
	body := http.MaxBytesReader(r.w, r.Body, r.max)
	if err := json.NewDecoder(body).Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return fmt.Errorf("%w: request body exceeds %d bytes", ErrBadRequest, r.max)
		}
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: empty request body", ErrBadRequest)
		}
		return fmt.Errorf("%w: invalid JSON: %v", ErrBadRequest, err)
	}
	return nil
}

// DecodeOptional 同 Decode，但请求体为空时不报错（全部字段取零值）。
func (r *Request) DecodeOptional(v any) error {
	body := http.MaxBytesReader(r.w, r.Body, r.max)
	if err := json.NewDecoder(body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return fmt.Errorf("%w: request body exceeds %d bytes", ErrBadRequest, r.max)
		}
		return fmt.Errorf("%w: invalid JSON: %v", ErrBadRequest, err)
	}
	return nil
}

// PathID 解析路径参数 name 为正整数 ID。
func (r *Request) PathID(name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: invalid %s", ErrBadRequest, name)
	}
	return id, nil
}

// QueryInt 解析查询参数 name 为非负整数，缺省为 0。
func (r *Request) QueryInt(name string) (int64, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%w: invalid %s", ErrBadRequest, name)
	}
	return n, nil
}

// QueryTime 解析 RFC 3339 时间查询参数，缺省为零值。
func (r *Request) QueryTime(name string) (time.Time, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s must be RFC 3339", ErrBadRequest, name)
	}
	return t, nil
}

// Query 返回查询参数。
func (r *Request) Query(name string) string { return r.URL.Query().Get(name) }
