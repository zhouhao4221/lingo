package knowledge

import (
	"context"
	"strings"
	"testing"
)

type captureStore struct {
	fakeStore
	got []NewEntry
}

func (c *captureStore) InsertKnowledge(_ context.Context, e NewEntry) (int64, error) {
	c.got = append(c.got, e)
	return int64(len(c.got)), nil
}
func (c *captureStore) SimilarKnowledge(context.Context, string, []string) (int64, float64, error) {
	return 7, 0.9, nil
}

// 回答候选：去掉 #N 工单号与 K 知识编号、脱敏，待审核，标记可能重复。
func TestSuggestStripsRefs(t *testing.T) {
	st := &captureStore{}
	s, _ := New(Options{Store: st})
	if err := s.Suggest(context.Background(), Draft{ProjectKey: "demo", Question: " SSO 怎么配置 ",
		Answer: "参考 K12：在集成页填写元数据，见 #7，联系 a@b.com"}, 99); err != nil {
		t.Fatal(err)
	}
	if len(st.got) != 1 {
		t.Fatalf("inserted %d", len(st.got))
	}
	e := st.got[0]
	if strings.Contains(e.Answer, "K12") || strings.Contains(e.Answer, "#7") || strings.Contains(e.Answer, "a@b.com") ||
		e.Question != "SSO 怎么配置" || e.Status != StatusPending || e.Source != SourceAssistantFeedback || e.SourceMessageID != 99 ||
		e.DuplicateOf != 7 {
		t.Errorf("entry = %+v", e)
	}
	// 内容为空：不入库
	if err := s.Suggest(context.Background(), Draft{ProjectKey: "demo", Question: "q", Answer: "K3 #4"}, 100); err != nil || len(st.got) != 1 {
		t.Errorf("empty answer inserted: %v %d", err, len(st.got))
	}
}
