// oauth.go OAuth 插件协议端点（申请登录链接 / 换取 token）。
//
// 与其它上游调用的关键差异：这两个端点是**未登录状态**下访问的，
// 因此必须带 X-No-Authorization 一族头（表明"我还没有身份"），
// 而不是带某个账号的 Bearer token。官方 CLI 的登录流程就是这么走的。
package upstream

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"workbuddy2api/internal/auth"
)

// Identity 从 access token 中提取的身份信息。
type Identity struct {
	// UID 稳定账号标识。取 JWT 的 sub —— 实测该值与凭证文件里的 account.uid
	// 完全一致（同一账号换 token 后 sub 不变），因此可用于判断"这是同一个账号"，
	// 避免重复登录同一账号时在池里产生两条记录。
	UID string
	// Nickname 可读昵称（JWT nickname 优先，回落 preferred_username）。
	Nickname string
	// EnterpriseID 所属企业（JWT 与企业声明均可能为空 = 个人账号）。
	EnterpriseID string
}

// jwtPayload 解析 JWT 的 payload 段（不做签名校验——只用于读取账号标识，
// token 本身的合法性由后续上游调用验证）。
//
// 返回 nil 表示不是可解析的 JWT（如 opaque token），调用方需回落其它来源。
func jwtPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	seg := parts[1]
	// JWT 用 base64url 且通常无 padding，补齐后再解码。
	if pad := len(seg) % 4; pad != 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		// 少数实现用标准 base64 编码，再试一次。
		raw, err = base64.StdEncoding.DecodeString(seg)
		if err != nil {
			return nil
		}
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// strField 从 map 取字符串字段（空/不存在返回 ""）。
func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// ParseIdentity 从 access token 提取身份，并用 API 响应字段补齐缺失项。
//
// 优先级说明：UID 必须优先取 JWT 的 sub。API 响应里的 uid 字段在实测中并不返回，
// 若只依赖它就会退化成随机标识，导致同一账号被重复加入池中。
func ParseIdentity(accessToken string, apiResp map[string]any) Identity {
	id := Identity{}
	if p := jwtPayload(accessToken); p != nil {
		// sub 是稳定标识；个别环境可能缺 sub，再试 uid/userId/sub_type 语义的字段。
		id.UID = strField(p, "sub", "uid", "userId", "user_id")
		id.Nickname = strField(p, "nickname", "preferred_username", "name")
		id.EnterpriseID = strField(p, "enterprise_id", "enterpriseId", "tenant_id", "tenantId")
	}
	// API 响应补齐（JWT 缺字段或本身不是 JWT 时）。
	if id.UID == "" && apiResp != nil {
		id.UID = strField(apiResp, "uid", "sub", "userId", "user_id")
	}
	if id.Nickname == "" && apiResp != nil {
		id.Nickname = strField(apiResp, "nickname", "name", "preferred_username")
	}
	if id.EnterpriseID == "" && apiResp != nil {
		id.EnterpriseID = strField(apiResp, "enterpriseId", "enterprise_id", "tenantId", "tenant_id")
	}
	return id
}

// oauthTraceHeaders 构造登录流程所需的"无身份"请求头。
// 取自实测可用的官方 CLI 形态（platform=CLI 流程）。
func (c *Client) oauthHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	// 关键：显式声明"本次请求不带身份信息"。
	req.Header.Set("X-No-Authorization", "true")
	req.Header.Set("X-No-Department-Info", "true")
	req.Header.Set("X-No-Enterprise-Id", "true")
	req.Header.Set("X-No-User-Id", "true")
	req.Header.Set("User-Agent", "CLI/1.0.8 CodeBuddy/1.0.8")
}

// OAuthJSON 调 billing 域的 OAuth 端点并返回原始响应体。
//
// 与 billingJSON 的差异：这里不解业务信封（调用方需要看 code/msg 自己分派，
// 尤其要区分 code=11217「等待用户登录」这个非错误状态），因此直接返回原始 JSON。
// path 需自带查询串（如 "/v2/plugin/auth/state?platform=CLI&nonce=x"）。
func (c *Client) OAuthJSON(ctx context.Context, path, method string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}

	// 用零值 Auth 取 billing 域基址（billingBase 当前不读取账号字段，
	// 这里显式传零值以避免调用方误以为需要某个已登录账号）。
	var noAuth auth.Auth
	base := c.billingBase(&noAuth)
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return nil, err
	}
	c.oauthHeaders(req)
	// X-Domain 取 billing 域主机名（www.codebuddy.cn），与官方 CLI 一致。
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		req.Header.Set("X-Domain", u.Host)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	// 注意：此处不把非 2xx 当错误，交给调用方解析 code/msg 决定文案。
	return json.RawMessage(raw), nil
}
