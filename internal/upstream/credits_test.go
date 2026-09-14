package upstream

import "testing"

// TestParseCredits 覆盖官方模型目录 credits 字段的解析。
//
// 官方形如 "x0.03 credits"；部分模型（auto、图片模型）无此字段。
// 解析失败必须返回 ok=false，让上层显示"浮动"而不是伪造一个 0。
func TestParseCredits(t *testing.T) {
	cases := []struct {
		raw    string
		want   float64
		wantOK bool
	}{
		{"x0.03 credits", 0.03, true},
		{"x0.00 credits", 0.00, true},
		{"x1.62 credits", 1.62, true},
		{"x2.20 credits", 2.20, true},
		{"x0.79 credits", 0.79, true},
		{"X0.5 credits", 0.5, true},   // 大写 X 也接受
		{" x0.17 credits ", 0.17, true}, // 首尾空白
		{"x0.5", 0.5, true},           // 无单位
		{"0.33 credits", 0.33, true},  // 无 x 前缀
		{"", 0, false},                // 缺失
		{"   ", 0, false},             // 全空白
		{"x credits", 0, false},       // 只有前缀无数字
		{"credits", 0, false},         // 无数字
		{"abc", 0, false},             // 纯字母
	}
	for _, c := range cases {
		got, ok := parseCredits(c.raw)
		if ok != c.wantOK {
			t.Errorf("parseCredits(%q) ok=%v want %v", c.raw, ok, c.wantOK)
			continue
		}
		if ok && got != c.want {
			t.Errorf("parseCredits(%q)=%v want %v", c.raw, got, c.want)
		}
	}
}

// TestParseCreditsZeroIsValid x0.00 是**有效**值（免费模型），不是"解析失败"。
// 这个区分很重要：hy3 就是 x0.00，界面应当显示"免费"而不是"未知"。
func TestParseCreditsZeroIsValid(t *testing.T) {
	v, ok := parseCredits("x0.00 credits")
	if !ok {
		t.Fatal("x0.00 应被识别为有效倍率（免费模型）")
	}
	if v != 0 {
		t.Errorf("x0.00 解析为 %v，期望 0", v)
	}
}
