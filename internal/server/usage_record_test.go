package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api/internal/pool"
)

// TestUsageRecordActuallyPersists 是最关键的一条：
// 发一次成功请求必须让 usage.json 的记录数增长。
//
// 背景（真实 bug）：UsageTracker.Record 此前定义了但**从未被任何地方调用**，
// 导致 usage.json 只有启动时加载的历史数据、之后永不增长。控制台的趋势图
// 与明细表看到的都是陈旧快照，表现为"明明用了很多却显示 0"。
//
// 这条测试同时覆盖非流式与流式两条记录路径。
func TestUsageRecordActuallyPersists(t *testing.T) {
	dir := t.TempDir()
	usagePath := filepath.Join(dir, "usage.json")

	h := NewHandler(Config{Pool: pool.New(""), Upstream: nil})
	tracker := NewUsageTracker(usagePath)
	h.usageTracker = tracker

	// --- 非流式路径 ---
	resp := map[string]any{
		"usage": map[string]any{
			"prompt_tokens":     float64(120),
			"completion_tokens": float64(30),
			"total_tokens":      float64(150),
			"credit":            float64(0.25),
		},
	}
	st := &chatStat{start: time.Now().Add(-1500 * time.Millisecond), model: "m1", toks: -1}
	h.recordUsageFromResp("uid-1", "m1", resp, st)

	if got := len(tracker.records); got != 1 {
		t.Fatalf("非流式：记录数=%d want 1", got)
	}
	r0 := tracker.records[0]
	if r0.PromptTokens != 120 || r0.CompletionTokens != 30 || r0.TotalTokens != 150 {
		t.Errorf("token 字段错误: %+v", r0)
	}
	if r0.Credit != 0.25 {
		t.Errorf("credit=%v want 0.25", r0.Credit)
	}
	if r0.UID != "uid-1" || r0.Model != "m1" {
		t.Errorf("uid/model 错误: %+v", r0)
	}
	if r0.DurationMS < 1400 {
		t.Errorf("duration_ms=%d，应反映请求耗时（~1500ms）", r0.DurationMS)
	}

	// --- 流式路径 ---
	credit := 0.5
	stats := &chatStatsReader{
		hasUsage: true, hasCredit: true,
		tokens: 40, prompt: 200, credit: credit,
	}
	h.recordUsage("uid-1", "m2", stats, st)

	if got := len(tracker.records); got != 2 {
		t.Fatalf("流式：记录数=%d want 2", got)
	}
	r1 := tracker.records[1]
	if r1.PromptTokens != 200 || r1.CompletionTokens != 40 || r1.TotalTokens != 240 {
		t.Errorf("流式 token 字段错误: %+v", r1)
	}
	if r1.Credit != 0.5 {
		t.Errorf("流式 credit=%v want 0.5", r1.Credit)
	}

	// --- 必须真的落盘（否则重启后仍丢失）---
	raw, err := os.ReadFile(usagePath)
	if err != nil {
		t.Fatalf("usage.json 未写入: %v", err)
	}
	var persisted []UsageRecord
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("usage.json 解析失败: %v", err)
	}
	if len(persisted) != 2 {
		t.Errorf("落盘记录数=%d want 2", len(persisted))
	}
}

// TestUsageRecordSkipsWhenUsageMissing usage 缺失时**不得**写入假记录。
//
// 若写入 token 全 0 的记录，趋势图会出现无意义的零点、占比也被拉低。
func TestUsageRecordSkipsWhenUsageMissing(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: nil})
	h.usageTracker = NewUsageTracker(filepath.Join(t.TempDir(), "usage.json"))
	st := &chatStat{start: time.Now(), toks: -1}

	// 非流式：resp 无 usage
	h.recordUsageFromResp("uid-1", "m1", map[string]any{"choices": []any{}}, st)
	// 流式：hasUsage=false
	h.recordUsage("uid-1", "m1", &chatStatsReader{}, st)

	if got := len(h.usageTracker.records); got != 0 {
		t.Errorf("usage 缺失时不应记录，实际写入 %d 条", got)
	}
}

// TestUsageRecordNilTrackerSafe 未初始化 tracker 时不得 panic。
func TestUsageRecordNilTrackerSafe(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: nil})
	h.usageTracker = nil
	st := &chatStat{start: time.Now()}
	// 两条路径都不应 panic
	h.recordUsageFromResp("uid-1", "m1", map[string]any{"usage": map[string]any{}}, st)
	h.recordUsage("uid-1", "m1", &chatStatsReader{hasUsage: true}, st)
}

// TestUsageRecordAppearsInAnalytics 写入的记录必须能被聚合查询看到。
//
// 这条把"写入"与"读取"接起来——此前的 bug 正是两者断开：
// 读取路径一直正常，写入从未发生。
func TestUsageRecordAppearsInAnalytics(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: nil})
	h.usageTracker = NewUsageTracker(filepath.Join(t.TempDir(), "usage.json"))
	st := &chatStat{start: time.Now()}

	h.recordUsageFromResp("uid-1", "glm-5.2", map[string]any{
		"usage": map[string]any{
			"prompt_tokens":     float64(1000),
			"completion_tokens": float64(500),
			"total_tokens":      float64(1500),
			"credit":            float64(1.5),
		},
	}, st)

	data := h.usageTracker.GetAnalytics("24h")
	summary, _ := data["range_summary"].(map[string]any)
	if summary == nil {
		t.Fatal("range_summary 缺失")
	}
	if n, _ := summary["total_requests"].(int); n != 1 {
		t.Errorf("total_requests=%v want 1（写入的记录未出现在聚合里）", summary["total_requests"])
	}
	if c, _ := summary["completion_tokens"].(int); c != 500 {
		t.Errorf("completion_tokens=%v want 500", summary["completion_tokens"])
	}
	if cr, _ := summary["total_credit"].(float64); cr != 1.5 {
		t.Errorf("total_credit=%v want 1.5", cr)
	}

	// 明细接口也应看到
	recs, total := h.usageTracker.Records("24h", "", 0, 100)
	if total != 1 || len(recs) != 1 {
		t.Errorf("Records: total=%d len=%d want 1/1", total, len(recs))
	}
	if len(recs) == 1 && recs[0].Model != "glm-5.2" {
		t.Errorf("model=%q want glm-5.2", recs[0].Model)
	}
}
