package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnknownAPIPathReturnsJSON404 API 命名空间下的未知路径必须返回 JSON 404，
// **不得**落入 SPA 兜底返回 index.html。
//
// 背景（真实问题）：静态文件 handler 注册在 "GET /" 通配路径上，
// 未注册的 /api/* 与 /v1/* 路径会命中它并被兜底成 index.html（200 + 一大段 HTML）。
// 对 API 客户端而言，这比 404 更难排查——它期待 JSON，拿到 HTML 只会解析失败，
// 且状态码是 200 会让人以为"调用成功了"。
func TestUnknownAPIPathReturnsJSON404(t *testing.T) {
	dir := t.TempDir()
	// 造一个最小 web 目录，让静态 handler 真的注册上（否则测不到兜底逻辑）
	writeTestFile(t, dir, "index.html", "<html>console</html>")

	h := NewHandler(Config{Pool: nil, WebDir: dir})

	for _, p := range []string{"/api/nope", "/api/usage/nope", "/v1/nope", "/v1/chat/nope"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))

		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: 状态码=%d want 404", p, rec.Code)
			continue
		}
		ct := rec.Header().Get("Content-Type")
		if !strings.Contains(ct, "application/json") {
			t.Errorf("%s: Content-Type=%q，应为 JSON（返回 HTML 会让 API 客户端解析失败）", p, ct)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("%s: 响应不是合法 JSON: %s", p, rec.Body.String()[:min(120, rec.Body.Len())])
			continue
		}
		if _, ok := body["error"]; !ok {
			t.Errorf("%s: JSON 缺少 error 字段: %v", p, body)
		}
	}
}

// TestFrontendRouteStillFallsBackToIndex 前端路由（非 API 前缀）仍应回退 index.html，
// 否则刷新页面会出现 404 —— 这是 SPA 的正常需要，不能被上面的修复误伤。
func TestFrontendRouteStillFallsBackToIndex(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "index.html", "<html>console</html>")
	h := NewHandler(Config{Pool: nil, WebDir: dir})

	for _, p := range []string{"/", "/dashboard", "/some/deep/route"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: 状态码=%d want 200（SPA 兜底）", p, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), "console") {
			t.Errorf("%s: 未返回 index.html 内容", p)
		}
	}
}

// TestStaticAssetsStillServed 静态资源不受影响（回归护栏）。
func TestStaticAssetsStillServed(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "index.html", "<html>console</html>")
	writeTestFile(t, dir, "app.js", "console.log(1)")
	writeTestFile(t, dir, "style.css", "body{}")
	h := NewHandler(Config{Pool: nil, WebDir: dir})

	cases := map[string]string{
		"/app.js":    "application/javascript",
		"/style.css": "text/css",
	}
	for p, wantCT := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: 状态码=%d want 200", p, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, strings.Split(wantCT, "/")[1]) {
			t.Errorf("%s: Content-Type=%q，期望含 %q", p, ct, wantCT)
		}
	}
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写入测试文件 %s 失败: %v", p, err)
	}
}
