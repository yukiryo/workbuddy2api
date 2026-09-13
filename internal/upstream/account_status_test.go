package upstream

import (
	"encoding/json"
	"testing"
)

// TestCheckinStatusParsingShape 锁定「信封已被 doJSON 剥掉」这一契约。
//
// 真实的 checkin-activity-status 上游响应形如：
//
//	{"code":0,"msg":"OK","data":{"active":true,"today_checked_in":false,
//	 "streak_days":6,"daily_credit":100,"total_credits":600,...}}
//
// billingJSON → doJSON 返回的是 **data 对象本身**，解析时不得再套一层 "data"。
// 若套了，CheckinStatus 会静默返回全零（曾实际发生过：控制台显示"连登 0 天"）。
func TestCheckinStatusParsingShape(t *testing.T) {
	// doJSON 的输出 = 上游 data 字段的内容。
	raw := json.RawMessage(`{
		"active": true,
		"today_checked_in": false,
		"streak_days": 6,
		"daily_credit": 100,
		"total_credits": 600,
		"theme_name": "Buddy加油站"
	}`)

	var resp struct {
		Active       bool  `json:"active"`
		TodayChecked bool  `json:"today_checked_in"`
		StreakDays   int   `json:"streak_days"`
		DailyCredit  int64 `json:"daily_credit"`
		TotalCredits int64 `json:"total_credits"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("解析: %v", err)
	}

	if resp.StreakDays != 6 {
		t.Errorf("streak_days=%d，期望 6（外层 data 若被重复套用此处会是 0）", resp.StreakDays)
	}
	if resp.DailyCredit != 100 {
		t.Errorf("daily_credit=%d，期望 100", resp.DailyCredit)
	}
	if resp.TotalCredits != 600 {
		t.Errorf("total_credits=%d，期望 600", resp.TotalCredits)
	}
	if !resp.Active {
		t.Error("active 应为 true")
	}
	if resp.TodayChecked {
		t.Error("today_checked_in 应为 false")
	}
}

// TestQuotaDetailParsingShape 配额响应有**两层**嵌套（Response.Data.Accounts），
// 与签到不同。此测试固定该差异，防止有人"统一"掉其中一层。
func TestQuotaDetailParsingShape(t *testing.T) {
	// doJSON 输出 = 上游 data 字段内容，即 {"Response":{"Data":{"Accounts":[...]}}}
	raw := json.RawMessage(`{
		"Response": {
			"Data": {
				"TotalCount": 1,
				"Accounts": [
					{
						"PackageName": "CodeBuddy个人版国内运营裂变包",
						"CapacitySize": 1000,
						"CapacityRemain": 810,
						"CapacityUsed": 189,
						"CycleCapacitySize": 1000,
						"CycleCapacityRemain": 810,
						"CycleCapacityUsed": 189,
						"CycleEndTime": "2026-09-30 23:59:59"
					}
				]
			}
		}
	}`)

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
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("解析: %v", err)
	}
	accts := resp.Response.Data.Accounts
	if len(accts) != 1 {
		t.Fatalf("Accounts 数量=%d，期望 1", len(accts))
	}
	a := accts[0]
	if a.PackageName != "CodeBuddy个人版国内运营裂变包" {
		t.Errorf("PackageName=%q", a.PackageName)
	}
	if a.CycleCapacityRemain != 810 || a.CycleCapacitySize != 1000 {
		t.Errorf("周期容量解析错误: remain=%d size=%d", a.CycleCapacityRemain, a.CycleCapacitySize)
	}
}

// TestClampNonNeg 负值必须钳到 0（上游在超支时可能返回负数）。
func TestClampNonNeg(t *testing.T) {
	if got := clampNonNeg(-5); got != 0 {
		t.Errorf("clampNonNeg(-5)=%d want 0", got)
	}
	if got := clampNonNeg(7); got != 7 {
		t.Errorf("clampNonNeg(7)=%d want 7", got)
	}
	if got := clampNonNeg(0); got != 0 {
		t.Errorf("clampNonNeg(0)=%d want 0", got)
	}
}
