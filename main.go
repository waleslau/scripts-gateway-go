// scripts-gateway-go（脚本执行网关）是一个对外暴露 RESTful 接口的服务，
// 接口与工作目录下的 shell 脚本通过 config.yaml 建立映射关系，
// 按参数调用后执行对应脚本并返回结果。
//
// 配置热更新：服务运行期间监测配置文件，修改后无需重启进程即可生效
// （见 usage 中的"配置热更新"说明）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"scripts-gateway-go/internal/config"
	"scripts-gateway-go/internal/executor"
	"scripts-gateway-go/internal/history"
	"scripts-gateway-go/internal/server"
)

// usage 输出 --help / -h 帮助信息：启动参数、配置文件选项、REST 接口与调用示例。
func usage() {
	out := flag.CommandLine.Output()
	fmt.Fprintf(out, "scripts-gateway-go（脚本执行网关）— 将 HTTP 接口与 Shell 脚本建立映射的 RESTful 服务\n\n")
	fmt.Fprintf(out, "用法:\n  %s [flags]\n\n", os.Args[0])

	fmt.Fprintln(out, "启动参数:")
	flag.PrintDefaults()
	fmt.Fprintln(out, "  -h, -help                    显示帮助信息")
	fmt.Fprint(out, `
配置文件 (config.yaml):
  server.addr                  监听地址，默认 ":8080"
  server.auth_token            访问令牌，为空则关闭鉴权；除 /healthz 外所有接口需携带 Authorization: Bearer <token> 或 X-Auth-Token
  server.stdout_format         任务结果、执行历史详情与历史 JSONL 落盘 stdout 格式：text=原样字符串（默认）；lines=按行拆分为数组
  server.max_concurrent        并发执行上限（默认 0 = 不限；达到上限时新请求返回 503）
  history                      执行历史与日志持久化（JSONL 追加写，默认关闭）：
    enabled                   是否启用（默认 false）
    dir                       历史存储目录（自动创建，默认 ./history，权限 0750）
    max_file_size_mb          单个文件滚动阈值（默认 50MB）
    max_files                 保留文件数（默认 25，超出删除最旧的）
    retention_days            记录保留天数（默认 60；0=不按时间清理）
    max_output_bytes          每条记录持久化的 stdout/stderr 上限（默认 64KB）
    stdout_format             历史落盘与查询接口 stdout 格式：text=原样字符串（默认）；lines=按行拆分为数组；未配置时跟随 server.stdout_format
  记录策略：成功执行始终记录；单个任务连续失败 3 次以内记录，第 4 次起跳过，
  直到该任务下一次成功执行后重置（计数仅存内存，重启清零）
  scripts_dir                  脚本根目录，默认 "./scripts"
  work_dir                     脚本进程工作目录，默认 "./"
  allow_outside_scripts        允许执行脚本目录之外的脚本（默认 false）；开启后 script 可为绝对路径或含 ../ 的相对路径
  mappings                     接口 -> 脚本 映射列表，每项包含：
    name                       任务名（URL 路径中的 {name}）
    method                     支持的 HTTP 方法（GET/POST/PUT/DELETE，默认 POST）
    script                     脚本文件（默认位于 scripts_dir 下；开启 allow_outside_scripts 后可为绝对路径或含 ../ 的相对路径）
    timeout_seconds            执行超时（秒），默认 30
    params                     参数列表：name / required / default / description

配置热更新:
  服务运行期间每秒监测配置文件，修改后保存即自动生效，无需重启进程：
  - 立即生效：mappings（增删改任务）、server.auth_token、server.stdout_format
  - 立即生效：server.max_concurrent（并发上限调整无需重启）
  - 自动重建：scripts_dir / work_dir / allow_outside_scripts 变更时重建执行器并原子切换
  - 自动切换监听：server.addr 变更时先绑定新地址再切换，旧连接优雅关闭
  配置文件解析/校验失败、执行器重建失败、新地址无法绑定时，保持原配置生效并记录错误日志。
  每次成功应用的配置会原子备份到 <config>.bak（如 config.yaml.bak）；热更新失败时
  旧配置仍生效，可用 cp config.yaml.bak config.yaml 回滚。

REST 接口:
  GET  /healthz                         健康检查
  GET  /api/v1/tasks                    列出所有任务及参数说明
  POST|GET|PUT|DELETE /api/v1/tasks/{name}  执行指定任务（实际方法由映射配置决定）
  GET  /api/v1/history                  查询执行历史（history.enabled=true 时可用）
                                        ?task=<任务名>&exit_code=<退出码>&from=<RFC3339>&to=<RFC3339>&limit=<默认20>&offset=<默认0>
  GET  /api/v1/history/{id}             查询单条执行记录详情（含完整 stdout/stderr）
  DELETE /api/v1/history                清空全部执行历史

参数传递（请求体优先于 Query String）:
  1. JSON Body:    curl -X POST http://localhost:8080/api/v1/tasks/deploy \
                     -H "Content-Type: application/json" \
                     -d '{"env": "prod", "version": "1.2.3"}'
                   （Content-Type 头非必填：请求体以 { 开头时自动按 JSON 解析）
  2. 表单 Body:    curl -X POST http://localhost:8080/api/v1/tasks/deploy \
                     -d 'env=prod&version=1.2.3'
  3. Query String: curl -X POST "http://localhost:8080/api/v1/tasks/deploy?env=prod&version=1.2.3"

示例:
  ./scripts-gateway-go                               # 默认加载 ./config.yaml
`)
}

func main() {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Usage = usage
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("加载配置失败", "error", err)
		os.Exit(1)
	}
	store := config.NewStore(cfg)

	// 启动时备份初始配置：后续热更新失败时，可从这里恢复（config.yaml.bak）。
	if data, e := os.ReadFile(*configPath); e == nil {
		if e := config.WriteBackup(*configPath, data); e != nil {
			logger.Warn("备份初始配置失败", "backup", config.BackupPath(*configPath), "error", e)
		}
	}

	ex, err := executor.New(cfg.ScriptsDir, cfg.WorkDir, cfg.AllowOutsideScripts)
	if err != nil {
		logger.Error("初始化执行器失败", "error", err)
		os.Exit(1)
	}
	if cfg.AllowOutsideScripts {
		logger.Warn("已开启 allow_outside_scripts（配置文件），映射中的脚本可指向脚本目录之外，请确保配置可信")
	}

	// 执行历史与日志持久化（history.enabled=true 时启用，JSONL 追加写）。
	var historyStore *history.Store
	if cfg.History.Enabled {
		historyStore, err = newHistoryStore(cfg.History)
		if err != nil {
			logger.Error("初始化执行历史存储失败", "error", err)
			os.Exit(1)
		}
		logger.Info("执行历史已启用",
			"dir", historyStore.Dir(),
			"max_file_size_mb", cfg.History.MaxFileSizeMB,
			"max_files", cfg.History.MaxFiles,
			"retention_days", cfg.History.RetentionDaysValue(),
			"max_output_bytes", cfg.History.MaxOutputBytes)
	}

	handler := server.New(store, ex, logger, historyStore)
	mgr := newServerManager(handler, logger)

	ln, err := net.Listen("tcp", cfg.Server.Addr)
	if err != nil {
		logger.Error("监听失败", "addr", cfg.Server.Addr, "error", err)
		os.Exit(1)
	}
	mgr.serve(ln)

	logger.Info("服务启动",
		"addr", cfg.Server.Addr,
		"scripts_dir", cfg.ScriptsDir,
		"work_dir", cfg.WorkDir,
		"tasks", len(cfg.Mappings),
		"config", *configPath,
		"config_watch", "enabled")

	// 配置热更新：监听文件变更，修改后自动生效（详见 usage"配置热更新"）。
	// 变更先全部准备就绪（重建执行器、绑定新地址）再统一生效；任一失败则整体回滚，
	// 保证 Store 中配置与实际运行状态（执行器、监听地址）始终一致。
	watcher := config.NewWatcher(*configPath, store, logger)
	watcher.SetOnReload(func(oldCfg, newCfg *config.Config) error {
		var newEx *executor.Executor
		if newCfg.ScriptsDir != oldCfg.ScriptsDir ||
			newCfg.WorkDir != oldCfg.WorkDir ||
			newCfg.AllowOutsideScripts != oldCfg.AllowOutsideScripts {
			built, e := executor.New(newCfg.ScriptsDir, newCfg.WorkDir, newCfg.AllowOutsideScripts)
			if e != nil {
				return fmt.Errorf("重建执行器失败（scripts_dir/work_dir/allow_outside_scripts 变更未生效）: %w", e)
			}
			newEx = built
		}

		var newHistory *history.Store
		if !newCfg.History.Equal(oldCfg.History) {
			if newCfg.History.Enabled {
				built, e := newHistoryStore(newCfg.History)
				if e != nil {
					return fmt.Errorf("重建执行历史存储失败（history 配置变更未生效）: %w", e)
				}
				newHistory = built
			}
		}

		var newLn net.Listener
		if newCfg.Server.Addr != oldCfg.Server.Addr {
			l, e := net.Listen("tcp", newCfg.Server.Addr)
			if e != nil {
				return fmt.Errorf("监听新地址失败（server.addr 变更未生效，继续使用原地址）: %w", e)
			}
			newLn = l
		}

		// 所有变更就绪后统一生效。
		if newEx != nil {
			handler.SetExecutor(newEx)
			logger.Info("执行器配置已更新",
				"scripts_dir", newCfg.ScriptsDir,
				"work_dir", newCfg.WorkDir,
				"allow_outside_scripts", newCfg.AllowOutsideScripts)
		}
		if newHistory != nil || !newCfg.History.Equal(oldCfg.History) {
			handler.SetHistoryStore(newHistory)
			if historyStore != nil {
				historyStore.Close()
			}
			historyStore = newHistory
			logger.Info("执行历史配置已更新",
				"enabled", newCfg.History.Enabled,
				"dir", newCfg.History.Dir,
				"retention_days", newCfg.History.RetentionDaysValue())
		}
		if newCfg.Server.MaxConcurrent != oldCfg.Server.MaxConcurrent {
			handler.SetMaxConcurrent(newCfg.Server.MaxConcurrent)
			logger.Info("并发执行上限已更新", "max_concurrent", newCfg.Server.MaxConcurrent)
		}
		if newLn != nil {
			oldAddr := mgr.serve(newLn)
			logger.Info("监听地址已切换", "addr", newLn.Addr().String(), "old_addr", oldAddr)
		}
		return nil
	})
	watcher.Start()

	// 优雅关闭：等待 SIGINT/SIGTERM。
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("收到退出信号，正在关闭服务...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	watcher.Stop()
	if historyStore != nil {
		if err := historyStore.Close(); err != nil {
			logger.Error("关闭执行历史存储失败", "error", err)
		}
	}
	if err := mgr.shutdown(ctx); err != nil {
		logger.Error("关闭服务失败", "error", err)
	}
	logger.Info("服务已退出")
}

// newHistoryStore 按配置构建执行历史存储。
func newHistoryStore(h config.HistoryConfig) (*history.Store, error) {
	return history.New(history.Options{
		Dir:            h.Dir,
		MaxFileSize:    int64(h.MaxFileSizeMB) << 20,
		MaxFiles:       h.MaxFiles,
		RetentionDays:  h.RetentionDaysValue(),
		MaxOutputBytes: h.MaxOutputBytes,
	})
}

// serverManager 管理 http.Server 的生命周期，支持监听地址热切换：
// 先绑定新地址（失败则不改动现状），再启动新服务，最后优雅关闭旧服务。
type serverManager struct {
	mu      sync.Mutex
	srv     *http.Server
	handler http.Handler
	logger  *slog.Logger
}

func newServerManager(handler http.Handler, logger *slog.Logger) *serverManager {
	return &serverManager{handler: handler, logger: logger}
}

// serve 用已绑定的监听器启动 HTTP 服务；若已有服务在运行，则优雅关闭它，
// 并返回被替换的旧监听地址（首次调用返回 ""）。
func (m *serverManager) serve(ln net.Listener) string {
	addr := ln.Addr().String()
	srv := &http.Server{
		Addr:              addr,
		Handler:           m.handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second, // 限制慢速请求体占用连接的时间
		IdleTimeout:       60 * time.Second,
	}
	m.mu.Lock()
	old := m.srv
	m.srv = srv
	m.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			m.logger.Error("服务异常退出", "addr", addr, "error", err)
		}
	}()

	if old != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := old.Shutdown(ctx); err != nil {
			m.logger.Warn("关闭旧服务失败", "error", err)
		}
		return old.Addr
	}
	return ""
}

// shutdown 优雅关闭当前服务。
func (m *serverManager) shutdown(ctx context.Context) error {
	m.mu.Lock()
	srv := m.srv
	m.srv = nil
	m.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}
