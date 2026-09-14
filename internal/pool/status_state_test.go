package pool

import (
	"testing"
	"time"

	"workbuddy2api/internal/auth"
)

// mustStatus 取单个账号状态（测试辅助，包装 Pool.Status）。
func mustStatus(t *testing.T, p *Pool, uid string) Status {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("uid %q 不在池中", uid)
	}
	return st
}

// TestStatusStateFailsBelowThresholdIsReady 是最重要的一条：
// breaker_fails>0 但未达阈值时，账号必须报告为 ready（健康），不是"熔断中"。
//
// 背景：前端曾用 breaker_fails>0 判定熔断，导致连续失败 1~2 次的健康账号
// 被标红。fails 只是"连续失败计数"，达 breakerThreshold 才真的熔断。
func TestStatusStateFailsBelowThresholdIsReady(t *testing.T) {
	p := New("")
	p.SetRandomSource(func(int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	// 记录 1 次失败（阈值 3，未达）
	p.NoteError("u1")

	st := mustStatus(t, p, "u1")
	if st.BreakerFails == 0 {
		t.Fatal("应记录到连续失败计数 >0")
	}
	if st.BreakerThreshold != defaultBreakerThreshold {
		t.Errorf("threshold=%d want %d", st.BreakerThreshold, defaultBreakerThreshold)
	}
	if st.State != "ready" {
		t.Errorf("失败 %d 次（阈值 %d）应为 ready，实际 %q（label=%q）",
			st.BreakerFails, st.BreakerThreshold, st.State, st.StateLabel)
	}
	if !st.Selectable {
		t.Error("未达熔断阈值的账号应仍可选号")
	}
	if st.StateLabel != "就绪" {
		t.Errorf("state_label=%q want 就绪", st.StateLabel)
	}
}

// TestStatusStateBreakerWhenUntilInFuture 达到阈值后 breaker_until 在未来 → breaker。
func TestStatusStateBreakerWhenUntilInFuture(t *testing.T) {
	p := New("")
	p.SetRandomSource(func(int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	// 连续失败到阈值触发熔断
	for i := 0; i < defaultBreakerThreshold; i++ {
		p.NoteError("u1")
	}

	st := mustStatus(t, p, "u1")
	if st.BreakerUntil.IsZero() {
		t.Fatal("达到阈值后 breaker_until 应被设置")
	}
	if st.State != "breaker" {
		t.Errorf("应在熔断中，实际 state=%q", st.State)
	}
	if st.Selectable {
		t.Error("熔断中的账号不应可选")
	}
	// 熔断时应给出熔断剩余时间（独立字段，不能用 cool_remaining_sec——
	// 后者只表示账号级软/硬冷却，熔断时它可能是 0）
	if st.BreakerRemaining <= 0 {
		t.Errorf("熔断中的 breaker_remaining_sec=%d，应为正数", st.BreakerRemaining)
	}
}

// TestStatusCoolRemainingExcludesBreaker 回归护栏：
// cool_remaining_sec 只反映账号级冷却，**不得**被熔断时间污染。
//
// 这条锁定既有契约（既有测试 TestCooldownSoftCappedBySoftRateMax 等依赖它）。
// 纯熔断（无软冷却）时 cool_remaining_sec 应为 0，而 breaker_remaining_sec > 0。
func TestStatusCoolRemainingExcludesBreaker(t *testing.T) {
	p := New("")
	p.SetRandomSource(func(int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	for i := 0; i < defaultBreakerThreshold; i++ {
		p.NoteError("u1")
	}
	st := mustStatus(t, p, "u1")

	if st.CoolRemaining != 0 {
		t.Errorf("纯熔断（无软冷却）时 cool_remaining_sec 应为 0，实际 %d", st.CoolRemaining)
	}
	if st.BreakerRemaining <= 0 {
		t.Errorf("熔断剩余应 >0，实际 %d", st.BreakerRemaining)
	}
}

// TestStatusStateDisabledWins disabled 优先级最高（即使同时有熔断/冷却）。
func TestStatusStateDisabledWins(t *testing.T) {
	p := New("")
	p.SetRandomSource(func(int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	for i := 0; i < defaultBreakerThreshold; i++ {
		p.NoteError("u1")
	}
	p.Disable("u1", "manual disable")

	st := mustStatus(t, p, "u1")
	if st.State != "disabled" {
		t.Errorf("disabled 应优先于熔断，实际 state=%q", st.State)
	}
	if st.Selectable {
		t.Error("已停用账号不应可选")
	}
	if st.DisabledReason == "" {
		t.Error("应透出禁用原因")
	}
}

// TestStatusStateHealthyIsReadyAndSelectable 全新账号 = ready + 可选。
func TestStatusStateHealthyIsReadyAndSelectable(t *testing.T) {
	p := New("")
	p.SetRandomSource(func(int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	st := mustStatus(t, p, "u1")
	if st.State != "ready" || !st.Selectable {
		t.Errorf("健康账号应为 ready+selectable，实际 state=%q selectable=%v", st.State, st.Selectable)
	}
	if st.BreakerFails != 0 {
		t.Errorf("新账号 breaker_fails 应为 0，实际 %d", st.BreakerFails)
	}
}

// TestStatusStateMatchesHealthy 不变量：Selectable 必须与 healthy() 完全一致。
//
// 这条防止两处口径分叉——状态字段是给界面看的，选号用的是 healthy()，
// 两者若不一致就会出现"界面显示就绪但选不中"这类幽灵问题。
func TestStatusStateMatchesHealthy(t *testing.T) {
	p := New("")
	p.SetRandomSource(func(int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})

	now := time.Now()
	check := func(stage string) {
		st := mustStatus(t, p, "u1")
		p.mu.RLock()
		e := p.byUID["u1"]
		healthy := e.healthy(now)
		p.mu.RUnlock()
		if st.Selectable != healthy {
			t.Errorf("%s: Selectable=%v 与 healthy()=%v 不一致（state=%q）",
				stage, st.Selectable, healthy, st.State)
		}
	}

	check("初始")
	p.NoteError("u1")
	check("失败1次")
	p.NoteError("u1")
	check("失败2次")
}
