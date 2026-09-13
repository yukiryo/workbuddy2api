// account.go 账号状态深度查询：配额余量、签到状态、连登天数、可用模型。
//
// 与 /status（读内存池状态，零成本）的区别：本文件的方法会**真实请求上游**，
// 因此只在用户主动点「刷新」时调用，绝不进入任何轮询路径。
//
// 所有查询都返回"部分成功"语义：某项失败只让该项为空 + 带 error 文案，
// 其余项照常返回。上游偶发抖动不该让整张卡片变成错误页。
package server

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// AccountStatus 单个账号的深度状态（对应前端一张卡片）。
//
// 直接复用 upstream 的查询结果类型，不在此重复定义同构结构体——
// 多一层映射只会带来漂移风险（字段改名时两边要同时改）。
type AccountStatus struct {
	UID      string               `json:"uid"`
	Nickname string               `json:"nickname,omitempty"`
	Quota    *upstream.QuotaDetail `json:"quota,omitempty"`
	Checkin  *upstream.CheckinInfo `json:"checkin,omitempty"`
	Models   []string             `json:"models,omitempty"`
	// Errors 各查询项的失败原因（键为 quota/checkin/models/account），成功项不出现。
	Errors    map[string]string `json:"errors,omitempty"`
	QueriedAt time.Time         `json:"queried_at"`
}

// statusCache 账号深度状态的短期缓存，防止用户连点刷新把上游打爆。
//
// 30 秒 TTL 的取舍：手动刷新场景下用户不会期待"每次点击都真实请求上游"，
// 而 4 个接口 × N 个账号的请求量在公网上是实打实的延迟（每项最长 20s）。
type statusCache struct {
	mu      sync.Mutex
	entries map[string]*AccountStatus
}

const statusCacheTTL = 30 * time.Second

func newStatusCache() *statusCache {
	return &statusCache{entries: make(map[string]*AccountStatus)}
}

func (c *statusCache) get(uid string, now time.Time) (*AccountStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.entries[uid]
	if !ok || now.Sub(st.QueriedAt) > statusCacheTTL {
		return nil, false
	}
	return st, true
}

func (c *statusCache) put(st *AccountStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[st.UID] = st
}

// invalidate 清掉某账号的缓存（签到后必须调用，否则用户看到的还是"未签到"）。
func (c *statusCache) invalidate(uid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, uid)
}

// accountQueryTimeout 单个上游查询的超时上限。
const accountQueryTimeout = 20 * time.Second

// fetchAccountStatus 并发查询单个账号的各项状态。
//
// 并发而非串行：4 项查询串行最坏 80s，并发后受最慢一项约束（约 20s）。
// 但只并发这 4 个 goroutine，不对外暴露并发度，避免用户刷 N 个账号时
// 对上游形成 N×4 的瞬时压力（前端逐个请求，天然限流）。
func (h *Handler) fetchAccountStatus(uid string) *AccountStatus {
	st := &AccountStatus{UID: uid, QueriedAt: time.Now()}
	acct := h.cfg.Pool.AuthByUID(uid)
	if acct == nil {
		st.Errors = map[string]string{"account": "账号不存在或已删除"}
		return st
	}
	st.Nickname = acct.Nickname

	var mu sync.Mutex
	var wg sync.WaitGroup
	fail := func(k, msg string) {
		mu.Lock()
		if st.Errors == nil {
			st.Errors = make(map[string]string, 3)
		}
		st.Errors[k] = msg
		mu.Unlock()
	}

	// 1. 配额：复用已有的 UserResource 聚合口径，但这里需要明细，故单独调。
	wg.Add(1)
	go func() {
		defer wg.Done()
		q, err := h.cfg.Upstream.QuotaDetail(acct)
		if err != nil {
			fail("quota", trimErrMsg(err))
			return
		}
		mu.Lock()
		st.Quota = q
		mu.Unlock()
	}()

	// 2. 签到状态。
	wg.Add(1)
	go func() {
		defer wg.Done()
		c, err := h.cfg.Upstream.CheckinStatus(acct)
		if err != nil {
			fail("checkin", trimErrMsg(err))
			return
		}
		mu.Lock()
		st.Checkin = c
		mu.Unlock()
	}()

	// 3. 可用模型。
	wg.Add(1)
	go func() {
		defer wg.Done()
		infos, err := h.cfg.Upstream.FetchModels(acct)
		if err != nil {
			fail("models", trimErrMsg(err))
			return
		}
		ids := make([]string, 0, len(infos))
		for _, m := range infos {
			ids = append(ids, m.ID)
		}
		mu.Lock()
		st.Models = ids
		mu.Unlock()
	}()

	wg.Wait()
	return st
}

// trimErrMsg 压缩错误文案：上游错误常带整段 JSON，前端卡片放不下。
func trimErrMsg(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// apiAccountStatus 深度查询账号状态。
//   GET /api/accounts/status?uid=xxx   单个账号
//   GET /api/accounts/status           全部账号
//   GET /api/accounts/status?uid=xxx&refresh=1  跳过缓存强制重查
func (h *Handler) apiAccountStatus(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	force := r.URL.Query().Get("refresh") == "1"
	now := time.Now()

	// 目标账号列表。
	var uids []string
	if uid != "" {
		uids = []string{uid}
	} else {
		for _, s := range h.cfg.Pool.List() {
			uids = append(uids, s.UID)
		}
	}

	out := make([]*AccountStatus, 0, len(uids))
	for _, u := range uids {
		if !force {
			if cached, ok := h.statusCache.get(u, now); ok {
				out = append(out, cached)
				continue
			}
		}
		st := h.fetchAccountStatus(u)
		h.statusCache.put(st)
		out = append(out, st)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"accounts": out,
		"total":    len(out),
		"cached":   !force,
	})
}

// apiCheckin 手动触发单个账号签到。
func (h *Handler) apiCheckin(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.URL.Query().Get("uid"))
	if uid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "缺少 uid 参数"})
		return
	}
	acct := h.cfg.Pool.AuthByUID(uid)
	if acct == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "message": "账号不存在"})
		return
	}

	err := h.cfg.Upstream.DailyCheckin(acct)
	switch {
	case err == nil:
		h.statusCache.invalidate(uid) // 签到成功：清缓存，下次查询能看到新状态
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "签到成功，积分 +100"})
	case upstream.IsAlreadyCheckin(err):
		h.statusCache.invalidate(uid)
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "今日已签到"})
	default:
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "message": trimErrMsg(err)})
	}
}

// accountPoolStatus 把 pool.Status 转成前端友好的形态。
//
// 存在的理由：pool.Status 是内部状态机口径（cooling/until/breaker_fails），
// 前端需要的是"能不能用 / 为什么不能用 / 什么时候恢复"。转换集中在此，
// 避免前端再猜字段名（此前前端引用了 in_cooldown 等 5 个不存在的字段）。
type accountView struct {
	pool.Status
	Usable      bool   `json:"usable"`                // 当前是否可接请求
	StateText   string `json:"state_text"`            // 人类可读状态：就绪/冷却中/已停用/熔断中
	CoolLeftSec int64  `json:"cool_left_sec,omitempty"` // 冷却剩余秒数
	Enterprise  string `json:"enterprise,omitempty"`   // 所属企业（空 = 个人）
	TokenLeftSec int64 `json:"token_left_sec,omitempty"` // access token 剩余有效秒数
	ExpiresAt   int64  `json:"expires_at,omitempty"`   // token 过期时间戳
}

// buildAccountViews 组装前端视图（纯内存读，零上游请求）。
func (h *Handler) buildAccountViews() []accountView {
	list := h.cfg.Pool.List()
	now := time.Now()
	out := make([]accountView, 0, len(list))
	for _, s := range list {
		v := accountView{Status: s}
		v.Usable = !s.Disabled && !s.Cooling && s.BreakerFails == 0

		switch {
		case s.Disabled:
			v.StateText = "已停用"
		case s.BreakerFails > 0:
			v.StateText = "熔断中"
		case s.Cooling:
			v.StateText = "冷却中"
		default:
			v.StateText = "就绪"
		}
		if s.Cooling && !s.Until.IsZero() {
			if left := int64(s.Until.Sub(now).Seconds()); left > 0 {
				v.CoolLeftSec = left
			}
		}

		if a := h.cfg.Pool.AuthByUID(s.UID); a != nil {
			v.Enterprise = a.EnterpriseID
			v.ExpiresAt = a.ExpiresAt
			if a.ExpiresAt > 0 {
				if left := a.ExpiresAt - now.Unix(); left > 0 {
					v.TokenLeftSec = left
				}
			}
		}
		out = append(out, v)
	}
	return out
}
