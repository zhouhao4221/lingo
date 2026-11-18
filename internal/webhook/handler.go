package webhook

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/zhouhao4221/lingo/internal/gitea"
)

// Record 是一次待落库的投递；worker 之后读出 Payload 交给 Parse 还原 Event 处理。
type Record struct {
	DeliveryID  string
	Event       string // X-Gitea-Event
	EventType   string // X-Gitea-Event-Type
	Kind        Kind
	DedupKey    string
	ParseError  string // Kind 为 Invalid 时的解析错误
	Repo        string
	IssueNumber int64
	FromBot     bool
	Payload     []byte
	ReceivedAt  time.Time
}

// Store 保存投递记录，由 store 包基于 webhook_deliveries 表实现。
type Store interface {
	// SaveDelivery 原子写入一条投递；DeliveryID 或 DedupKey 已存在时不写入并返回 inserted=false。
	// Payload 是原始字节，应存为 bytea（jsonb 拒收含 NUL 字符 U+0000 转义的字符串）。
	SaveDelivery(ctx context.Context, r Record) (inserted bool, err error)
	// RecordPing 记录仓库（"owner/name"）最近一次收到 push / 测试投递的时间（按仓库覆盖写），供启动自检确认连通。
	RecordPing(ctx context.Context, repo string, at time.Time) error
}

// Options 是 Handler 配置。
type Options struct {
	Secret string // Webhook 共享密钥，必填
	// Authorization 非空时要求请求的 Authorization 头与之完全相同（对应 Hook.authorization_header）。
	Authorization string
	// BotLogin 返回机器人账号 login；启动时经 GET /user 取得，未就绪时返回空串（此时不判定 FromBot，
	// worker 处理时会按当时的 login 重新判定）。
	BotLogin  func() string
	ReplyMode ReplyMode
	MaxBytes  int64        // 请求体上限，≤0 时为 gitea.DefaultMaxPayloadBytes
	Store     Store        // 必填
	Logger    *slog.Logger // 为空时用 slog.Default()
	// SaveTimeout 是落库的超时时间，默认 3 秒，保证在 Gitea 5 秒投递超时内返回。
	SaveTimeout time.Duration
}

// Handler 是 Gitea Webhook 的 http.Handler：验签 → 归一化 → 去重落库 → 返回。
type Handler struct {
	o Options
}

// NewHandler 校验配置并创建 Handler。
func NewHandler(o Options) (*Handler, error) {
	if o.Secret == "" {
		return nil, errors.New("webhook: Secret is required")
	}
	if o.Store == nil {
		return nil, errors.New("webhook: Store is required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.SaveTimeout <= 0 {
		o.SaveTimeout = 3 * time.Second
	}
	if o.BotLogin == nil {
		o.BotLogin = func() string { return "" }
	}
	return &Handler{o: o}, nil
}

// ServeHTTP 的响应：
//   - 200 已落库，或重复投递（同一 delivery / 业务键已处理过）
//   - 200 验签通过但载荷无法解析（以 Kind=Invalid 落库，避免丢失）
//   - 400 缺少 delivery ID；401 签名或 Authorization 不符；405 非 POST；413 请求体过大
//   - 500 落库失败（Gitea 不会自动重投，靠定时对账补齐）
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	log := h.o.Logger.With("delivery", r.Header.Get(gitea.HeaderDelivery), "event", r.Header.Get(gitea.HeaderEventType))
	if h.o.Authorization != "" &&
		subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(h.o.Authorization)) != 1 {
		log.Warn("webhook rejected: authorization mismatch")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	d, err := gitea.ReadDelivery(r, h.o.Secret, h.o.MaxBytes)
	switch {
	case errors.Is(err, gitea.ErrPayloadTooLarge):
		// Gitea 不会重投，事件只能靠对账补齐，按错误级别记录
		log.Error("webhook rejected: payload too large", "max_bytes", h.o.MaxBytes)
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	case errors.Is(err, gitea.ErrMissingSignature), errors.Is(err, gitea.ErrBadSignature):
		log.Warn("webhook rejected: bad signature")
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	case err != nil:
		log.Warn("webhook rejected", "err", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if d.ID == "" {
		log.Warn("webhook rejected: missing delivery id")
		http.Error(w, "missing "+gitea.HeaderDelivery, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.o.SaveTimeout)
	defer cancel()
	e, perr := Parse(d, ParseOptions{BotLogin: h.o.BotLogin(), ReplyMode: h.o.ReplyMode})
	if perr != nil {
		log.Error("webhook payload unparsable, stored as invalid", "err", perr)
		e = &Event{Kind: Invalid, DedupKey: "delivery:" + d.ID}
	}
	switch e.Kind {
	case Ignored:
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ignored"))
		return
	case Ping:
		if err := h.o.Store.RecordPing(ctx, e.Repo, d.ReceivedAt); err != nil {
			log.Error("webhook ping save failed", "err", err)
			http.Error(w, "store failed", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
		return
	}
	parseErr := ""
	if perr != nil {
		parseErr = perr.Error()
	}
	inserted, err := h.o.Store.SaveDelivery(ctx, Record{
		DeliveryID:  d.ID,
		Event:       d.Event,
		EventType:   d.EventType,
		Kind:        e.Kind,
		DedupKey:    e.DedupKey,
		ParseError:  parseErr,
		Repo:        e.Repo,
		IssueNumber: e.IssueNumber,
		FromBot:     e.FromBot,
		Payload:     d.Body,
		ReceivedAt:  d.ReceivedAt,
	})
	if err != nil {
		log.Error("webhook save failed", "kind", e.Kind, "err", err)
		http.Error(w, "store failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	if !inserted {
		log.Debug("webhook duplicate", "kind", e.Kind, "dedup_key", e.DedupKey)
		_, _ = w.Write([]byte("duplicate"))
		return
	}
	_, _ = w.Write([]byte("ok"))
}
