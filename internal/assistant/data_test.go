package assistant

import (
	"strings"
	"testing"

	"github.com/zhouhao4221/lingo/internal/connector"
)

func TestNeedInputText(t *testing.T) {
	missing := needInputText(&connector.InputError{Param: "order_id", Want: "required", Desc: "订单号"})
	if missing != "查询需要订单号（order_id），请补充后再问一次。" {
		t.Errorf("missing = %q", missing)
	}
	wrong := needInputText(&connector.InputError{Param: "order_id", Want: "string", Desc: "订单号"})
	if !strings.Contains(wrong, "格式不对") || !strings.Contains(wrong, "文本") || strings.Contains(wrong, "补充") {
		t.Errorf("type mismatch = %q", wrong)
	}
	if s := needInputText(&connector.InputError{Param: "days", Want: "[integer null]"}); !strings.Contains(s, "正确的格式") {
		t.Errorf("multi-type = %q", s)
	}
}
