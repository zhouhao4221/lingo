package gitea

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 请求在发出时取一次配置：期间被 Reconfigure 替换不影响进行中的请求，之后的请求用新配置。
func TestReconfigureKeepsInFlightConfig(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"id":1,"login":"bot"}`))
	}))
	defer srv.Close()
	c := NewUnconfigured()
	if _, err := c.GetCurrentUser(context.Background()); !errors.Is(err, ErrNotConfigured) || c.Configured() {
		t.Fatalf("unconfigured: %v", err)
	}
	if err := c.Reconfigure(Options{BaseURL: srv.URL, Token: "old"}); err != nil {
		t.Fatal(err)
	}
	req, err := c.newRequest(context.Background(), "GET", "/user", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reconfigure(Options{BaseURL: srv.URL, Token: "new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.send(req, nil); err != nil { // 已建好的请求仍用旧配置
		t.Fatal(err)
	}
	if _, err := c.GetCurrentUser(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "token old" || got[1] != "token new" {
		t.Errorf("authorization = %q", got)
	}
	if err := c.Reconfigure(Options{BaseURL: "not a url", Token: "x"}); err == nil {
		t.Error("invalid options accepted")
	}
	c.Unconfigure()
	if c.Configured() {
		t.Error("still configured")
	}
}
