package config

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"reflect"
	"time"
)

// 默认轮询间隔，以及检测到变化后等待文件写入稳定再读取的时间。
const (
	defaultPollInterval = 1 * time.Second
	writeDebounce       = 150 * time.Millisecond
)

// Watcher 监听配置文件变化并热更新 Store。
//
// 采用轻量轮询（默认每秒一次）而非文件系统事件：
//   - 无需额外依赖，跨平台行为一致；
//   - 兼容编辑器"先写临时文件再原子重命名"的保存方式；
//   - 通过内容哈希判断是否真正变化，仅注释/空白修改不会触发重载。
//
// 重载流程：内容变化 -> 解析并校验 -> 调用 OnReload 应用需额外动作的变更
// （执行器重建、监听地址切换等，全部就绪才提交）-> 更新 Store。
// 解析校验失败、OnReload 返回 error 时均保持当前生效配置，仅记录日志。
//
// SetOnReload 必须在 Start 之前调用。
type Watcher struct {
	path     string
	store    *Store
	logger   *slog.Logger
	interval time.Duration
	debounce time.Duration
	onReload func(oldCfg, newCfg *Config) error

	lastHash    string // 上次已处理内容的哈希；空串表示尚未做过首次同步
	fileMissing bool   // 文件缺失状态，用于只在状态切换时告警一次
	stopCh      chan struct{}
	doneCh      chan struct{}
}

// NewWatcher 创建配置文件监听器。
func NewWatcher(path string, store *Store, logger *slog.Logger) *Watcher {
	return &Watcher{
		path:     path,
		store:    store,
		logger:   logger,
		interval: defaultPollInterval,
		debounce: writeDebounce,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
}

// SetOnReload 注册配置变更回调：在配置解析校验通过、即将生效前调用。
// 回调用于应用执行器、监听地址等需要额外动作的变更；返回 error 时整体
// 回滚（不更新 Store，保持原配置）。
func (w *Watcher) SetOnReload(fn func(oldCfg, newCfg *Config) error) {
	w.onReload = fn
}

// Start 启动后台轮询（非阻塞）。
func (w *Watcher) Start() { go w.loop() }

// Stop 停止轮询并等待退出。
func (w *Watcher) Stop() {
	close(w.stopCh)
	<-w.doneCh
}

func (w *Watcher) loop() {
	defer close(w.doneCh)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
			w.checkOnce()
		}
	}
}

// checkOnce 执行一次检查。首次 tick 即使内容未变也会完成一次"同步"
// （解析并与 Store 对比），这样若文件在启动加载之后、首次检查之前被修改，
// 也能在第一个周期内被捕获应用。
func (w *Watcher) checkOnce() {
	data, err := os.ReadFile(w.path)
	if err != nil {
		missing := os.IsNotExist(err)
		if missing && !w.fileMissing {
			w.fileMissing = true
			w.logger.Warn("配置文件不存在，保持当前配置", "path", w.path)
		} else if !missing {
			w.logger.Warn("读取配置文件失败（将重试）", "path", w.path, "error", err)
		}
		return
	}
	w.fileMissing = false

	hash := hashBytes(data)
	if hash == w.lastHash {
		return
	}

	// 等待写入稳定后再读取一次，避免读到写了一半的文件。
	time.Sleep(w.debounce)
	if data2, err2 := os.ReadFile(w.path); err2 == nil {
		data, hash = data2, hashBytes(data2)
	}
	w.lastHash = hash

	newCfg, err := Parse(data)
	if err != nil {
		w.logger.Warn("配置变更无效，已保持当前配置（旧配置备份于 "+BackupPath(w.path)+"）",
			"path", w.path, "error", err)
		return
	}

	oldCfg := w.store.Load()
	if reflect.DeepEqual(oldCfg, newCfg) {
		return // 仅注释/空白等不影响配置的变化
	}

	if w.onReload != nil {
		if err := w.onReload(oldCfg, newCfg); err != nil {
			w.logger.Warn("配置变更未生效，已保持当前配置（旧配置备份于 "+BackupPath(w.path)+"）",
				"path", w.path, "error", err)
			return
		}
	}

	w.store.Update(newCfg)
	// 备份本次生效的配置内容：作为后续热更新失败时的恢复依据。
	// 备份失败不影响本次生效，仅告警。
	if err := WriteBackup(w.path, data); err != nil {
		w.logger.Warn("备份配置失败", "backup", BackupPath(w.path), "error", err)
	}
	w.logger.Info("配置已热更新", "changed", changedFields(oldCfg, newCfg), "path", w.path,
		"backup", BackupPath(w.path))
}

// changedFields 列出新旧配置间的变更字段（用于日志）。
func changedFields(oldCfg, newCfg *Config) []string {
	var changed []string
	if oldCfg.Server.Addr != newCfg.Server.Addr {
		changed = append(changed, "server.addr")
	}
	if oldCfg.Server.AuthToken != newCfg.Server.AuthToken {
		changed = append(changed, "server.auth_token")
	}
	if oldCfg.Server.StdoutFormat != newCfg.Server.StdoutFormat {
		changed = append(changed, "server.stdout_format")
	}
	if oldCfg.Server.MaxConcurrent != newCfg.Server.MaxConcurrent {
		changed = append(changed, "server.max_concurrent")
	}
	if oldCfg.ScriptsDir != newCfg.ScriptsDir {
		changed = append(changed, "scripts_dir")
	}
	if oldCfg.WorkDir != newCfg.WorkDir {
		changed = append(changed, "work_dir")
	}
	if oldCfg.AllowOutsideScripts != newCfg.AllowOutsideScripts {
		changed = append(changed, "allow_outside_scripts")
	}
	if oldCfg.History.StdoutFormat != newCfg.History.StdoutFormat {
		changed = append(changed, "history.stdout_format")
	}
	if !reflect.DeepEqual(oldCfg.Mappings, newCfg.Mappings) {
		changed = append(changed, "mappings")
	}
	return changed
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
