package assistant

import (
	"fmt"
	"time"
)

var weekdays = [...]string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}

// today 是告诉模型的当前日期，如 "2026-11-24 星期二"。
func today(now time.Time, loc *time.Location) string {
	t := now.In(loc)
	return fmt.Sprintf("%s %s", t.Format("2006-01-02"), weekdays[t.Weekday()])
}

// timeRange 把意图中的时间换算为 [from, to)（零值表示不限）；日期格式不合法时 ok 为假。
// 自然周从周一开始，日期按 loc 解释。
func timeRange(t TimeSpec, now time.Time, loc *time.Location) (from, to time.Time, ok bool) {
	n := now.In(loc)
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc)
	week := day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7)) // 本周一
	month := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
	switch t.Preset {
	case "today":
		return day, day.AddDate(0, 0, 1), true
	case "yesterday":
		return day.AddDate(0, 0, -1), day, true
	case "this_week":
		return week, week.AddDate(0, 0, 7), true
	case "last_week":
		return week.AddDate(0, 0, -7), week, true
	case "this_month":
		return month, month.AddDate(0, 1, 0), true
	case "last_month":
		return month.AddDate(0, -1, 0), month, true
	case "last_7_days":
		return day.AddDate(0, 0, -6), day.AddDate(0, 0, 1), true
	case "last_30_days":
		return day.AddDate(0, 0, -29), day.AddDate(0, 0, 1), true
	}
	if t.From != "" {
		d, err := time.ParseInLocation("2006-01-02", t.From, loc)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		from = d
	}
	if t.To != "" {
		d, err := time.ParseInLocation("2006-01-02", t.To, loc)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		to = d.AddDate(0, 0, 1) // To 含当天
	}
	if !from.IsZero() && !to.IsZero() && !from.Before(to) {
		return time.Time{}, time.Time{}, false
	}
	return from, to, true
}
