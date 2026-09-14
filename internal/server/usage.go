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

// GetAnalytics 获取指定范围的聚合分析数据。
func (t *UsageTracker) GetAnalytics(rangeType string) map[string]any {
	t.mu.RLock()
	defer t.mu.RUnlock()

	now := time.Now()
	var startTime time.Time

	switch rangeType {
	case "1h":
		startTime = now.Add(-1 * time.Hour)
	case "today":
		y, m, d := now.Date()
		startTime = time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	case "7d":
		startTime = now.Add(-7 * 24 * time.Hour)
	case "all":
		startTime = time.Time{}
	case "24h":
		fallthrough
	default:
		rangeType = "24h"
		startTime = now.Add(-24 * time.Hour)
	}

	startUnix := startTime.Unix()

	// 1. 过滤符合时间范围的记录
	var filtered []UsageRecord
	for _, r := range t.records {
		if r.Timestamp >= startUnix {
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
	step, bucketCount := pickBucketStep(rangeType, now, filtered)
	if bucketCount <= 0 {
		bucketCount = 1
	}
	// 对齐到 step 的整数倍，保证桶边界稳定
	nowTrunc := time.Unix((now.Unix()/int64(step.Seconds()))*int64(step.Seconds()), 0)
	buckets := make([]*TimeBucket, 0, bucketCount)
	start := nowTrunc.Add(-time.Duration(bucketCount-1) * step)
	for i := 0; i < bucketCount; i++ {
		t := start.Add(time.Duration(i) * step)
		buckets = append(buckets, &TimeBucket{
			Label:     bucketLabel(t, step),
			Timestamp: t.Unix(),
		})
	}

	// 归桶：以第一个桶的起始时间为原点，按 step 求下标（O(1)，无需遍历桶）
	origin := start.Unix()
	stepSec := int64(step.Seconds())
	for _, r := range filtered {
		if r.Timestamp < origin {
			continue // 落在窗口之前（all 的自适应窗口可能窄于全量数据）
		}
		idx := int((r.Timestamp - origin) / stepSec)
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
		"bucket_seconds":    int64(step.Seconds()),
		"last_updated":      now.Format(time.RFC3339),
	}
}

// pickBucketStep 依 range 与数据实际跨度选择分桶粒度。
// 返回 (每桶时长, 桶数)。
func pickBucketStep(rangeType string, now time.Time, filtered []UsageRecord) (time.Duration, int) {
	const (
		fiveMin = 5 * time.Minute
		hour    = time.Hour
		day     = 24 * time.Hour
		week    = 7 * 24 * time.Hour
	)
	switch rangeType {
	case "1h":
		return fiveMin, 12
	case "today":
		// 当日 0 点到现在，按小时分桶（至少 1 桶）
		y, m, d := now.Date()
		midnight := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
		n := int(now.Sub(midnight).Hours()) + 1
		if n > 24 {
			n = 24
		}
		return hour, n
	case "7d":
		return day, 7
	case "24h":
		return hour, 24
	case "all":
		// 按真实跨度自适应：跨度小就细化，跨度大就粗化，保证桶数在 24~60 之间。
		if len(filtered) == 0 {
			return hour, 24
		}
		oldest := filtered[0].Timestamp
		for _, r := range filtered {
			if r.Timestamp < oldest {
				oldest = r.Timestamp
			}
		}
		span := now.Sub(time.Unix(oldest, 0))
		switch {
		case span <= 2*time.Hour:
			n := int(span.Minutes()/5) + 1
			if n < 1 {
				n = 1
			}
			if n > 60 {
				n = 60
			}
			return fiveMin, n
		case span <= 2*day:
			n := int(span.Hours()) + 1
			if n > 60 {
				n = 60
			}
			return hour, n
		case span <= 60*day:
			n := int(span.Hours()/24) + 1
			if n > 60 {
				n = 60
			}
			return day, n
		default:
			n := int(span.Hours()/(24*7)) + 1
			if n > 60 {
				n = 60
			}
			return week, n
		}
	}
	return hour, 24
}

// bucketLabel 按粒度生成人类可读标签。
func bucketLabel(t time.Time, step time.Duration) string {
	switch {
	case step < time.Hour:
		return t.Format("15:04")
	case step < 24*time.Hour:
		return t.Format("01-02 15:00")
	case step < 7*24*time.Hour:
		return t.Format("01-02")
	default:
		return t.Format("01-02")
	}
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
	var startUnix int64
	switch rangeType {
	case "1h":
		startUnix = now.Add(-1 * time.Hour).Unix()
	case "today":
		y, m, d := now.Date()
		startUnix = time.Date(y, m, d, 0, 0, 0, 0, now.Location()).Unix()
	case "7d":
		startUnix = now.Add(-7 * 24 * time.Hour).Unix()
	case "all":
		startUnix = 0
	case "24h":
		fallthrough
	default:
		startUnix = now.Add(-24 * time.Hour).Unix()
	}

	// 先过滤（含模型子串）
	matched := make([]UsageRecord, 0, len(t.records))
	for _, r := range t.records {
		if r.Timestamp < startUnix {
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
