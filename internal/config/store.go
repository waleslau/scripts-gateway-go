package config

import "sync/atomic"

// Store 提供并发安全的 *Config 读写，支撑配置热更新。
//
// 配置变更（文件被修改、解析校验通过）时通过 Update 原子替换，
// Server 等组件在每次请求中 Load 读取当前生效配置，无需加锁。
// 读取方不得修改返回的 *Config。
type Store struct {
	cfg atomic.Pointer[Config]
}

// NewStore 以初始配置创建 Store。
func NewStore(cfg *Config) *Store {
	s := &Store{}
	s.cfg.Store(cfg)
	return s
}

// Load 返回当前生效的配置。
func (s *Store) Load() *Config { return s.cfg.Load() }

// Update 原子地替换当前配置。
func (s *Store) Update(cfg *Config) { s.cfg.Store(cfg) }
