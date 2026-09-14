package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

// realmFake 捕获出站 chat 请求的 authz/model/path/messages 数的双域 fake upstream。
// GlobalEnabled=true（生产接线语义），base 假值无实际连接。
type realmFake struct {
	up *upstream.Client

	mu    sync.Mutex
	authz string
	model string
	path  string
	msgs  int
}

func newRealmFake(t *testing.T) *realmFake {
	t.Helper()
	cf := &realmFake{}
	cf.up = &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			cf.mu.Lock()
			defer cf.mu.Unlock()
			cf.authz = r.Header.Get("Authorization")
			cf.path = r.URL.Path
			raw, _ := io.ReadAll(r.Body)
			var req map[string]any
			_ = json.Unmarshal(raw, &req)
			cf.model, _ = req["model"].(string)
			if ms, ok := req["messages"].([]any); ok {
				cf.msgs = len(ms)
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:       "https://fake.cn",
		BillingBaseCN:    "https://fake.cn",
		ChatBaseGlobal:   "https://fake.global",
		BillingBaseGlobal: "https://fake.global",
		GlobalEnabled:    true,
	}
	return cf
}

// TestChatRealmSelectionAndBodyRewrite 断言 realm 贯穿：
// global: 前缀 → 全局号 + 出站 body 剥前缀 + /console 路径 + ensureConsoleSystem 补 system；
// 裸名 → CN 号 + /v2 路径 + body 原样（零回归）。
// TestModelsGlobalListGating 断言 global 名单的**双重门槛**：
// 既要有开关 GlobalEnabled=true，也要池中确有 global 账号。
//
// 行为变更说明（2026-09-14）：此前只看开关，池里没有 global 账号也会列出 21 个
// global: 名字（回落静态名单）。这让客户端看到选了必然 503 的"幽灵模型"。
// 现在要求「该 realm 有账号」才列出——CN 侧同理（无 cn 账号则不列 cn:）。
// CN 模型恒加 cn: 前缀。
func TestModelsGlobalListGating(t *testing.T) {
	// 关闭动态（无健康 CN 账号）→ 回退静态表，便于精确计数。
	resetModelsCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	up := upstream.New()
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true})
	data := h.modelList()

	hasGlobal := false
	hasCN := false
	for _, m := range data {
		id := m["id"].(string)
		if strings.HasPrefix(id, "global:") {
			hasGlobal = true
		}
		if id == "cn:glm-5.2" {
			hasCN = true
		}
	}
	if !hasCN {
		t.Error("cn:glm-5.2 missing from list")
	}
	// 该池只有 CN 号（uid=u1，无 domain → cn realm），故 global 不应出现。
	if hasGlobal {
		t.Error("池中无 global 账号：不应列出 global: 模型（避免不可用的幽灵模型）")
	}

	// GlobalEnabled=false（逃生门）同样不列 global。
	h2 := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})
	for _, m := range h2.modelList() {
		if strings.HasPrefix(m["id"].(string), "global:") {
			t.Fatalf("GlobalEnabled=false: global: model %q should not be listed", m["id"])
		}
	}
}

func TestChatRealmSelectionAndBodyRewrite(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	cf := newRealmFake(t)
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	// global: 前缀请求
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:gpt-5.4","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("global chat code=%d body=%s", rec.Code, rec.Body)
	}
	cf.mu.Lock()
	gotAuthz, gotModel, gotPath, gotMsgs := cf.authz, cf.model, cf.path, cf.msgs
	cf.mu.Unlock()
	if gotAuthz != "Bearer at_gl" {
		t.Errorf("global chat authz=%q want g1 Bearer", gotAuthz)
	}
	if gotModel != "gpt-5.4" {
		t.Errorf("global chat outbound model=%q want gpt-5.4 (prefix stripped)", gotModel)
	}
	if gotPath != "/console/chat/completions" {
		t.Errorf("global chat path=%q want /console/chat/completions", gotPath)
	}
	if gotMsgs != 2 { // ensureConsoleSystem：user 前置补 system
		t.Errorf("global chat messages=%d want 2 (system fallback)", gotMsgs)
	}

	// 裸名请求（CN 零回归）
	cf.mu.Lock()
	cf.authz, cf.model, cf.path, cf.msgs = "", "", "", 0
	cf.mu.Unlock()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("cn chat code=%d body=%s", rec.Code, rec.Body)
	}
	cf.mu.Lock()
	gotAuthz, gotModel, gotPath, gotMsgs = cf.authz, cf.model, cf.path, cf.msgs
	cf.mu.Unlock()
	if gotAuthz != "Bearer at_cn" {
		t.Errorf("cn chat authz=%q want cn1 Bearer", gotAuthz)
	}
	if gotModel != "glm-5.2" {
		t.Errorf("cn chat outbound model=%q want glm-5.2", gotModel)
	}
	if gotPath != "/v2/chat/completions" {
		t.Errorf("cn chat path=%q want /v2/chat/completions", gotPath)
	}
	if gotMsgs != 1 { // CN 不补 system
		t.Errorf("cn chat messages=%d want 1 (no system fallback)", gotMsgs)
	}
}

// TestChatRealmGlobalAbortedNoGlobalAccount 逃生门双保险：默认开启后，仅池里没有 global
// 账号时，global: 前缀请求无可选号 → 503（不跨 realm 用 CN 号顶上）。
func TestChatRealmGlobalAbortedNoGlobalAccount(t *testing.T) {
	cf := newRealmFake(t)
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:gpt-5.4","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no global account: code=%d want 503 (must not fall back to cn)", rec.Code)
	}
}

// TestChatRealmGlobalDisabledEscapeHatch 逃生门：config GlobalEnabled=false 时，即便默认
// 开启（auth.Realm() 判 global 账号），也没有 global 上游可路由，请求回落 CN 路径（200，
// 不崩溃、不泄漏 global 凭据）。
func TestChatRealmGlobalDisabledEscapeHatch(t *testing.T) {
	cf := newRealmFake(t)
	cf.up.GlobalEnabled = false
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: false})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:gpt-5.4","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("global disabled escape hatch: code=%d want 200 (routes CN path)", rec.Code)
	}
	cf.mu.Lock()
	gotPath := cf.path
	cf.mu.Unlock()
	if gotPath != "/v2/chat/completions" {
		t.Errorf("global disabled escape hatch: path=%q want CN /v2/chat/completions", gotPath)
	}
}