package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var errGone = errors.New("gone")

func newRouter() *Router {
	rt := New(Options{
		Authenticate: func(r *http.Request) (any, error) {
			if r.Header.Get("X-User") == "" {
				return nil, errors.New("no user")
			}
			return r.Header.Get("X-User"), nil
		},
		MapError: func(err error) (int, string) {
			if errors.Is(err, errGone) {
				return http.StatusNotFound, "not_found"
			}
			return 0, ""
		},
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxBody: 32,
	})
	rt.Handle("POST /things", func(r *Request) (any, error) {
		var in struct{ Name string }
		if err := r.Decode(&in); err != nil {
			return nil, err
		}
		return Created{Value: map[string]any{"name": in.Name, "user": r.User}}, nil
	})
	rt.Handle("GET /things/{id}", func(r *Request) (any, error) {
		id, err := r.PathID("id")
		if err != nil {
			return nil, err
		}
		switch id {
		case 1:
			return nil, errGone
		case 2:
			return nil, errors.New("db exploded: secret dsn")
		}
		return nil, nil
	})
	return rt
}

func TestRouter(t *testing.T) {
	rt := newRouter()
	for _, c := range []struct {
		method, path, user, body string
		status                   int
		contains                 string
	}{
		{"POST", "/things", "", `{"name":"a"}`, 401, `"unauthenticated"`},
		{"POST", "/things", "u1", `{"name":"a"}`, 201, `"user":"u1"`},
		{"POST", "/things", "u1", `{bad`, 400, `"bad_request"`},
		{"POST", "/things", "u1", "", 400, "empty request body"},
		{"POST", "/things", "u1", `{"name":"` + strings.Repeat("x", 64) + `"}`, 400, "exceeds"},
		{"GET", "/things/x", "u1", "", 400, "invalid id"},
		{"GET", "/things/1", "u1", "", 404, `"not_found"`},
		{"GET", "/things/2", "u1", "", 500, `"internal error"`},
		{"GET", "/things/3", "u1", "", 204, ""},
	} {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		if c.user != "" {
			r.Header.Set("X-User", c.user)
		}
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		if w.Code != c.status || !strings.Contains(w.Body.String(), c.contains) {
			t.Errorf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "secret") {
			t.Errorf("internal error leaked: %s", w.Body)
		}
	}
}
