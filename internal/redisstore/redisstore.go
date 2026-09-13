// Package redisstore 提供池状态快照与粘性会话镜像的存储接口。
//
// 本包原为 Upstash（云 Redis）封装，现已裁掉该实现：
// 网关为单实例部署，粘性会话与池状态在本地内存 + state.json 中已完整表达，
// 远程镜像从未被使用（upstash 未配置时一直走空实现），却引入了 go-redis
// 依赖（实测约占二进制 983KB / 13.2%）。故保留接口、移除实现。
//
// 保留接口而非直接内联的原因：session.Router 与 pool.StoreSnapshotter 都按
// 接口注入，接口在位可让这些包零改动，也为将来若真需要外部存储留出扩展点。
package redisstore

import "time"

// Store 池状态快照 + 粘性会话镜像的最小接口。
//
// 当前唯一实现是 Noop（纯内存，各方法空操作）。调用方据此降级为
// "仅本地 state.json 持久化"，功能完整。
type Store interface {
	// SetBind 镜像粘性会话绑定（key→uid）。
	SetBind(key, uid string, ttl time.Duration)
	// DelBind 删除粘性会话绑定。
	DelBind(key string)
	// LoadBinds 全量读取粘性会话绑定（key→uid）。仅在启动时调用。
	LoadBinds() map[string]string
	// SaveState 写池状态 JSON 快照。
	SaveState(data []byte)
	// LoadState 读池状态快照；仅在启动时调用。
	LoadState() ([]byte, bool)
}

// Noop 纯内存实现：所有方法空操作，读操作报告"无数据"。
type Noop struct{}

func (Noop) SetBind(string, string, time.Duration) {}
func (Noop) DelBind(string)                        {}
func (Noop) LoadBinds() map[string]string          { return nil }
func (Noop) SaveState([]byte)                      {}
func (Noop) LoadState() ([]byte, bool)             { return nil, false }

// New 返回存储实现。现存唯一实现是 Noop。
//
// 保留 url/token 形参是为了让调用方（cmd/server）与既有测试的接线不必改动；
// 两个参数当前均被忽略。
func New(url, token string) Store { return Noop{} }
