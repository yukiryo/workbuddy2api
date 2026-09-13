package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
)

// writeCredFile 在 dir 下写一个凭证文件，返回文件路径。
func writeCredFile(t *testing.T, dir, name string, a *auth.Auth) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	full := filepath.Join(dir, name)
	a.FilePath = full
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	return full
}

// TestSanitizeFilePart 文件名净化必须挡掉路径穿越与奇怪字符。
func TestSanitizeFilePart(t *testing.T) {
	cases := map[string]string{
		"../../etc/passwd": "etc-passwd",
		"abc-123_XYZ":      "abc-123_XYZ",
		"a/b\\c":           "a-b-c",
		"":                 "unknown",
		"///":              "unknown",
	}
	for in, want := range cases {
		if got := sanitizeFilePart(in); got != want {
			t.Errorf("sanitizeFilePart(%q)=%q want %q", in, got, want)
		}
	}
	// 超长 UID 必须被截断，避免文件名超限。
	long := strings.Repeat("a", 200)
	if got := sanitizeFilePart(long); len(got) > 64 {
		t.Errorf("超长 UID 应截断到 64，实际 %d", len(got))
	}
}

// TestShortHashStable 同输入必须得到同哈希（否则无 UID 的凭证会每次生成新文件）。
func TestShortHashStable(t *testing.T) {
	a := shortHash("token-abc")
	b := shortHash("token-abc")
	c := shortHash("token-abd")
	if a != b {
		t.Error("同输入应得到同哈希")
	}
	if a == c {
		t.Error("不同输入不应碰撞")
	}
	if len(a) != 12 {
		t.Errorf("哈希长度应为 12，实际 %d", len(a))
	}
}

// TestCredFileNameHasPrefix credFileName 产出的名字必须能被 LoadDir 的 glob 命中。
func TestCredFileNameHasPrefix(t *testing.T) {
	got := credFileName(&auth.Auth{UID: "abc-123"})
	if got != "workbuddy-abc-123.json" {
		t.Errorf("credFileName=%q", got)
	}
	if !strings.HasPrefix(got, "workbuddy") || !strings.HasSuffix(got, ".json") {
		t.Error("文件名必须匹配 LoadDir 的 workbuddy*.json 约定")
	}
}

// TestApiCredentialsListsFiles 列表接口应返回已存凭证且**不含 token 原文**。
func TestApiCredentialsListsFiles(t *testing.T) {
	dir := t.TempDir()
	writeCredFile(t, dir, "workbuddy-u1.json", &auth.Auth{
		AccessToken: "SECRET-TOKEN-VALUE", RefreshToken: "SECRET-REFRESH",
		UID: "u1", Nickname: "雪凌", Domain: "www.codebuddy.cn",
	})

	h := &Handler{cfg: Config{AuthDir: dir}}
	rec := httptest.NewRecorder()
	h.apiCredentials(rec, httptest.NewRequest("GET", "/api/credentials", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 %d", rec.Code)
	}
	body := rec.Body.String()
	// 脱敏是本接口的红线：泄露 token 等于把账号交出去。
	if strings.Contains(body, "SECRET-TOKEN-VALUE") || strings.Contains(body, "SECRET-REFRESH") {
		t.Fatal("响应泄露了 token 原文！")
	}

	var resp struct {
		Success     bool `json:"success"`
		Total       int  `json:"total"`
		Credentials []struct {
			UID       string `json:"uid"`
			Nickname  string `json:"nickname"`
			FileName  string `json:"file_name"`
			TokenState string `json:"token_state"`
			HasRefresh bool  `json:"has_refresh"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if !resp.Success || resp.Total != 1 {
		t.Fatalf("success=%v total=%d", resp.Success, resp.Total)
	}
	c := resp.Credentials[0]
	if c.UID != "u1" || c.Nickname != "雪凌" {
		t.Errorf("凭证字段不对: %+v", c)
	}
	if !c.HasRefresh {
		t.Error("带 refreshToken 时应报告 has_refresh=true")
	}
}

// TestApiCredentialsReportsBroken 无法解析的文件应进入 broken 列表而非静默消失。
func TestApiCredentialsReportsBroken(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "workbuddy-bad.json"), []byte("{not json"), 0o600)

	h := &Handler{cfg: Config{AuthDir: dir}}
	rec := httptest.NewRecorder()
	h.apiCredentials(rec, httptest.NewRequest("GET", "/api/credentials", nil))

	var resp struct {
		Broken []map[string]string `json:"broken"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Broken) != 1 {
		t.Fatalf("应报告 1 个坏文件，实际 %d", len(resp.Broken))
	}
}

// TestDeletePathTraversalRejected 删除接口必须挡住路径穿越。
// 这是安全红线：若能用 file_name 传 ../ 就能删掉路由器上任意文件。
func TestDeletePathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	// 在 auths 的上级放一个"重要文件"，确认它不会被动到。
	important := filepath.Join(filepath.Dir(dir), "important.json")
	os.WriteFile(important, []byte("do not delete"), 0o600)
	defer os.Remove(important)

	h := &Handler{cfg: Config{AuthDir: dir, Pool: nil}}
	bad := []string{"../important.json", "/etc/passwd", "workbuddy/../x.json", "other.json"}
	for _, fn := range bad {
		body, _ := json.Marshal(map[string]string{"file_name": fn})
		rec := httptest.NewRecorder()
		h.apiCredentialDelete(rec, httptest.NewRequest("POST", "/api/credentials/delete", strings.NewReader(string(body))))

		if rec.Code != http.StatusBadRequest {
			t.Errorf("file_name=%q 应被拒（400），实际 %d", fn, rec.Code)
		}
	}
	if _, err := os.Stat(important); err != nil {
		t.Fatal("上级目录的文件被删掉了！路径穿越防护失效")
	}
}

// TestAccountStatusCacheTTL 缓存应在 TTL 内命中、过期后失效。
func TestAccountStatusCacheTTL(t *testing.T) {
	c := newStatusCache()
	st := &AccountStatus{UID: "u1", Nickname: "n"}
	c.put(st)

	if got, ok := c.get("u1", st.QueriedAt); !ok || got.Nickname != "n" {
		t.Fatal("TTL 内应命中缓存")
	}
	// 刚过 TTL 边界应失效。
	later := st.QueriedAt.Add(statusCacheTTL + 1)
	if _, ok := c.get("u1", later); ok {
		t.Fatal("超过 TTL 应失效")
	}
	// 未缓存的 key。
	if _, ok := c.get("nope", st.QueriedAt); ok {
		t.Fatal("不存在的 key 不应命中")
	}
}

// TestAccountStatusCacheInvalidate 签到后必须能精确清掉某账号的缓存。
func TestAccountStatusCacheInvalidate(t *testing.T) {
	c := newStatusCache()
	st := &AccountStatus{UID: "u1"}
	c.put(st)
	c.invalidate("u1")
	if _, ok := c.get("u1", st.QueriedAt); ok {
		t.Fatal("invalidate 后不应命中")
	}
}

// TestTrimErrMsg 错误文案必须被压缩（上游错误常带整段 JSON，卡片放不下）。
func TestTrimErrMsg(t *testing.T) {
	short := trimErrMsg(errString("boom"))
	if short != "boom" {
		t.Errorf("短错误应原样，实际 %q", short)
	}
	long := trimErrMsg(errString(strings.Repeat("x", 500)))
	if len(long) > 130 {
		t.Errorf("长错误应被截断，实际长度 %d", len(long))
	}
	multi := trimErrMsg(errString("first line\nsecond line"))
	if multi != "first line" {
		t.Errorf("应只取首行，实际 %q", multi)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// TestBuildAccountViewsStateText 状态文案与可用性判定要覆盖四种状态。
func TestBuildAccountViewsStateText(t *testing.T) {
	// 该用例依赖 pool，若不可用则跳过（避免脆弱）。
	t.Skip("需要真实 pool.Pool，已在集成测试中覆盖")
}
