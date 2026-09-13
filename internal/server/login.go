// login.go 控制台登录会话：给 WebUI 与后台管理接口加一道密码门。
//
// 设计取舍（KISS）：
//   - 会话仅保存在内存（进程重启即失效，比落盘更安全，也少一份待清理的文件）；
//   - 密码以明文存于 config.json，靠文件权限（0600）保护，用户可见可改；
//   - 密码为空时整条逻辑短路，行为与旧版本完全一致（不破坏既有部署与测试）；
//   - /v1/* 走独立的 Bearer api_key 鉴权，不归本门管辖；
//   - /healthz 保持开放，供前端探活与负载均衡使用（仅暴露 healthy/total）。
package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// sessionCookieName 控制台登录态的 Cookie 名。
	sessionCookieName = "wb_session"
	// sessionTTL 登录态有效期：7 天。
	sessionTTL = 7 * 24 * time.Hour
	// loginMaxFails 单 IP 连续失败上限，达到即锁定。
	loginMaxFails = 5
	// loginLockDur 达到失败上限后的锁定时长（防公网暴力破解）。
	loginLockDur = 5 * time.Minute
	// loginBodyLimit 登录请求体上限，防超大 body 打爆内存。
	loginBodyLimit = 1 << 12
)

// authGate 控制台登录会话管理。password 为空 = 不启用登录。
type authGate struct {
	mu       sync.Mutex
	password string
	sessions map[string]time.Time // token → 过期时刻
	fails    map[string]*failState
}

// failState 单个来源 IP 的失败计数与锁定状态。
type failState struct {
	count int
	until time.Time
}

func newAuthGate(password string) *authGate {
	// 自行 trim：空白密码若被当成有效密码，会出现"设了密码却人人可登录"的假安全，
	// 所以这里做一次归一，不依赖调用方（config 层）是否已经处理过。
	if strings.TrimSpace(password) == "" {
		password = ""
	}
	return &authGate{
		password: password,
		sessions: make(map[string]time.Time),
		fails:    make(map[string]*failState),
	}
}

// enabled 是否启用了登录门。
func (g *authGate) enabled() bool {
	return g != nil && g.password != ""
}

// openPath 判断某路径是否无需登录即可访问。
func (g *authGate) openPath(p string) bool {
	switch {
	case p == "/healthz",
		p == "/login.html",
		p == "/style.css",
		p == "/favicon.ico",
		p == "/api/login",
		// 供前端查询登录态 / 退出，本身不泄露任何业务数据。
		p == "/api/session",
		p == "/api/logout",
		// 登录页需要加载的前端依赖。
		strings.HasPrefix(p, "/assets/"),
		// OpenAI 兼容接口走独立的 Bearer api_key 鉴权。
		strings.HasPrefix(p, "/v1/"):
		return true
	}
	return false
}

// verify 校验密码。用 SHA-256 归一长度后再比对，避免长度侧信道，并做定时安全比较。
func (g *authGate) verify(password string) bool {
	want := sha256.Sum256([]byte(g.password))
	got := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// locked 返回该 IP 是否处于锁定中；顺带做惰性过期清理。
// 注意：只有"锁定期已过"才清零计数；未达上限时计数必须累积，否则限流形同虚设。
func (g *authGate) locked(ip string, now time.Time) (bool, time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.fails[ip]
	if !ok {
		return false, 0
	}
	if f.count >= loginMaxFails {
		if now.Before(f.until) {
			return true, f.until.Sub(now)
		}
		// 锁定期已过：清零，给新的尝试机会。
		delete(g.fails, ip)
		return false, 0
	}
	return false, 0
}

// recordFail 记一次失败，达到上限则进入锁定期。
func (g *authGate) recordFail(ip string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.fails[ip]
	if !ok {
		f = &failState{}
		g.fails[ip] = f
	} else if !f.until.IsZero() && now.After(f.until) {
		// 上一轮锁定期已过：从零重新计数，避免历史失败永久叠加。
		f.count = 0
		f.until = time.Time{}
	}
	f.count++
	if f.count >= loginMaxFails {
		f.until = now.Add(loginLockDur)
	}
}

// clearFails 登录成功后清除该 IP 的失败记录。
func (g *authGate) clearFails(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fails, ip)
}

// issue 生成并登记一个新会话 token。
func (g *authGate) issue(now time.Time) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)

	g.mu.Lock()
	defer g.mu.Unlock()
	// 惰性清理过期会话，避免 map 无限增长。
	for t, exp := range g.sessions {
		if now.After(exp) {
			delete(g.sessions, t)
		}
	}
	g.sessions[token] = now.Add(sessionTTL)
	return token, nil
}

// valid 判断请求携带的会话是否有效。
func (g *authGate) valid(r *http.Request, now time.Time) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	exp, ok := g.sessions[c.Value]
	if !ok {
		return false
	}
	if now.After(exp) {
		delete(g.sessions, c.Value)
		return false
	}
	return true
}

// revoke 注销某个会话 token。
func (g *authGate) revoke(token string) {
	if token == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.sessions, token)
}

// clientIP 取来源 IP（去掉端口）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// wantsHTML 判断请求是不是浏览器直接导航（需要跳登录页而非返回 JSON 401）。
func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// gateCheck 在进入 mux 之前的统一拦截。返回 true 表示已处理并应中断请求。
func (h *Handler) gateCheck(w http.ResponseWriter, r *http.Request) bool {
	g := h.authGate
	if !g.enabled() || g.openPath(r.URL.Path) {
		return false
	}
	now := time.Now()
	if g.valid(r, now) {
		return false
	}
	// 未登录：浏览器导航跳登录页，接口请求返回 401 JSON。
	if wantsHTML(r) {
		http.Redirect(w, r, "/login.html", http.StatusFound)
		return true
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{
		"success":        false,
		"login_required": true,
		"message":        "登录已过期，请重新登录",
	})
	return true
}

// apiLogin 校验密码并下发会话 Cookie。
func (h *Handler) apiLogin(w http.ResponseWriter, r *http.Request) {
	g := h.authGate
	if !g.enabled() {
		// 未配置密码时，直接放行并告知前端无需登录，避免前端卡在登录页。
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "login_required": false})
		return
	}

	now := time.Now()
	ip := clientIP(r)
	if locked, left := g.locked(ip, now); locked {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"success": false,
			"message": "失败次数过多，请 " + left.Round(time.Second).String() + " 后再试",
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, loginBodyLimit)
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "请求格式有误"})
		return
	}

	if !g.verify(body.Password) {
		g.recordFail(ip, now)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "message": "密码不正确"})
		return
	}

	token, err := g.issue(now)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "生成登录态失败"})
		return
	}
	g.clearFails(ip)

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true, // 禁止 JS 读取，降低 XSS 窃取风险
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL / time.Second),
	})
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "login_required": true})
}

// apiLogout 注销当前会话。
func (h *Handler) apiLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		h.authGate.revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// apiSession 供前端查询当前是否已登录。
func (h *Handler) apiSession(w http.ResponseWriter, r *http.Request) {
	g := h.authGate
	if !g.enabled() {
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "login_required": false, "logged_in": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":        true,
		"login_required": true,
		"logged_in":      g.valid(r, time.Now()),
	})
}
