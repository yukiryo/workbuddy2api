package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UsageRecord 单次请求用量记录。
type UsageRecord struct {
	Timestamp        int64   `json:"timestamp"` // Unix 秒
	Model            string  `json:"model"`
	UID              string  `json:"uid"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Credit           float64 `json:"credit"`
	DurationMS       int64   `json:"duration_ms"`
}

// UsageTracker 管理用量历史记录与聚合分析。
type UsageTracker struct {
	mu       sync.RWMutex
	filePath string
	records  []UsageRecord
}

// NewUsageTracker 初始化用量统计跟踪器。
func NewUsageTracker(savePath string) *UsageTracker {
	t := &UsageTracker{
		filePath: savePath,
		records:  make([]UsageRecord, 0),
	}
	t.load()
	return t
}

func (t *UsageTracker) load() {
	if t.filePath == "" {
		return
	}
	data, err := os.ReadFile(t.filePath)
	if err != nil {
		return
	}
	var recs []UsageRecord
	if err := json.Unmarshal(data, &recs); err == nil {
		t.records = recs
	}
}

func (t *UsageTracker) save() {
	if t.filePath == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(t.filePath), 0o755)
	data, err := json.Marshal(t.records)
	if err != nil {
		return
	}
	_ = os.WriteFile(t.filePath, data, 0o644)
}

// Record 记录一次成功的请求用量。
func (t *UsageTracker) Record(rec UsageRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// 补齐 total
	if rec.TotalTokens == 0 {
		rec.TotalTokens = rec.PromptTokens + rec.CompletionTokens
	}
	t.records = append(t.records, rec)

	// 保留最新 5000 条，防闪存超标
	if len(t.records) > 5000 {
		t.records = t.records[len(t.records)-5000:]
	}

	t.save()
}

// Clear 清空用量记录。
func (t *UsageTracker) Clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.records = make([]UsageRecord, 0)
	t.save()
}

// recordUsage 记录一次**流式**成功请求的用量。
//
// 为什么必须显式调用：UsageTracker.Record 此前从未被任何地方调用过，
// 导致 usage.json 只有首次加载的历史数据、之后永不增长——控制台的
// 趋势图与明细表看到的都是陈旧快照（表现为"明明用了很多却显示 0"）。
//
// usage 缺失（hasUsage=false）时不记录：宁可不记，也不写一条 token 全 0
// 的假记录，否则会污染趋势与占比统计。credit 缺失记 0（缺失≠0 但此处
// 仅用于展示，成本决策走 pool.NoteModelCost 的独立路径）。
func (h *Handler) recordUsage(uid, model string, stats *chatStatsReader, st *chatStat) {
	if h.usageTracker == nil || stats == nil {
		return
	}
	completion, ok := stats.Tokens()
	if !ok {
		return // 无 usage：不写假记录
	}
	credit, _ := stats.Credit()
	h.usageTracker.Record(UsageRecord{
		Timestamp:        time.Now().Unix(),
		Model:            model,
		UID:              uid,
		PromptTokens:     stats.PromptTokens(),
		CompletionTokens: completion,
		TotalTokens:      stats.PromptTokens() + completion,
		Credit:           credit,
		DurationMS:       time.Since(st.start).Milliseconds(),
	})
}

// recordUsageFromResp 记录一次**非流式**成功请求的用量（对应 recordUsage 的同步分支）。
func (h *Handler) recordUsageFromResp(uid, model string, resp map[string]any, st *chatStat) {
	if h.usageTracker == nil || resp == nil {
		return
	}
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return // 无 usage：不写假记录
	}
	num := func(k string) int {
		if v, ok := u[k].(float64); ok {
			return int(v)
		}
		return 0
	}
	credit := 0.0
	if v, ok := u["credit"].(float64); ok {
		credit = v
	}
	prompt, completion := num("prompt_tokens"), num("completion_tokens")
	total := num("total_tokens")
	if total == 0 {
		total = prompt + completion
	}
	h.usageTracker.Record(UsageRecord{
		Timestamp:        time.Now().Unix(),
		Model:            model,
		UID:              uid,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
		Credit:           credit,
		DurationMS:       time.Since(st.start).Milliseconds(),
	})
}

// ModelStat 模型用量统计明细。
type ModelStat struct {
	Model            string  `json:"model"`
	CallCount        int     `json:"call_count"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Credit           float64 `json:"credit"`      // 真实消耗积分（成本主指标）
	Percentage       float64 `json:"percentage"`  // 按积分占比（无积分时回落 token 占比）
}

// AccountStat 账号用量明细。
type AccountStat struct {
	UID              string  `json:"uid"`
	CallCount        int     `json:"call_count"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Credit           float64 `json:"credit"`     // 真实消耗积分（成本主指标）
	Percentage       float64 `json:"percentage"` // 按积分占比（无积分时回落 token 占比）
}

// TimeBucket 时间趋势数据桶。
type TimeBucket struct {
	Label            string  `json:"label"`
	Timestamp        int64   `json:"timestamp"`
	CallCount        int     `json:"call_count"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Credit           float64 `json:"credit"` // 该桶真实消耗积分（成本视角的主指标）
}

// usageWindows 时间窗定义（对齐参考项目 codebuddy2api 的分桶模型）。
//
// 设计要点：**固定桶数、按桶宽对齐**，而不是"回溯 N 小时再随便切"。
// 这样同一 range 下桶的边界是稳定的（例如 24h 总是整点对齐），
// 自动刷新时曲线不会左右抖动。
type usageWindow struct {
	bucketCount  int
	bucketSizeMs int64
	// dayAligned 为 true 时按自然日对齐（today/yesterday），
	// 否则按 bucketSizeMs 的整数倍滚动对齐。
	dayAligned bool
	// dayOffset 自然日对齐时的偏移天数（yesterday = -1）。
	dayOffset int
	// fixedDays 按整天分桶时的天数（3d/7d）
	fixedDays int
}

// rollingWindows 滚动窗口（按 bucketSize 整数倍对齐）。
var rollingWindows = map[string]usageWindow{
	"1h":  {bucketCount: 12, bucketSizeMs: 5 * 60 * 1000},
	"3h":  {bucketCount: 12, bucketSizeMs: 15 * 60 * 1000},
	"6h":  {bucketCount: 12, bucketSizeMs: 30 * 60 * 1000},
	"12h": {bucketCount: 12, bucketSizeMs: 60 * 60 * 1000},
	"24h": {bucketCount: 24, bucketSizeMs: 60 * 60 * 1000},
}

// resolveUsageWindow 求某 range 的窗口边界（对齐参考项目实现）。
// 返回 (startMs, endMs, bucketCount, bucketSizeMs)。
func resolveUsageWindow(rangeType string, now time.Time) (int64, int64, int, int64) {
	nowMs := now.UnixMilli()

	switch rangeType {
	case "today", "yesterday":
		// 按自然日 0 点对齐，固定 24 桶（每小时一格）
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		if rangeType == "yesterday" {
			start = start.AddDate(0, 0, -1)
		}
		end := start.AddDate(0, 0, 1)
		return start.UnixMilli(), end.UnixMilli(), 24, 60 * 60 * 1000

	case "3d", "7d":
		// 按自然日对齐，固定 N 桶（每天一格），末桶是"今天"
		days := 3
		if rangeType == "7d" {
			days = 7
		}
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		end := today.AddDate(0, 0, 1)
		start := end.AddDate(0, 0, -days)
		return start.UnixMilli(), end.UnixMilli(), days, 24 * 60 * 60 * 1000
	}

	w, ok := rollingWindows[rangeType]
	if !ok {
		// 未知 range 回落到 24h，避免前端传错就白屏
		w = rollingWindows["24h"]
	}
	// 当前桶起点：向下取整到 bucketSize 的整数倍
	currentStart := (nowMs / w.bucketSizeMs) * w.bucketSizeMs
	end := currentStart + w.bucketSizeMs
	start := currentStart - int64(w.bucketCount-1)*w.bucketSizeMs
	return start, end, w.bucketCount, w.bucketSizeMs
}

// GetAnalytics 获取指定范围的聚合分析数据。
func (t *UsageTracker) GetAnalytics(rangeType string) map[string]any {
	t.mu.RLock()
	defer t.mu.RUnlock()

	now := time.Now()

	// 规范 range 并求窗口（对齐参考项目：固定桶数 + 桶宽对齐）
	switch rangeType {
	case "1h", "3h", "6h", "12h", "24h", "3d", "7d", "today", "yesterday":
		// 合法
	default:
		rangeType = "24h"
	}
	winStartMs, winEndMs, bucketCount, bucketSizeMs := resolveUsageWindow(rangeType, now)
	startUnix := winStartMs / 1000
	endUnix := winEndMs / 1000

	// 1. 过滤符合时间范围的记录（窗口为 [start, end) 半开区间）
	var filtered []UsageRecord
	for _, r := range t.records {
		if r.Timestamp >= startUnix && r.Timestamp < endUnix {
			filtered = append(filtered, r)
		}
	}

	// 2. 汇总指标
	totalRequests := len(filtered)
	totalTokens := 0
	totalPrompt := 0
	totalCompletion := 0
	totalCredit := 0.0

	modelMap := make(map[string]*ModelStat)
	accountMap := make(map[string]*AccountStat)

	for _, r := range filtered {
		totalTokens += r.TotalTokens
		totalPrompt += r.PromptTokens
		totalCompletion += r.CompletionTokens
		totalCredit += r.Credit

		// 模型分组
		m := r.Model
		if m == "" {
			m = "unknown"
		}
		ms, ok := modelMap[m]
		if !ok {
			ms = &ModelStat{Model: m}
			modelMap[m] = ms
		}
		ms.CallCount++
		ms.PromptTokens += r.PromptTokens
		ms.CompletionTokens += r.CompletionTokens
		ms.TotalTokens += r.TotalTokens
		ms.Credit += r.Credit

		// 账号分组
		u := r.UID
		if u == "" {
			u = "unknown"
		}
		as, ok := accountMap[u]
		if !ok {
			as = &AccountStat{UID: u}
			accountMap[u] = as
		}
		as.CallCount++
		as.PromptTokens += r.PromptTokens
		as.CompletionTokens += r.CompletionTokens
		as.TotalTokens += r.TotalTokens
		as.Credit += r.Credit
	}

	// 3. 计算模型占比并排序
	// 占比优先按积分（成本口径）；全部无积分时才回落 token 占比。
	byCredit := totalCredit > 0
	modelList := make([]*ModelStat, 0, len(modelMap))
	for _, ms := range modelMap {
		if byCredit {
			ms.Percentage = ms.Credit / totalCredit * 100
		} else if totalTokens > 0 {
			ms.Percentage = float64(ms.TotalTokens) / float64(totalTokens) * 100
		}
		modelList = append(modelList, ms)
	}
	// 排序同样以成本为主：积分高的在前（成本视角谁最烧钱）
	sort.Slice(modelList, func(i, j int) bool {
		if byCredit && modelList[i].Credit != modelList[j].Credit {
			return modelList[i].Credit > modelList[j].Credit
		}
		return modelList[i].TotalTokens > modelList[j].TotalTokens
	})

	// 4. 计算账号占比并排序
	accountList := make([]*AccountStat, 0, len(accountMap))
	for _, as := range accountMap {
		if byCredit {
			as.Percentage = as.Credit / totalCredit * 100
		} else if totalTokens > 0 {
			as.Percentage = float64(as.TotalTokens) / float64(totalTokens) * 100
		}
		accountList = append(accountList, as)
	}
	sort.Slice(accountList, func(i, j int) bool {
		if byCredit && accountList[i].Credit != accountList[j].Credit {
			return accountList[i].Credit > accountList[j].Credit
		}
		return accountList[i].TotalTokens > accountList[j].TotalTokens
	})

	// 5. 生成时间序列趋势桶 (Time Buckets)
	//
	// 分桶粒度自适应，避免"all 与 24h 都是 24 桶"这种看不出趋势的情况：
	//   1h    -> 5 分钟一桶（12 桶）
	//   today -> 1 小时一桶（按当日已过小时数）
	//   24h   -> 1 小时一桶（24 桶）
	//   7d    -> 1 天一桶（7 桶）
	//   all   -> 按实际跨度自选：<=2h 用 5 分钟 / <=2d 用小时 / <=60d 用天 / 更长用周
	//
	// 桶按**时间戳区间**归并（不是按格式化后的字符串比较），
	// 这样跨天/跨月/夏令时都不会出现"标签相同但时间不同"的错配。
	// 5. 生成时间序列趋势桶
	//
	// 桶边界由 resolveUsageWindow 给出（固定桶数 + 桶宽对齐，对齐参考项目），
	// 归桶用「时间戳区间求下标」而非字符串比较——后者跨天/跨月会标签碰撞。
	bucketSec := bucketSizeMs / 1000
	origin := winStartMs / 1000
	buckets := make([]*TimeBucket, bucketCount)
	for i := 0; i < bucketCount; i++ {
		startSec := origin + int64(i)*bucketSec
		buckets[i] = &TimeBucket{
			Label:     bucketLabel(time.Unix(startSec, 0), bucketSizeMs),
			Timestamp: startSec,
		}
	}
	for _, r := range filtered {
		if r.Timestamp < origin {
			continue
		}
		idx := int((r.Timestamp - origin) / bucketSec)
		if idx < 0 || idx >= len(buckets) {
			continue
		}
		b := buckets[idx]
		b.CallCount++
		b.PromptTokens += r.PromptTokens
		b.CompletionTokens += r.CompletionTokens
		b.TotalTokens += r.TotalTokens
		b.Credit += r.Credit
	}

	return map[string]any{
		"range": rangeType,
		"range_summary": map[string]any{
			"total_requests":    totalRequests,
			"total_tokens":      totalTokens,
			"prompt_tokens":     totalPrompt,
			"completion_tokens": totalCompletion,
			"total_credit":      totalCredit,
			// avg_duration_ms 平均耗时（全部记录都有 duration_ms）
			"avg_duration_ms": avgDurationMs(filtered),
			// prompt_tokens 语义提示：客户端在会话内每轮重发全部历史，
			// 故该值是"累计重发量"，会随会话轮次二次增长，不等于真实消耗。
			// 真实成本看 total_credit，真实产出看 completion_tokens。
			"prompt_tokens_note": "累计重发量（含会话上下文重复计数）",
		},
		"model_breakdown":   modelList,
		"account_breakdown": accountList,
		"time_series":       buckets,
		"bucket_seconds":    bucketSec,
		"window_start":      origin,
		"window_end":        winEndMs / 1000,
		"last_updated":      now.Format(time.RFC3339),
	}
}

// bucketLabel 按桶宽生成人类可读标签（对齐参考项目的格式约定）：
//   - 天级桶：MM-DD
//   - 小时及更细：HH:MM
//
// 额外的跨天可读性处理：当 24h 窗口跨过午夜时，仅显示 HH:MM 会让人
// 分不清是哪一天，故小时级桶一律带日期前缀（MM-DD HH:MM）。
func bucketLabel(t time.Time, bucketSizeMs int64) string {
	const dayMs = 24 * 60 * 60 * 1000
	if bucketSizeMs >= dayMs {
		return t.Format("01-02")
	}
	return t.Format("01-02 15:04")
}

// avgDurationMs 计算平均耗时（毫秒）；无数据返回 0。
func avgDurationMs(recs []UsageRecord) int64 {
	if len(recs) == 0 {
		return 0
	}
	var sum int64
	for _, r := range recs {
		sum += r.DurationMS
	}
	return sum / int64(len(recs))
}

func (h *Handler) apiGetUsage(w http.ResponseWriter, r *http.Request) {
	rangeType := r.URL.Query().Get("range")
	if rangeType == "" {
		rangeType = "24h"
	}
	if h.usageTracker == nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": "usage tracker not initialized"})
		return
	}
	data := h.usageTracker.GetAnalytics(rangeType)
	writeJSON(w, http.StatusOK, data)
}

// apiGetUsageRecords 返回逐条请求明细（秒级时间戳），供"明细"视图。
//
// 查询参数：
//
//	range  时间窗（1h/today/24h/7d/all，默认 1h）
//	limit  返回条数上限（默认 200，上限 2000）
//	offset 跳过条数（用于分页，默认 0）
//	model  可选，按模型名过滤（支持子串匹配）
//
// 返回按时间**倒序**（最新在前），便于"最近发生了什么"的排查场景。
func (h *Handler) apiGetUsageRecords(w http.ResponseWriter, r *http.Request) {
	if h.usageTracker == nil {
		writeJSON(w, http.StatusOK, map[string]any{"error": "usage tracker not initialized"})
		return
	}
	q := r.URL.Query()
	rangeType := q.Get("range")
	if rangeType == "" {
		rangeType = "1h"
	}
	limit := 200
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 2000 {
		limit = 2000
	}
	offset := 0
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	modelFilter := q.Get("model")

	recs, total := h.usageTracker.Records(rangeType, modelFilter, offset, limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"range":   rangeType,
		"total":   total,
		"offset":  offset,
		"limit":   limit,
		"records": recs,
	})
}

// Records 按条件取明细记录（倒序）。
// 返回 (本页记录, 过滤后的总条数)。
func (t *UsageTracker) Records(rangeType, modelFilter string, offset, limit int) ([]UsageRecord, int) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	now := time.Now()
	// 与聚合视图共用同一套窗口定义，保证"图表"与"明细"看到的是同一个时间范围
	switch rangeType {
	case "1h", "3h", "6h", "12h", "24h", "3d", "7d", "today", "yesterday":
	default:
		rangeType = "24h"
	}
	winStartMs, winEndMs, _, _ := resolveUsageWindow(rangeType, now)
	startUnix := winStartMs / 1000
	endUnix := winEndMs / 1000

	// 先过滤（含模型子串）
	matched := make([]UsageRecord, 0, len(t.records))
	for _, r := range t.records {
		if r.Timestamp < startUnix || r.Timestamp >= endUnix {
			continue
		}
		if modelFilter != "" && !strings.Contains(r.Model, modelFilter) {
			continue
		}
		matched = append(matched, r)
	}
	total := len(matched)

	// 倒序（最新在前）
	sort.Slice(matched, func(i, j int) bool { return matched[i].Timestamp > matched[j].Timestamp })

	if offset >= len(matched) {
		return []UsageRecord{}, total
	}
	end := offset + limit
	if end > len(matched) {
		end = len(matched)
	}
	return matched[offset:end], total
}

func (h *Handler) apiClearUsage(w http.ResponseWriter, r *http.Request) {
	if h.usageTracker != nil {
		h.usageTracker.Clear()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Usage history cleared",
	})
}
