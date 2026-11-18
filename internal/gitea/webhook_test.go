package gitea

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func signedRequest(body, secret, sig string) *http.Request {
	r := httptest.NewRequest("POST", "/webhooks/gitea", strings.NewReader(body))
	if sig == "" && secret != "" {
		sig = hex.EncodeToString(Sign(secret, []byte(body)))
	}
	if sig != "-" {
		r.Header.Set(HeaderSignature, sig)
	}
	r.Header.Set(HeaderDelivery, "d-1")
	r.Header.Set(HeaderEvent, "issues")
	return r
}

func TestReadDelivery(t *testing.T) {
	body := `{"action":"label_updated","number":12}`
	r := signedRequest(body, "s", "")
	r.Header.Set(HeaderEventType, EventIssueLabel)
	d, err := ReadDelivery(r, "s", 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "d-1" || d.Event != "issues" || d.EventType != EventIssueLabel || string(d.Body) != body {
		t.Errorf("delivery = %+v", d)
	}
}

func TestReadDeliveryEventTypeFallback(t *testing.T) {
	d, err := ReadDelivery(signedRequest("{}", "s", ""), "s", 0)
	if err != nil || d.EventType != "issues" {
		t.Fatalf("delivery = %+v, err = %v", d, err)
	}
}

func TestReadDeliveryRejects(t *testing.T) {
	body := `{"a":1}`
	good := hex.EncodeToString(Sign("s", []byte(body)))
	cases := []struct {
		name string
		req  *http.Request
		max  int64
		want error
	}{
		{"missing", signedRequest(body, "", "-"), 0, ErrMissingSignature},
		{"wrong secret", signedRequest(body, "other", ""), 0, ErrBadSignature},
		{"not hex", signedRequest(body, "", "zz"), 0, ErrBadSignature},
		{"tampered", signedRequest(`{"a":2}`, "", good), 0, ErrBadSignature},
		{"too large", signedRequest(body, "s", ""), 3, ErrPayloadTooLarge},
	}
	for _, c := range cases {
		if _, err := ReadDelivery(c.req, "s", c.max); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if _, err := ReadDelivery(signedRequest(body, "", "sha256="+good), "s", 0); err != nil {
		t.Errorf("sha256= prefix rejected: %v", err)
	}
}

func TestDecodeCommentPayload(t *testing.T) {
	// 节选自 Gitea issue_comment 投递
	body := `{
	  "action": "created",
	  "issue": {"id": 1, "number": 12, "title": "导出失败", "state": "open", "labels": [{"id": 3, "name": "type/bug"}]},
	  "comment": {"id": 99, "body": "/reply 已修复，请升级到 v1.3", "user": {"id": 5, "login": "dev-agent"},
	              "issue_url": "https://g/api/v1/repos/o/r/issues/12", "created_at": "2026-11-18T08:00:00+08:00", "updated_at": "2026-11-18T08:00:00+08:00"},
	  "repository": {"id": 7, "name": "r", "full_name": "o/r", "owner": {"login": "o"}},
	  "sender": {"id": 5, "login": "dev-agent"},
	  "is_pull": false
	}`
	var p IssueCommentPayload
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	if p.Action != ActionCreated || p.Issue.Number != 12 || p.Comment.ID != 99 || p.Sender.Login != "dev-agent" ||
		p.Repository.FullName != "o/r" || IssueIndexFromURL(p.Comment.IssueURL) != 12 || p.Issue.Labels[0].Name != "type/bug" {
		t.Errorf("payload = %+v", p)
	}
}

func TestDecodePullRequestMerged(t *testing.T) {
	body := `{"action":"closed","number":3,"pull_request":{"number":3,"merged":true,"merge_commit_sha":"abc","body":"Fixes o/r#12",
	  "base":{"ref":"main"},"merged_at":"2026-11-18T00:00:00Z"},"repository":{"full_name":"o/r"},"sender":{"login":"dev"}}`
	var p PullRequestPayload
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	if p.Action != ActionClosed || !p.PullRequest.Merged || p.PullRequest.MergeCommitSHA != "abc" || p.PullRequest.MergedAt == nil {
		t.Errorf("payload = %+v", p.PullRequest)
	}
}
