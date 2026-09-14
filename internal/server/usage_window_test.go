package server

import (
	"testing"
	"time"
)

// TestResolveUsageWindowShape 锁定各 range 的桶数与桶宽（对齐参考项目 codebuddy2api）。
//
// 桶边界必须稳定：同一 range 下桶宽固定且对齐到整数倍，
// 否则自动刷新时曲线会左右抖动。
func TestResolveUsageWindowShape(t *testing.T) {
	now := time.Date(2026, 9, 14, 17, 23, 45, 0, time.Local)

	cases := []struct {
		rangeType string
		wantCount int
		wantStep  int64 // 毫秒
	}{
		{"1h", 12, 5 * 60 * 1000},
		{"3h", 12, 15 * 60 * 1000},
		{"6h", 12, 30 * 60 * 1000},
		{"12h", 12, 60 * 60 * 1000},
		{"24h", 24, 60 * 60 * 1000},
		{"today", 24, 60 * 60 * 1000},
		{"yesterday", 24, 60 * 60 * 1000},
		{"3d", 3, 24 * 60 * 60 * 1000},
		{"7d", 7, 24 * 60 * 60 * 1000},
	}
	for _, c := range cases {
		start, end, count, step := resolveUsageWindow(c.rangeType, now)
		if count != c.wantCount {
			t.Errorf("%s: bucketCount=%d want %d", c.rangeType, count, c.wantCount)
		}
		if step != c.wantStep {
			t.Errorf("%s: bucketSizeMs=%d want %d", c.rangeType, step, c.wantStep)
		}
		// 窗口跨度必须等于 桶数 × 桶宽
		if end-start != int64(count)*step {
			t.Errorf("%s: span=%d want %d", c.rangeType, end-start, int64(count)*step)
		}
	}
}

// TestResolveUsageWindowAlignment 桶边界必须对齐（1h 按 5 分钟、24h 按整小时）。
func TestResolveUsageWindowAlignment(t *testing.T) {
	// 17:23:45 → 1h 窗口末桶应含当前时刻，且起点对齐到 5 分钟整数倍
	now := time.Date(2026, 9, 14, 17, 23, 45, 0, time.Local)
	start, end, count, step := resolveUsageWindow("1h", now)
	if (start % step) != 0 {
		t.Errorf("1h start=%d 未对齐到 %d 的整数倍", start, step)
	}
	if end != start+int64(count)*step {
		t.Error("窗口终点与桶数/桶宽不自洽")
	}
	// 当前时刻必须落在窗口内
	if ms := now.UnixMilli(); ms < start || ms >= end {
		t.Errorf("当前时刻 %d 不在窗口 [%d,%d) 内", ms, start, end)
	}

	// 24h 应整点对齐
	s24, _, _, step24 := resolveUsageWindow("24h", now)
	if s24%(60*60*1000) != 0 {
		t.Errorf("24h start=%d 未整点对齐", s24)
	}
	_ = step24
}

// TestResolveUsageWindowTodayAlignedToMidnight today/yesterday 必须按自然日 0 点对齐。
func TestResolveUsageWindowTodayAlignedToMidnight(t *testing.T) {
	now := time.Date(2026, 9, 14, 17, 23, 45, 0, time.Local)
	start, end, _, _ := resolveUsageWindow("today", now)

	startT := time.UnixMilli(start)
	if startT.Hour() != 0 || startT.Minute() != 0 || startT.Second() != 0 {
		t.Errorf("today 起点应为 0 点，实际 %v", startT)
	}
	if startT.Day() != 14 {
		t.Errorf("today 起点应为当日，实际 %v", startT)
	}
	// 终点是次日 0 点
	if time.UnixMilli(end).Day() != 15 {
		t.Errorf("today 终点应为次日，实际 %v", time.UnixMilli(end))
	}

	// yesterday 起点应是当日 0 点减一天
	yStart, yEnd, _, _ := resolveUsageWindow("yesterday", now)
	if yEnd != start {
		t.Error("yesterday 终点应等于 today 起点")
	}
	if time.UnixMilli(yStart).Day() != 13 {
		t.Errorf("yesterday 起点应为 13 日，实际 %v", time.UnixMilli(yStart))
	}
}

// TestResolveUsageWindowUnknownFallsBack 未知 range 回落到 24h（不 panic、不空窗）。
func TestResolveUsageWindowUnknownFallsBack(t *testing.T) {
	now := time.Now()
	start, end, count, step := resolveUsageWindow("bogus", now)
	if count != 24 || step != 60*60*1000 {
		t.Errorf("未知 range 应回落 24h，实际 count=%d step=%d", count, step)
	}
	if end <= start {
		t.Error("窗口无效")
	}
}

// TestBucketLabelFormat 标签格式：天级 MM-DD，小时级 MM-DD HH:MM。
func TestBucketLabelFormat(t *testing.T) {
	ts := time.Date(2026, 9, 14, 15, 30, 0, 0, time.Local)
	const dayMs = int64(24 * 60 * 60 * 1000)

	if got := bucketLabel(ts, dayMs); got != "09-14" {
		t.Errorf("天级标签=%q want 09-14", got)
	}
	if got := bucketLabel(ts, 60*60*1000); got != "09-14 15:30" {
		t.Errorf("小时级标签=%q want 09-14 15:30", got)
	}
	if got := bucketLabel(ts, 5*60*1000); got != "09-14 15:30" {
		t.Errorf("分钟级标签=%q want 09-14 15:30", got)
	}
}
