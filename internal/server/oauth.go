// oauth.go 网页端 CodeBuddy OAuth 登录：生成登录链接 → 轮询 → 落凭证文件。
//
// 流程（对齐官方 CLI 插件协议，已实测可用）：
//  1. POST /v2/plugin/auth/state?platform=CLI&nonce=xxx  → 返回 { state, authUrl }
//  2. 用户在浏览器打开 authUrl 完成登录
//  3. GET  /v2/plugin/auth/token?state=xxx               → 返回 accessToken/refreshToken
//  4. 写入 auths/workbuddy-<uid>.json 并热重载
//
// 轮询由前端驱动（每次调 /api/credentials/oauth/poll 查一次），不在服务端起后台
// 协程：路由器资源有限，且用户随时可能关掉页面，服务端常驻轮询是纯浪费。
package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/upstream"
)

const (
	// oauthStatePath / oauthTokenPath OAuth 协议端点（billing 域）。
	oauthStatePath = "/v2/plugin/auth/state"
	oauthTokenPath = "/v2/plugin/auth/token"

	// oauthSessionTTL 登录会话有效期，超时后 poll 会明确失败而不是无限等待。
	oauthSessionTTL = 30 * time.Minute

	// oauthPendingCode 上游表示"用户还没完成登录"的业务码。
	oauthPendingCode = 11217
)

// oauthSession 一次待完成的登录（仅存内存，重启即失效）。
type oauthSession struct {
	State     string
	Nonce     string
	AuthURL   string
	CreatedAt time.Time
}

// apiOAuthStart 申请一个登录链接。
func (h *Handler) apiOAuthStart(w http.ResponseWriter, r *http.Request) {
	nonce, err := randToken(16)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "随机数生成失败"})
		return
	}

	path := fmt.Sprintf("%s?platform=CLI&nonce=%s", oauthStatePath, nonce)
	data, err := h.cfg.Upstream.OAuthJSON(r.Context(), path, http.MethodPost, map[string]any{"nonce": nonce})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "message": "申请登录链接失败: " + trimErrMsg(err)})
		return
	}

	var resp struct {
		Code int `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			State   string `json:"state"`
			AuthURL string `json:"authUrl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "message": "上游响应解析失败"})
		return
	}
	if resp.Code != 0 || resp.Data.State == "" || resp.Data.AuthURL == "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "message": "上游拒绝了申请: " + resp.Msg})
		return
	}

	h.oauthMu.Lock()
	// 惰性清理过期会话，避免 map 无限增长。
	now := time.Now()
	for s, sess := range h.oauthSessions {
		if now.Sub(sess.CreatedAt) > oauthSessionTTL {
			delete(h.oauthSessions, s)
		}
	}
	h.oauthSessions[resp.Data.State] = &oauthSession{
		State:     resp.Data.State,
		Nonce:     nonce,
		AuthURL:   resp.Data.AuthURL,
		CreatedAt: now,
	}
	h.oauthMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"success":     true,
		"auth_state":  resp.Data.State,
		"auth_url":    resp.Data.AuthURL,
		"expires_in":  int(oauthSessionTTL / time.Second),
		"message":     "请在浏览器打开链接完成登录，然后回到本页点击「我已登录」",
	})
}

// apiOAuthPoll 查询登录是否完成；完成后落盘凭证。
func (h *Handler) apiOAuthPoll(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var body struct {
		AuthState string `json:"auth_state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "请求格式有误"})
		return
	}
	state := strings.TrimSpace(body.AuthState)
	if state == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "缺少 auth_state"})
		return
	}

	h.oauthMu.Lock()
	sess, ok := h.oauthSessions[state]
	h.oauthMu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "message": "登录会话不存在或已过期，请重新发起"})
		return
	}
	if time.Since(sess.CreatedAt) > oauthSessionTTL {
		h.oauthMu.Lock()
		delete(h.oauthSessions, state)
		h.oauthMu.Unlock()
		writeJSON(w, http.StatusGone, map[string]any{"success": false, "message": "登录会话已超时，请重新发起"})
		return
	}

	data, err := h.cfg.Upstream.OAuthJSON(r.Context(), oauthTokenPath+"?state="+state, http.MethodGet, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "message": "查询登录状态失败: " + trimErrMsg(err)})
		return
	}

	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int64  `json:"expiresIn"`
			Domain       string `json:"domain"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
			UID          string `json:"uid"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "message": "上游响应解析失败"})
		return
	}

	// 用户还没点完登录：正常状态，前端继续轮询。
	if resp.Code == oauthPendingCode {
		writeJSON(w, http.StatusOK, map[string]any{
			"success":  true,
			"pending":  true,
			"message":  "等待你在浏览器完成登录…",
		})
		return
	}
	if resp.Code != 0 {
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "message": "登录失败: " + resp.Msg})
		return
	}
	if strings.TrimSpace(resp.Data.AccessToken) == "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"success": false, "message": "上游未返回 accessToken"})
		return
	}

	// 落盘凭证。
	//
	// UID 优先从 JWT 的 sub 提取（实测等于凭证文件里的 account.uid）：
	// 同一账号重复登录必须落到同一个文件名，否则池里会出现两条指向同一
	// 腾讯账号的记录，调度器会当两个号轮换，白白消耗额度。
	apiFields := map[string]any{
		"uid":          resp.Data.UID,
		"nickname":     resp.Data.Nickname,
		"enterpriseId": resp.Data.EnterpriseID,
	}
	ident := upstream.ParseIdentity(resp.Data.AccessToken, apiFields)

	a := &auth.Auth{
		AccessToken:  resp.Data.AccessToken,
		RefreshToken: resp.Data.RefreshToken,
		Domain:       resp.Data.Domain,
		UID:          ident.UID,
		Nickname:     ident.Nickname,
		EnterpriseID: ident.EnterpriseID,
	}
	if a.Domain == "" {
		a.Domain = "www.codebuddy.cn"
	}
	if resp.Data.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Unix() + resp.Data.ExpiresIn
	}
	// 兜底：JWT 与 API 都没给出标识时，用 token 哈希派生（保证至少稳定且可区分）。
	if a.UID == "" {
		a.UID = "oauth-" + shortHash(a.AccessToken)
	}
	a.FilePath = filepath.Join(h.authDir(), credFileName(a))

	if err := a.SaveAtomic(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "保存凭证失败: " + err.Error()})
		return
	}
	if err := h.reloadPool(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "凭证已保存，但重载失败: " + err.Error()})
		return
	}

	// 登录成功：会话用完即弃。
	h.oauthMu.Lock()
	delete(h.oauthSessions, state)
	h.oauthMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"success":  true,
		"pending":  false,
		"message":  "登录成功，账号已加入池中",
		"uid":      a.UID,
		"nickname": a.Nickname,
	})
}

// randToken 生成 n 字节的随机 URL-safe 字符串。
func randToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
