package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
	Percentage       float64 `json:"percentage"`
}

// AccountStat 账号用量明细。
type AccountStat struct {
	UID              string  `json:"uid"`
	CallCount        int     `json:"call_count"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Percentage       float64 `json:"percentage"`
}

// TimeBucket 时间趋势数据桶。
type TimeBucket struct {
	Label            string `json:"label"`
	Timestamp        int64  `json:"timestamp"`
	CallCount        int    `json:"call_count"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
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
	}

	// 3. 计算模型占比并排序
	modelList := make([]*ModelStat, 0, len(modelMap))
	for _, ms := range modelMap {
		if totalTokens > 0 {
			ms.Percentage = float64(ms.TotalTokens) / float64(totalTokens) * 100
		}
		modelList = append(modelList, ms)
	}
	sort.Slice(modelList, func(i, j int) bool {
		return modelList[i].TotalTokens > modelList[j].TotalTokens
	})

	// 4. 计算账号占比并排序
	accountList := make([]*AccountStat, 0, len(accountMap))
	for _, as := range accountMap {
		if totalTokens > 0 {
			as.Percentage = float64(as.TotalTokens) / float64(totalTokens) * 100
		}
		accountList = append(accountList, as)
	}
	sort.Slice(accountList, func(i, j int) bool {
		return accountList[i].TotalTokens > accountList[j].TotalTokens
	})

	// 5. 生成时间序列趋势桶 (Time Buckets)
	var buckets []*TimeBucket
	if rangeType == "1h" {
		// 1 小时内：按 5 分钟分桶，共 12 桶
		for i := 11; i >= 0; i-- {
			bucketStart := now.Add(-time.Duration(i*5) * time.Minute)
			label := bucketStart.Format("15:04")
			buckets = append(buckets, &TimeBucket{
				Label:     label,
				Timestamp: bucketStart.Unix(),
			})
		}
	} else if rangeType == "7d" {
		// 7 天内：按天分桶，共 7 桶
		for i := 6; i >= 0; i-- {
			bucketStart := now.AddDate(0, 0, -i)
			label := bucketStart.Format("01-02")
			buckets = append(buckets, &TimeBucket{
				Label:     label,
				Timestamp: bucketStart.Unix(),
			})
		}
	} else {
		// 24h 或 today：按小时分桶，共 24 桶
		for i := 23; i >= 0; i-- {
			bucketStart := now.Add(-time.Duration(i) * time.Hour)
			label := bucketStart.Format("15:00")
			buckets = append(buckets, &TimeBucket{
				Label:     label,
				Timestamp: bucketStart.Unix(),
			})
		}
	}

	// 将 filtered 数据归入桶中
	if len(buckets) > 0 {
		for _, r := range filtered {
			tVal := time.Unix(r.Timestamp, 0)
			var matchLabel string
			if rangeType == "7d" {
				matchLabel = tVal.Format("01-02")
			} else if rangeType == "1h" {
				// 找到最近的 5 分钟桶
				min := (tVal.Minute() / 5) * 5
				matchLabel = time.Date(tVal.Year(), tVal.Month(), tVal.Day(), tVal.Hour(), min, 0, 0, tVal.Location()).Format("15:04")
			} else {
				matchLabel = tVal.Format("15:00")
			}

			for _, b := range buckets {
				if b.Label == matchLabel {
					b.CallCount++
					b.PromptTokens += r.PromptTokens
					b.CompletionTokens += r.CompletionTokens
					b.TotalTokens += r.TotalTokens
					break
				}
			}
		}
	}

	return map[string]any{
		"range": rangeType,
		"range_summary": map[string]any{
			"total_requests":    totalRequests,
			"total_tokens":      totalTokens,
			"prompt_tokens":     totalPrompt,
			"completion_tokens": totalCompletion,
			"total_credit":      totalCredit,
		},
		"model_breakdown":   modelList,
		"account_breakdown": accountList,
		"time_series":       buckets,
		"last_updated":      now.Format(time.RFC3339),
	}
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

func (h *Handler) apiClearUsage(w http.ResponseWriter, r *http.Request) {
	if h.usageTracker != nil {
		h.usageTracker.Clear()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Usage history cleared",
	})
}
