package ticket

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// 待办 #698：转研发后驳回与补充评论外发的仲裁、驳回原因与署名。

func forwardedWithComment(st Status) *memStore {
	m := newMem(base(st))
	m.notes = []Comment{{TicketID: 1, Source: "user", Body: "补充"}, {TicketID: 2, Source: "user", Body: "别的工单"}}
	return m
}

// 驳回先提交：之后的发送仲裁返回 false，评论留在灵问并审计 comment.withheld（source=system），不发事件，同一条只记一次；
// 未驳回的工单取得租约，不记审计。
func TestClaimCommentSendAfterRejectForwarded(t *testing.T) {
	ctx := context.Background()
	m := forwardedWithComment(StatusInProgress)
	s := newService(t, m, nil)
	if _, err := s.RejectForwarded(ctx, reviewer, 1, "重复"); err != nil {
		t.Fatal(err)
	}
	nh, ne := len(m.history), len(m.events)
	for range 2 {
		if ok, err := s.ClaimCommentSend(ctx, 1, 1, time.Minute); ok || err != nil {
			t.Fatalf("claim on rejected: %v %v", ok, err)
		}
	}
	if ok, err := s.ClaimCommentSend(ctx, 1, 2, time.Minute); ok || err != nil { // 不属于该工单
		t.Fatalf("foreign comment: %v %v", ok, err)
	}
	if got := m.history[nh:]; len(got) != 1 || got[0].Kind != "comment.withheld" || got[0].Source != "system" ||
		got[0].Payload["comment"] != int64(1) || got[0].Payload["issue"] != int64(12) {
		t.Errorf("history = %+v", got)
	}
	if len(m.events) != ne || len(m.sending) != 0 {
		t.Errorf("events %v sending %v", kinds(m.events[ne:]), m.sending)
	}
	m2 := forwardedWithComment(StatusOpen)
	if ok, err := newService(t, m2, nil).ClaimCommentSend(ctx, 1, 1, time.Minute); !ok || err != nil || len(m2.history) != 0 || !m2.sending[1] {
		t.Errorf("active ticket: %v %v %+v %v", ok, err, m2.history, m2.sending)
	}
}

// 发送先取得仲裁：租约有效期内驳回不提交（等待 SendWait 后冲突，不改状态、不入队、不发事件、不留痕）；请求方取消时不再等待；
// 租约释放后驳回成功，之后的发送仲裁返回 false。
func TestRejectForwardedWaitsForCommentSend(t *testing.T) {
	ctx := context.Background()
	m := forwardedWithComment(StatusInProgress)
	s, err := NewService(ServiceOptions{Store: m, Now: func() time.Time { return t0 }, SendWait: 150 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ClaimCommentSend(ctx, 1, 1, time.Minute); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	start := time.Now()
	_, err = s.RejectForwarded(ctx, reviewer, 1, "重复")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "being sent") || time.Since(start) < 150*time.Millisecond {
		t.Errorf("reject while sending: %v after %v", err, time.Since(start))
	}
	if tk := m.tickets[1]; tk.Status != StatusInProgress || len(m.tasks) != 0 || len(m.events) != 0 || len(m.history) != 0 {
		t.Errorf("reject committed while sending: %+v tasks %v events %v", tk, m.tasks, kinds(m.events))
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.RejectForwarded(cctx, reviewer, 1, "重复"); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled reject: %v", err)
	}
	if err := s.ReleaseCommentSend(ctx, 1, 1); err != nil {
		t.Fatal(err)
	}
	if tk, err := s.RejectForwarded(ctx, reviewer, 1, "重复"); err != nil || tk.Status != StatusRejected {
		t.Fatalf("reject after release: %+v %v", tk, err)
	}
	if ok, err := s.ClaimCommentSend(ctx, 1, 1, time.Minute); ok || err != nil {
		t.Errorf("claim after reject: %v %v", ok, err)
	}
	// 已驳回（幂等返回）不受租约影响；不等待（SendWait < 0）时直接冲突
	m2 := forwardedWithComment(StatusOpen)
	s2, _ := NewService(ServiceOptions{Store: m2, SendWait: -1})
	_, _ = s2.ClaimCommentSend(ctx, 1, 1, time.Minute)
	start = time.Now()
	if _, err := s2.RejectForwarded(ctx, reviewer, 1, "x"); !errors.Is(err, ErrConflict) || time.Since(start) > 50*time.Millisecond {
		t.Errorf("no-wait reject: %v after %v", err, time.Since(start))
	}
}

// 驳回原因与署名进入关闭任务；署名只取后台人员姓名（没有姓名时为空，不以用户 ID 代替）；重试轮次沿用原原因与署名。
func TestCloseTaskCarriesReasonAndSignature(t *testing.T) {
	ctx := context.Background()
	m := newMem(base(StatusOpen))
	s := newService(t, m, nil)
	if _, err := s.RejectForwarded(ctx, reviewer, 1, "  不在支持范围  "); err != nil {
		t.Fatal(err)
	}
	p := m.tasks["close_issue:ticket:1"].Payload
	if p["reason"] != "不在支持范围" || p["rejected_by"] != "Rita" || p["issue"] != int64(12) {
		t.Errorf("payload = %+v", p)
	}
	m.states = map[string]*TaskState{"close_issue:ticket:1": {Key: "close_issue:ticket:1", Status: CloseFailed}}
	if _, err := s.RetryIssueClose(ctx, admin, 1); err != nil {
		t.Fatal(err)
	}
	if p := m.tasks["close_issue:ticket:1:retry:1"].Payload; p["reason"] != "不在支持范围" || p["rejected_by"] != "Rita" {
		t.Errorf("retry payload = %+v", p)
	}
	m2 := newMem(base(StatusInProgress))
	if _, err := newService(t, m2, nil).RejectForwarded(ctx, admin, 1, "无效"); err != nil {
		t.Fatal(err)
	}
	if p := m2.tasks["close_issue:ticket:1"].Payload; p["rejected_by"] != "" || p["reason"] != "无效" {
		t.Errorf("no-name payload = %+v", p)
	}
}
