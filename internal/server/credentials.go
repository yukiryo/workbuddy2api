// credentials.go 凭证管理：列表 / 上传 / 删除 / 重载 / OAuth 网页登录。
//
// 设计约束：所有写操作都落到 auths/ 目录下的 workbuddy*.json 文件（单一事实来源），
// 然后调 pool.SyncToDir 让内存池对齐。不引入数据库、不维护第二份状态。
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// credentialView 凭证的对外视图（脱敏：永不返回 token 原文）。
type credentialView struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	Domain       string `json:"domain,omitempty"`
	FileName     string `json:"file_name"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	TokenLeftSec int64  `json:"token_left_sec,omitempty"`
	TokenState   string `json:"token_state"`         // valid / expiring / expired / unknown
	HasRefresh   bool   `json:"has_refresh"`         // 是否带 refreshToken（决定能否自动续期）
	FileSize     int64  `json:"file_size,omitempty"`
}

// authDir 返回凭证目录（与 main.go 注入的保持一致）。
func (h *Handler) authDir() string {
	if h.cfg.AuthDir != "" {
		return h.cfg.AuthDir
	}
	return "/etc/workbuddy2api/auths"
}

// apiCredentials 列出全部凭证文件（含未成功解析的，便于用户发现坏文件）。
func (h *Handler) apiCredentials(w http.ResponseWriter, r *http.Request) {
	dir := h.authDir()
	files, _ := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	now := time.Now()

	views := make([]credentialView, 0, len(files))
	broken := make([]map[string]string, 0)

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			broken = append(broken, map[string]string{"file_name": filepath.Base(f), "reason": "读取失败"})
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			broken = append(broken, map[string]string{"file_name": filepath.Base(f), "reason": "格式错误，无法解析"})
			continue
		}
		v := credentialView{
			UID:          a.UID,
			Nickname:     a.Nickname,
			EnterpriseID: a.EnterpriseID,
			Domain:       a.Domain,
			FileName:     filepath.Base(f),
			ExpiresAt:    a.ExpiresAt,
			HasRefresh:   strings.TrimSpace(a.RefreshToken) != "",
		}
		if fi, err := os.Stat(f); err == nil {
			v.FileSize = fi.Size()
		}
		// token 剩余有效期与状态。
		switch {
		case a.ExpiresAt <= 0:
			v.TokenState = "unknown"
		default:
			left := a.ExpiresAt - now.Unix()
			v.TokenLeftSec = left
			switch {
			case left <= 0:
				v.TokenState = "expired"
			case left < 3600:
				v.TokenState = "expiring"
			default:
				v.TokenState = "valid"
			}
		}
		views = append(views, v)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Nickname < views[j].Nickname })

	writeJSON(w, http.StatusOK, map[string]any{
		"success":     true,
		"credentials": views,
		"total":       len(views),
		"broken":      broken,
		"auth_dir":    dir,
	})
}

// apiCredentialUpload 手动添加/覆盖一个凭证（粘贴 accessToken，可选 refreshToken）。
func (h *Handler) apiCredentialUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		UID          string `json:"uid"`
		Nickname     string `json:"nickname"`
		EnterpriseID string `json:"enterprise_id"`
		Domain       string `json:"domain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "请求格式有误"})
		return
	}
	body.AccessToken = strings.TrimSpace(body.AccessToken)
	if body.AccessToken == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "accessToken 不能为空"})
		return
	}
	if body.Domain == "" {
		body.Domain = "www.codebuddy.cn"
	}

	a := &auth.Auth{
		AccessToken:  body.AccessToken,
		RefreshToken: strings.TrimSpace(body.RefreshToken),
		Domain:       body.Domain,
		UID:          strings.TrimSpace(body.UID),
		Nickname:     strings.TrimSpace(body.Nickname),
		EnterpriseID: strings.TrimSpace(body.EnterpriseID),
	}
	// 未提供 UID 时用 token 的哈希派生一个稳定标识，避免同名文件互相覆盖。
	if a.UID == "" {
		a.UID = "manual-" + shortHash(body.AccessToken)
	}
	a.FilePath = filepath.Join(h.authDir(), credFileName(a))

	if err := a.SaveAtomic(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "写入失败: " + err.Error()})
		return
	}
	if err := h.reloadPool(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "重载失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "凭证已保存并加载", "uid": a.UID})
}

// apiCredentialDelete 删除凭证文件并从池中移除。
func (h *Handler) apiCredentialDelete(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var body struct {
		FileName string `json:"file_name"`
		UID      string `json:"uid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "请求格式有误"})
		return
	}

	target := strings.TrimSpace(body.FileName)
	// 未给文件名时按 UID 反查。
	if target == "" {
		if strings.TrimSpace(body.UID) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "需提供 file_name 或 uid"})
			return
		}
		target = h.findFileByUID(body.UID)
		if target == "" {
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "message": "找不到该账号的凭证文件"})
			return
		}
	}

	// 路径安全：只允许删除 auths 目录下的 workbuddy*.json，杜绝 ../ 穿越。
	base := filepath.Base(target)
	if base != target || !strings.HasPrefix(base, "workbuddy") || !strings.HasSuffix(base, ".json") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "文件名不合法"})
		return
	}
	full := filepath.Join(h.authDir(), base)
	if err := os.Remove(full); err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "message": "文件不存在"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "删除失败: " + err.Error()})
		return
	}
	if err := h.reloadPool(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "重载失败: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "凭证已删除"})
}

// apiCredentialReload 重新扫描 auths/ 目录并热更新内存池（新增/删除/换 token 都靠它生效）。
func (h *Handler) apiCredentialReload(w http.ResponseWriter, r *http.Request) {
	if err := h.reloadPool(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": err.Error()})
		return
	}
	_, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": fmt.Sprintf("已重新加载，当前 %d 个账号可用", healthy),
		"healthy": healthy,
	})
}

// reloadPool 重扫 auths 目录并对齐内存池。
func (h *Handler) reloadPool() error {
	auths, err := auth.LoadDir(h.authDir())
	if err != nil {
		return fmt.Errorf("扫描凭证目录失败: %w", err)
	}
	h.cfg.Pool.SyncToDir(auths)
	return nil
}

// findFileByUID 按 UID 反查凭证文件名（空 = 未找到）。
func (h *Handler) findFileByUID(uid string) string {
	files, _ := filepath.Glob(filepath.Join(h.authDir(), "workbuddy*.json"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		if a.UID == uid {
			return filepath.Base(f)
		}
	}
	return ""
}

// credFileName 生成凭证文件名：与插件 OAuth 输出同构（workbuddy-<uid>.json）。
func credFileName(a *auth.Auth) string {
	return "workbuddy-" + sanitizeFilePart(a.UID) + ".json"
}

// sanitizeFilePart 把 UID 里不适合做文件名的字符替换掉，防路径穿越与奇怪字符。
func sanitizeFilePart(s string) string {
	if s == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// shortHash 取 sha256 前 12 位十六进制，用于无 UID 时的稳定标识。
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}
