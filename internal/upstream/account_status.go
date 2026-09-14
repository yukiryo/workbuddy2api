// account_status.go 账号深度状态查询（配额明细 / 签到状态）。
//
// 与 UserResource（只聚合出总剩余）的区别：这里需要明细，供控制台展示
// 「套餐名 / 本期用量 / 周期结束时间」。两者共用 billing 域与请求头。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"workbuddy2api/internal/auth"
)

const (
	checkinStatusPath = "/v2/billing/meter/checkin-activity-status"
)

// QuotaDetail 配额明细（一期账单口径）。
type QuotaDetail struct {
	Plan         string `json:"plan,omitempty"`
	Remain       int64  `json:"remain"`
	Total        int64  `json:"total"`
	Used         int64  `json:"used"`
	CycleEndTime string `json:"cycle_end_time,omitempty"`
}

// quotaPath 复用余额查询接口，取其明细字段。
func (c *Client) QuotaDetail(a *auth.Auth) (*QuotaDetail, error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":  1,
		"PageSize":    100,
		"ProductCode": "p_tcaca",
		"Status":      []int{0, 3},
		// 时间窗口必须足够宽，且 Begin 用过去时间：
		//
		// 上游按「套餐扣费结束时间」(DeductionEndTime) 过滤。不同账号的套餐
		// 周期差异极大——实测有的账号扣费期到 2034 年。若只开 1 年窗口（Begin=now），
		// 这类账号会被整条过滤掉，返回 Accounts=0，表现为"配额全 0 且无报错"。
		//
		// 这里的口径与 upstream.UserResource 保持一致（它用 365*101 天），
		// 但额外把 Begin 前推一年：既能覆盖已开始但未结束的套餐，
		// 也不会因客户端时钟略快而漏掉刚开始的套餐。
		"PackageEndTimeRangeBegin": now.AddDate(-1, 0, 0).Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	data, err := c.billingJSON(a, http.MethodPost, billingMeterPath, body)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []struct {
					PackageName         string `json:"PackageName"`
					CapacitySize        int64  `json:"CapacitySize"`
					CapacityRemain      int64  `json:"CapacityRemain"`
					CapacityUsed        int64  `json:"CapacityUsed"`
					CycleCapacitySize   int64  `json:"CycleCapacitySize"`
					CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
					CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
					CycleEndTime        string `json:"CycleEndTime"`
				} `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("quota parse: %w", err)
	}

	q := &QuotaDetail{}
	for _, acct := range resp.Response.Data.Accounts {
		// 与 UserResource 保持一致的取值优先级（周期口径优先，回落总量口径）。
		if acct.CycleCapacitySize > 0 {
			q.Remain += clampNonNeg(acct.CycleCapacityRemain)
			q.Total += acct.CycleCapacitySize
			q.Used += clampNonNeg(acct.CycleCapacityUsed)
		} else {
			q.Remain += clampNonNeg(acct.CapacityRemain)
			q.Total += acct.CapacitySize
			q.Used += clampNonNeg(acct.CapacityUsed)
		}
		// 套餐名/周期结束取第一个非空（通常所有套餐同属一个订阅）。
		if q.Plan == "" && acct.PackageName != "" {
			q.Plan = acct.PackageName
		}
		if q.CycleEndTime == "" && acct.CycleEndTime != "" {
			q.CycleEndTime = acct.CycleEndTime
		}
	}

	// Accounts 为空时报错而不是返回全零：
	//
	// "查不到套餐"与"套餐额度为 0"在 UI 上是完全不同的结论（前者是数据缺失、
	// 后者是余额耗尽）。此前静默返回 {0,0,0} 会让用户误以为账号没额度，
	// 而真实原因往往只是查询参数把套餐过滤掉了（曾实际发生过：
	// PackageEndTimeRangeEnd 窗口过窄导致某账号整条被滤掉）。
	if len(resp.Response.Data.Accounts) == 0 {
		return nil, fmt.Errorf("quota: 上游未返回任何套餐（可能被查询时间窗过滤，或该账号确实无有效订阅）")
	}
	return q, nil
}

func clampNonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// CheckinInfo 签到活动状态。
type CheckinInfo struct {
	Active      bool  `json:"active"`
	CheckedIn   bool  `json:"checked_in"`
	StreakDays  int   `json:"streak_days"`
	DailyCredit int64 `json:"daily_credit"`
	TotalCredit int64 `json:"total_credits"`
}

// CheckinStatus 查询签到活动状态（只读，不触发签到）。
//
// 注意：billingJSON → doJSON 已剥掉外层 {code,msg,data} 信封，返回的 data 就是
// 签到对象本身，故这里直接按签到字段解析（不要再套一层 "data"）。
func (c *Client) CheckinStatus(a *auth.Auth) (*CheckinInfo, error) {
	data, err := c.billingJSON(a, http.MethodPost, checkinStatusPath, map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Active       bool  `json:"active"`
		TodayChecked bool  `json:"today_checked_in"`
		StreakDays   int   `json:"streak_days"`
		DailyCredit  int64 `json:"daily_credit"`
		TotalCredits int64 `json:"total_credits"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("checkin parse: %w", err)
	}
	return &CheckinInfo{
		Active:      resp.Active,
		CheckedIn:   resp.TodayChecked,
		StreakDays:  resp.StreakDays,
		DailyCredit: resp.DailyCredit,
		TotalCredit: resp.TotalCredits,
	}, nil
}
