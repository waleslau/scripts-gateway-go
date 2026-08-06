package config

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustParse(t *testing.T, content string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(content))
	if err != nil {
		t.Fatalf("Parse 失败: %v", err)
	}
	return cfg
}

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入配置文件失败: %v", err)
	}
}

// newTestWatcher 创建轮询间隔极短的 Watcher，便于测试快速收敛。
func newTestWatcher(t *testing.T, path string, store *Store) *Watcher {
	t.Helper()
	w := NewWatcher(path, store, testLogger())
	w.interval = 20 * time.Millisecond
	w.debounce = 5 * time.Millisecond
	return w
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

func TestStore(t *testing.T) {
	store := NewStore(mustParse(t, "server:\n  addr: \":9999\"\n"))
	if got := store.Load().Server.Addr; got != ":9999" {
		t.Fatalf("Load() addr = %q, want :9999", got)
	}

	store.Update(mustParse(t, "server:\n  addr: \":8888\"\n"))
	if got := store.Load().Server.Addr; got != ":8888" {
		t.Fatalf("Update 后 Load() addr = %q, want :8888", got)
	}
}

func TestWatcherReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, "server:\n  addr: \":8080\"\n")
	store := NewStore(mustParse(t, "server:\n  addr: \":8080\"\n"))

	w := newTestWatcher(t, path, store)
	reloaded := make(chan struct{}, 4)
	w.SetOnReload(func(_, _ *Config) error {
		reloaded <- struct{}{}
		return nil
	})
	w.Start()
	defer w.Stop()

	writeConfig(t, path, "server:\n  addr: \":9090\"\n")
	select {
	case <-reloaded:
	case <-time.After(2 * time.Second):
		t.Fatal("未检测到配置变更")
	}
	waitFor(t, 2*time.Second, func() bool { return store.Load().Server.Addr == ":9090" },
		"Store 未更新为新配置")
}

func TestWatcherInvalidConfigKeepsOld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, "server:\n  addr: \":8080\"\n")
	store := NewStore(mustParse(t, "server:\n  addr: \":8080\"\n"))

	w := newTestWatcher(t, path, store)
	w.Start()
	defer w.Stop()
	time.Sleep(50 * time.Millisecond) // 等待首次同步

	// method 非法 → 校验失败，保持原配置。
	writeConfig(t, path, "mappings:\n  - name: x\n    method: TRACE\n    script: x.sh\n")
	time.Sleep(200 * time.Millisecond)
	if got := store.Load().Server.Addr; got != ":8080" {
		t.Fatalf("非法配置不应生效，addr = %q", got)
	}
	if len(store.Load().Mappings) != 0 {
		t.Fatal("非法配置不应更新 mappings")
	}
}

func TestWatcherOnReloadErrorKeepsOld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, "server:\n  addr: \":8080\"\n")
	store := NewStore(mustParse(t, "server:\n  addr: \":8080\"\n"))

	w := newTestWatcher(t, path, store)
	w.SetOnReload(func(_, _ *Config) error { return errors.New("apply 失败") })
	w.Start()
	defer w.Stop()
	time.Sleep(50 * time.Millisecond)

	writeConfig(t, path, "server:\n  addr: \":9090\"\n")
	// 等待 watcher 处理本次变更（onReload 返回错误后 Store 不应更新）。
	time.Sleep(300 * time.Millisecond)
	if got := store.Load().Server.Addr; got != ":8080" {
		t.Fatalf("onReload 失败时不应更新 Store，addr = %q", got)
	}
}

func TestWatcherIgnoresCommentOnlyChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, "server:\n  addr: \":8080\"\n")
	store := NewStore(mustParse(t, "server:\n  addr: \":8080\"\n"))

	var reloads atomic.Int32
	w := newTestWatcher(t, path, store)
	w.SetOnReload(func(_, _ *Config) error {
		reloads.Add(1)
		return nil
	})
	w.Start()
	defer w.Stop()
	time.Sleep(50 * time.Millisecond)

	// 仅加注释：内容哈希变化但解析结果相同，不应触发重载。
	writeConfig(t, path, "# 新增注释\nserver:\n  addr: \":8080\"\n")
	time.Sleep(300 * time.Millisecond)
	if n := reloads.Load(); n != 0 {
		t.Fatalf("注释变化不应触发重载，共触发 %d 次", n)
	}
}

func TestWriteBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := WriteBackup(path, []byte("hello\n")); err != nil {
		t.Fatalf("WriteBackup 失败: %v", err)
	}
	if BackupPath(path) != path+".bak" {
		t.Fatalf("BackupPath = %q, want %q", BackupPath(path), path+".bak")
	}
	data, err := os.ReadFile(BackupPath(path))
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("备份内容 = %q, err = %v", data, err)
	}

	// 重复备份应覆盖旧内容（原子替换）。
	if err := WriteBackup(path, []byte("world\n")); err != nil {
		t.Fatalf("重复备份失败: %v", err)
	}
	data, _ = os.ReadFile(BackupPath(path))
	if string(data) != "world\n" {
		t.Fatalf("覆盖后备份内容 = %q, want world", data)
	}
}

func TestWatcherWritesBackupOnReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfig(t, path, "server:\n  addr: \":8080\"\n")
	store := NewStore(mustParse(t, "server:\n  addr: \":8080\"\n"))

	w := newTestWatcher(t, path, store)
	w.Start()
	defer w.Stop()
	time.Sleep(50 * time.Millisecond) // 等待首次同步

	// 触发一次成功的重载，备份文件应更新为新生效的配置内容。
	writeConfig(t, path, "server:\n  addr: \":9090\"\n")
	waitFor(t, 2*time.Second, func() bool { return store.Load().Server.Addr == ":9090" },
		"配置未热更新")

	waitFor(t, 2*time.Second, func() bool {
		data, err := os.ReadFile(BackupPath(path))
		return err == nil && strings.Contains(string(data), ":9090")
	}, "备份文件未更新为新配置")

	// 写入坏配置后，备份仍是上一个生效配置（回滚依据）。
	writeConfig(t, path, "mappings:\n  - name: x\n    method: TRACE\n    script: x.sh\n")
	time.Sleep(300 * time.Millisecond)
	data, err := os.ReadFile(BackupPath(path))
	if err != nil {
		t.Fatalf("读取备份失败: %v", err)
	}
	if !strings.Contains(string(data), ":9090") {
		t.Fatalf("坏配置下备份应保持上一个生效配置，备份内容 = %q", data)
	}
	if got := store.Load().Server.Addr; got != ":9090" {
		t.Fatalf("坏配置下 Store 应保持 :9090, got %q", got)
	}
}

func TestChangedFields(t *testing.T) {
	oldCfg := mustParse(t, "server:\n  addr: \":8080\"\n  auth_token: \"a\"\nscripts_dir: \"./s\"\nmappings:\n  - name: a\n    script: a.sh\n")
	newCfg := mustParse(t, "server:\n  addr: \":9090\"\n  auth_token: \"b\"\n  stdout_format: \"lines\"\n  max_concurrent: 5\nscripts_dir: \"./s2\"\nwork_dir: \"/tmp\"\nallow_outside_scripts: true\nmappings:\n  - name: a\n    script: a.sh\n    timeout_seconds: 5\n")
	got := changedFields(oldCfg, newCfg)
	want := []string{"server.addr", "server.auth_token", "server.stdout_format",
		"server.max_concurrent", "scripts_dir", "work_dir", "allow_outside_scripts",
		"history.stdout_format", "mappings"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changedFields = %v, want %v", got, want)
	}

	if got := changedFields(oldCfg, oldCfg); len(got) != 0 {
		t.Fatalf("相同配置 changedFields = %v, want 空", got)
	}

	// history.stdout_format 独立报告变更。
	histOld := mustParse(t, "history:\n  stdout_format: \"text\"\n")
	histNew := mustParse(t, "history:\n  stdout_format: \"lines\"\n")
	if got := changedFields(histOld, histNew); !reflect.DeepEqual(got, []string{"history.stdout_format"}) {
		t.Fatalf("changedFields = %v, want [history.stdout_format]", got)
	}
}
