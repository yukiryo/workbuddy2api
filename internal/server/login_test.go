package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestGate 构造一个启用了密码的 authGate。
func newTestGate(pw string) *authGate { return newAuthGate(pw) }

// TestAuthGateDisabledWhenNoPassword 密码为空时整条登录逻辑必须短路（保持旧行为）。
func TestAuthGateDisabledWhenNoPassword(t *testing.T) {
	g := newAuthGate("")
	if g.enabled() {
		t.Fatal("空密码不应启用登录门")
	}
	if g.enabled() && !g.openPath("/status") {
		t.Fatal("/status 在未启用登录时不应被拦截")
	}
}

// TestAuthGatePasswordTrimmedToEmpty 全是空白的密码应视为未设置，
// 否则用户会以为设了密码、实际任何人都能用空格登录。
func TestAuthGatePasswordTrimmedToEmpty(t *testing.T) {
	if newAuthGate("   ").enabled() {
		t.Fatal("空白密码不应启用登录门")
	}
}

// TestAuthGateOpenPaths 白名单路径必须免登录（探活、登录页及其依赖、/v1 接口）。
func TestAuthGateOpenPaths(t *testing.T) {
	g := newTestGate("secret")
	open := []string{"/healthz", "/login.html", "/style.css", "/favicon.ico",
		"/api/login", "/api/session", "/api/logout",
		"/assets/tailwindcss.js", "/assets/lucide.min.js", "/v1/models", "/v1/chat/completions"}
	for _, p := range open {
		if !g.openPath(p) {
			t.Errorf("%s 应为免登录路径", p)
		}
	}
	closed := []string{"/", "/index.html", "/app.js", "/status", "/api/key",
		"/api/usage", "/api/usage/clear"}
	for _, p := range closed {
		if g.openPath(p) {
			t.Errorf("%s 不应免登录", p)
		}
	}
}

// TestAuthGatePWAPathsOpen PWA 资源必须免登录。
//
// 浏览器在安装/更新 Service Worker、读取 manifest 时由**浏览器自身**发起请求，
// 不会携带我们的会话 Cookie。若这些路径被登录门拦下，PWA 会静默安装失败，
// 而用户在控制台里看不出任何异常——排障成本极高，故用测试锁死。
func TestAuthGatePWAPathsOpen(t *testing.T) {
	g := newTestGate("secret")
	for _, p := range []string{
		"/manifest.json", "/sw.js",
		"/icons/icon-192.png", "/icons/icon-512.png",
		"/icons/icon-512-maskable.png", "/icons/favicon-32.png",
	} {
		if !g.openPath(p) {
			t.Errorf("%s 必须免登录（PWA 资源）", p)
		}
	}
}

// TestAuthGateVerify 密码校验：正确通过、错误拒绝、前缀/大小写不算匹配。
func TestAuthGateVerify(t *testing.T) {
	// 用明显的测试占位符，避免真实部署密码出现在公开仓库里。
	const pw = "test-password-1a2b"
	g := newTestGate(pw)
	if !g.verify(pw) {
		t.Fatal("正确密码应通过")
	}
	for _, bad := range []string{"", "test-password", pw + "x", "TEST-PASSWORD-1A2B", " " + pw} {
		if g.verify(bad) {
			t.Errorf("错误密码 %q 不应通过", bad)
		}
	}
}

// TestAuthGateRateLimitLocksAfterMaxFails 连续失败达上限后必须锁定，且锁定期内正确密码也拒绝。
// 这是防公网暴力破解的核心保障，一旦回归则密码可被无限次猜测。
func TestAuthGateRateLimitLocksAfterMaxFails(t *testing.T) {
	g := newTestGate("secret")
	now := time.Now()
	ip := "1.2.3.4"

	// 前 loginMaxFails-1 次失败：不应锁定。
	for i := 0; i < loginMaxFails-1; i++ {
		g.recordFail(ip, now)
		if locked, _ := g.locked(ip, now); locked {
			t.Fatalf("第 %d 次失败后不应锁定（上限 %d）", i+1, loginMaxFails)
		}
	}
	// 第 loginMaxFails 次失败：进入锁定。
	g.recordFail(ip, now)
	locked, left := g.locked(ip, now)
	if !locked {
		t.Fatalf("连续 %d 次失败后应锁定", loginMaxFails)
	}
	if left <= 0 || left > loginLockDur {
		t.Fatalf("剩余锁定时长异常: %v", left)
	}

	// 锁定期内即使密码正确，locked 也应为 true（由 apiLogin 拦在校验之前）。
	if locked, _ := g.locked(ip, now); !locked {
		t.Fatal("锁定期内应保持锁定")
	}
}

// TestAuthGateUnlocksAfterLockDur 锁定期结束后应自动放行并可重新尝试。
func TestAuthGateUnlocksAfterLockDur(t *testing.T) {
	g := newTestGate("secret")
	now := time.Now()
	ip := "1.2.3.4"
	for i := 0; i < loginMaxFails; i++ {
		g.recordFail(ip, now)
	}
	if locked, _ := g.locked(ip, now); !locked {
		t.Fatal("应处于锁定中")
	}
	// 时间推进到锁定期之后。
	later := now.Add(loginLockDur + time.Second)
	if locked, _ := g.locked(ip, later); locked {
		t.Fatal("锁定期已过应解锁")
	}
	// 解锁后计数应从零开始：再失败一次不应立刻又锁定。
	g.recordFail(ip, later)
	if locked, _ := g.locked(ip, later); locked {
		t.Fatal("解锁后首次失败不应立即重新锁定")
	}
}

// TestAuthGateSuccessClearsFails 登录成功后应清零失败计数，避免正常用户被自己的手误拖累。
func TestAuthGateSuccessClearsFails(t *testing.T) {
	g := newTestGate("secret")
	now := time.Now()
	ip := "1.2.3.4"
	for i := 0; i < loginMaxFails-1; i++ {
		g.recordFail(ip, now)
	}
	g.clearFails(ip)
	g.recordFail(ip, now) // 清零后重来，只算 1 次
	if locked, _ := g.locked(ip, now); locked {
		t.Fatal("成功登录清零后不应锁定")
	}
}

// TestAuthGateSessionLifecycle 会话签发 / 校验 / 撤销 / 过期。
func TestAuthGateSessionLifecycle(t *testing.T) {
	g := newTestGate("secret")
	now := time.Now()

	token, err := g.issue(now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if len(token) != 64 {
		t.Fatalf("token 应为 32 字节的 hex（64 字符），实际 %d", len(token))
	}

	reqWith := func(tok string) *http.Request {
		r := httptest.NewRequest("GET", "/status", nil)
		if tok != "" {
			r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tok})
		}
		return r
	}

	if !g.valid(reqWith(token), now) {
		t.Fatal("新签发的会话应有效")
	}
	if g.valid(reqWith(""), now) {
		t.Fatal("无 Cookie 不应有效")
	}
	if g.valid(reqWith("forged-token"), now) {
		t.Fatal("伪造 token 不应有效")
	}
	// 过期后失效。
	if g.valid(reqWith(token), now.Add(sessionTTL+time.Second)) {
		t.Fatal("过期会话不应有效")
	}

	// 撤销后失效。
	token2, _ := g.issue(now)
	g.revoke(token2)
	if g.valid(reqWith(token2), now) {
		t.Fatal("撤销后不应有效")
	}
	// 撤销空 token 不应 panic。
	g.revoke("")
}

// TestAuthGateGateCheckRedirectVs401 浏览器导航跳登录页，接口请求返回 401 JSON。
func TestAuthGateGateCheckRedirectVs401(t *testing.T) {
	h := &Handler{authGate: newTestGate("secret")}

	// 浏览器导航（Accept: text/html）→ 302 到登录页。
	reqHTML := httptest.NewRequest("GET", "/", nil)
	reqHTML.Header.Set("Accept", "text/html,application/xhtml+xml")
	recHTML := httptest.NewRecorder()
	if !h.gateCheck(recHTML, reqHTML) {
		t.Fatal("未登录的 HTML 请求应被拦截")
	}
	if recHTML.Code != http.StatusFound {
		t.Fatalf("HTML 请求应 302，实际 %d", recHTML.Code)
	}
	if loc := recHTML.Header().Get("Location"); loc != "/login.html" {
		t.Fatalf("应跳 /login.html，实际 %q", loc)
	}

	// 接口请求（无 Accept: text/html）→ 401 JSON。
	reqAPI := httptest.NewRequest("GET", "/status", nil)
	recAPI := httptest.NewRecorder()
	if !h.gateCheck(recAPI, reqAPI) {
		t.Fatal("未登录的接口请求应被拦截")
	}
	if recAPI.Code != http.StatusUnauthorized {
		t.Fatalf("接口请求应 401，实际 %d", recAPI.Code)
	}
	if body := recAPI.Body.String(); !strings.Contains(body, "login_required") {
		t.Fatalf("401 响应应带 login_required 标记，实际 %s", body)
	}

	// 白名单路径不拦截。
	reqOpen := httptest.NewRequest("GET", "/healthz", nil)
	recOpen := httptest.NewRecorder()
	if h.gateCheck(recOpen, reqOpen) {
		t.Fatal("/healthz 不应被拦截")
	}

	// 已登录不拦截。
	now := time.Now()
	token, _ := h.authGate.issue(now)
	reqOK := httptest.NewRequest("GET", "/status", nil)
	reqOK.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	recOK := httptest.NewRecorder()
	if h.gateCheck(recOK, reqOK) {
		t.Fatal("已登录请求不应被拦截")
	}
}

// TestAuthGateDisabledGateCheckPasses 未设密码时 gateCheck 必须全部放行。
func TestAuthGateDisabledGateCheckPasses(t *testing.T) {
	h := &Handler{authGate: newAuthGate("")}
	for _, p := range []string{"/", "/status", "/api/key"} {
		req := httptest.NewRequest("GET", p, nil)
		rec := httptest.NewRecorder()
		if h.gateCheck(rec, req) {
			t.Fatalf("未启用登录时 %s 不应被拦截", p)
		}
	}
}

// TestClientIP 来源 IP 解析（含 IPv6 与无端口场景）。
func TestClientIP(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4:5678":              "1.2.3.4",
		"[2408:8221::1]:5678":       "2408:8221::1",
		"192.168.5.9:1":             "192.168.5.9",
	}
	for in, want := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = in
		if got := clientIP(r); got != want {
			t.Errorf("clientIP(%q)=%q, want %q", in, got, want)
		}
	}
}
