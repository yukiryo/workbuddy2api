package upstream

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// makeJWT 用给定 payload 造一个未签名的 JWT（header.payload.fakesig）。
// 只为测试解析逻辑，签名无关（ParseIdentity 明确不做签名校验）。
func makeJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(body) + ".fakesignature"
}

// TestParseIdentityUsesJWTSub 是关键回归测试。
//
// 实测事实：JWT 的 sub 等于凭证文件里的 account.uid
// （真账号 4c86bd9b-b904-47e6-a5ec-414d1aeb7a3d 已核验）。
// 而 OAuth 的 token 响应**并不返回 uid 字段**。
//
// 若此测试失败（UID 退化成哈希或为空），同一账号重复登录会在池里产生
// 两条记录 —— 调度器当两个号轮换，白白消耗额度，且用户无从察觉。
func TestParseIdentityUsesJWTSub(t *testing.T) {
	tok := makeJWT(t, map[string]any{
		"sub":                "4c86bd9b-b904-47e6-a5ec-414d1aeb7a3d",
		"nickname":           "雪凌Yukiryo",
		"preferred_username": "15639838599",
		"exp":                1789567200,
	})

	// 模拟真实的 token 响应：不含 uid。
	apiResp := map[string]any{}

	got := ParseIdentity(tok, apiResp)
	if got.UID != "4c86bd9b-b904-47e6-a5ec-414d1aeb7a3d" {
		t.Fatalf("UID=%q，期望取 JWT 的 sub", got.UID)
	}
	if got.Nickname != "雪凌Yukiryo" {
		t.Errorf("Nickname=%q，期望 nickname", got.Nickname)
	}
}

// TestParseIdentitySameAccountSameUID 同账号换 token 必须得到同一 UID。
func TestParseIdentitySameAccountSameUID(t *testing.T) {
	sub := "4c86bd9b-b904-47e6-a5ec-414d1aeb7a3d"
	tok1 := makeJWT(t, map[string]any{"sub": sub, "iat": 1789308000, "jti": "aaa"})
	tok2 := makeJWT(t, map[string]any{"sub": sub, "iat": 1789400000, "jti": "bbb"})

	a := ParseIdentity(tok1, nil)
	b := ParseIdentity(tok2, nil)
	if a.UID != b.UID {
		t.Fatalf("同账号两次登录得到不同 UID: %q vs %q（会产生重复账号）", a.UID, b.UID)
	}
	if a.UID != sub {
		t.Errorf("UID=%q 期望 %q", a.UID, sub)
	}
}

// TestParseIdentityFallsBackToAPI 非 JWT（opaque token）时回落 API 字段。
func TestParseIdentityFallsBackToAPI(t *testing.T) {
	apiResp := map[string]any{"uid": "api-uid-123", "nickname": "API 昵称"}
	got := ParseIdentity("opaque-token-not-a-jwt", apiResp)
	if got.UID != "api-uid-123" {
		t.Errorf("UID=%q，期望回落 API 的 uid", got.UID)
	}
	if got.Nickname != "API 昵称" {
		t.Errorf("Nickname=%q", got.Nickname)
	}
}

// TestParseIdentityJWTPriorityOverAPI JWT 与 API 都有 uid 时，JWT 优先
// （JWT 的 sub 才是与本地凭证文件一致的口径）。
func TestParseIdentityJWTPriorityOverAPI(t *testing.T) {
	tok := makeJWT(t, map[string]any{"sub": "from-jwt"})
	got := ParseIdentity(tok, map[string]any{"uid": "from-api"})
	if got.UID != "from-jwt" {
		t.Errorf("UID=%q，应优先 JWT 的 sub", got.UID)
	}
}

// TestParseIdentityHandlesEmptyAndMalformed 边界输入不得 panic。
func TestParseIdentityHandlesEmptyAndMalformed(t *testing.T) {
	cases := []struct{ name, tok string }{
		{"空串", ""},
		{"单段", "notajwt"},
		{"两段但非法 base64", "a.!!!notbase64!!!"},
		{"payload 不是 JSON", "a." + base64.RawURLEncoding.EncodeToString([]byte("not json")) + ".c"},
		{"payload 是 JSON 数组", "a." + base64.RawURLEncoding.EncodeToString([]byte("[1,2]")) + ".c"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseIdentity(c.tok, nil)
			// 只要求不 panic 且不给出错误 UID。
			if got.UID != "" {
				t.Errorf("非法 token 不应得出 UID，实际 %q", got.UID)
			}
		})
	}
}

// TestParseIdentityEnterpriseAndPreferredUsername 企业账号与无 nickname 时的回落。
func TestParseIdentityEnterpriseAndPreferredUsername(t *testing.T) {
	tok := makeJWT(t, map[string]any{
		"sub":                "ent-user",
		"preferred_username": "13800138000",
		"enterpriseId":       "ent-999",
	})
	got := ParseIdentity(tok, nil)
	if got.Nickname != "13800138000" {
		t.Errorf("nickname 缺失时应回落 preferred_username，实际 %q", got.Nickname)
	}
	if got.EnterpriseID != "ent-999" {
		t.Errorf("EnterpriseID=%q 期望 ent-999", got.EnterpriseID)
	}
}

// TestJwtPayloadPadding 带 padding 与不带 padding 的 base64url 都要能解。
func TestJwtPayloadPadding(t *testing.T) {
	payload := map[string]any{"sub": "padding-test-uid"}
	body, _ := json.Marshal(payload)

	// 标准 RawURLEncoding（无 padding）
	raw := base64.RawURLEncoding.EncodeToString(body)
	tok := "h." + raw + ".s"
	if got := ParseIdentity(tok, nil); got.UID != "padding-test-uid" {
		t.Errorf("无 padding 时解析失败: %q", got.UID)
	}

	// 带 padding
	padded := base64.URLEncoding.EncodeToString(body)
	tok2 := "h." + padded + ".s"
	if got := ParseIdentity(tok2, nil); got.UID != "padding-test-uid" {
		t.Errorf("带 padding 时解析失败: %q", got.UID)
	}

	// 用标准（非 URL-safe）base64 编码
	std := base64.StdEncoding.EncodeToString(body)
	tok3 := "h." + strings.ReplaceAll(std, "+", "-") + ".s"
	if got := ParseIdentity(tok3, nil); got.UID != "padding-test-uid" {
		t.Errorf("标准 base64 时解析失败: %q", got.UID)
	}
}
