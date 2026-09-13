package redisstore

import (
	"testing"
	"time"
)

// TestNewReturnsNoop New 在任何入参下都返回 Noop（go-redis 已移除）。
func TestNewReturnsNoop(t *testing.T) {
	// 历史配置形（Upstash URL + token）与全空都要安全降级，不 panic、不发网络请求。
	cases := []struct{ name, url, token string }{
		{"空配置", "", ""},
		{"https 主机形", "https://foo.upstash.io", "tok"},
		{"完整 rediss 串", "rediss://default:tok@host:6379", "ignored"},
		{"裸主机", "foo.upstash.io", "tok"},
		{"非法串", "://bad host", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := New(c.url, c.token).(Noop); !ok {
				t.Fatalf("New(%q,%q) 应返回 Noop", c.url, c.token)
			}
		})
	}
}

// TestNoopMethods Noop 各方法空实现：不 panic，读操作报告无数据。
func TestNoopMethods(t *testing.T) {
	n := Noop{}
	n.SetBind("k", "u", time.Minute)
	n.DelBind("k")
	n.SaveState([]byte("{}"))
	if _, ok := n.LoadState(); ok {
		t.Error("Noop.LoadState 应报告未找到")
	}
	if binds := n.LoadBinds(); binds != nil {
		t.Errorf("Noop.LoadBinds 应返回 nil，实际 %v", binds)
	}
}

// TestStoreInterfaceSatisfied Noop 必须满足 Store 接口（session/pool 依赖此契约）。
func TestStoreInterfaceSatisfied(t *testing.T) {
	var s Store = Noop{}
	// 编译期已由赋值保证；这里再跑一遍方法，防止接口被误改后静默漂移。
	s.SetBind("a", "b", 0)
	s.DelBind("a")
	if _, ok := s.LoadState(); ok {
		t.Error("应无快照")
	}
}
