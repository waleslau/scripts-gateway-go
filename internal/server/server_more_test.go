package server

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scripts-gateway-go/internal/config"
	"scripts-gateway-go/internal/executor"
)

// newCustomTestServer 构建一个测试服务，允许在创建前调整配置，返回服务与临时目录。
func newCustomTestServer(t *testing.T, mutate func(*config.Config)) (*Server, string) {
	t.Helper()
	base := t.TempDir()
	scriptsDir := filepath.Join(base, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scripts := map[string]string{
		"echo.sh": "#!/bin/bash\necho \"hello $1\"\n",
		"slow.sh": "#!/bin/bash\ntouch " + filepath.Join(base, "started.flag") + "\nsleep 1\n",
	}
	for name, content := range scripts {
		if err := os.WriteFile(filepath.Join(scriptsDir, name), []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		Server:     config.ServerConfig{Addr: ":0", StdoutFormat: "text"},
		ScriptsDir: scriptsDir,
		WorkDir:    base,
		Mappings: []config.Mapping{
			{Name: "echo", Method: "POST", Script: "echo.sh", TimeoutSeconds: 5,
				Params: []config.Param{{Name: "name", Required: true}}},
			{Name: "slow", Method: "POST", Script: "slow.sh", TimeoutSeconds: 10},
		},
	}
	if mutate != nil {
		mutate(cfg)
	}
	store := config.NewStore(cfg)
	ex, err := executor.New(scriptsDir, base, false)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(store, ex, logger, nil), base
}

func TestAuthAndHealthzExempt(t *testing.T) {
	srv, _ := newCustomTestServer(t, func(c *config.Config) {
		c.Server.AuthToken = "secret-token"
	})

	// 未带令牌 -> 401
	rec, _ := doReq(t, srv, "POST", "/api/v1/tasks/echo", `{"name":"world"}`)
	if rec.Code != 401 {
		t.Fatalf("未带令牌应 401, got %d", rec.Code)
	}

	// 错误令牌 -> 401
	req := httptest.NewRequest("POST", "/api/v1/tasks/echo", strings.NewReader(`{"name":"world"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer wrong")
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req)
	if rec2.Code != 401 {
		t.Fatalf("错误令牌应 401, got %d", rec2.Code)
	}

	// 正确令牌 -> 200
	req = httptest.NewRequest("POST", "/api/v1/tasks/echo", strings.NewReader(`{"name":"world"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth-Token", "secret-token")
	rec3 := httptest.NewRecorder()
	srv.ServeHTTP(rec3, req)
	if rec3.Code != 200 {
		t.Fatalf("正确令牌应 200, got %d", rec3.Code)
	}

	// /healthz 免鉴权
	req = httptest.NewRequest("GET", "/healthz", nil)
	rec4 := httptest.NewRecorder()
	srv.ServeHTTP(rec4, req)
	if rec4.Code != 200 {
		t.Fatalf("healthz 免鉴权应 200, got %d", rec4.Code)
	}
}

// TestJSONNumberPrecision 大整数参数不应因 float64 解析而丢精度。
func TestJSONNumberPrecision(t *testing.T) {
	srv, _ := newCustomTestServer(t, nil)
	rec, resp := doReq(t, srv, "POST", "/api/v1/tasks/echo",
		`{"name":12345678901234567890}`)
	if rec.Code != 200 {
		t.Fatalf("执行失败: %d %s", rec.Code, rec.Body.String())
	}
	data := resp["data"].(map[string]any)
	if data["stdout"] != "hello 12345678901234567890\n" {
		t.Fatalf("大整数丢精度: stdout=%v", data["stdout"])
	}
}

// TestMaxConcurrentLimit 并发上限：占用满时新请求快速返回 503，
// 释放后恢复执行。
func TestMaxConcurrentLimit(t *testing.T) {
	srv, base := newCustomTestServer(t, func(c *config.Config) {
		c.Server.MaxConcurrent = 1
	})
	startedFlag := filepath.Join(base, "started.flag")

	done := make(chan struct{})
	go func() {
		defer close(done)
		doReq(t, srv, "POST", "/api/v1/tasks/slow", "")
	}()

	// 等待第一个请求真正开始执行脚本
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(startedFlag); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(startedFlag); err != nil {
		t.Fatal("第一个请求未开始执行")
	}

	// 槽位占满：第二个请求立即 503
	rec, _ := doReq(t, srv, "POST", "/api/v1/tasks/slow", "")
	if rec.Code != 503 {
		t.Fatalf("占满时应 503, got %d", rec.Code)
	}

	<-done // 等第一个请求结束

	// 释放后恢复执行
	os.Remove(startedFlag)
	rec2, _ := doReq(t, srv, "POST", "/api/v1/tasks/slow", "")
	if rec2.Code != 200 {
		t.Fatalf("释放后应恢复, got %d", rec2.Code)
	}
}

// TestMaxConcurrentHotUpdate 并发上限热更新：占用的槽位来自旧信号量，
// 释放时必须归还旧信号量，不能阻塞在重建后的新信号量上。
func TestMaxConcurrentHotUpdate(t *testing.T) {
	srv, base := newCustomTestServer(t, func(c *config.Config) {
		c.Server.MaxConcurrent = 1
	})
	startedFlag := filepath.Join(base, "started.flag")

	done := make(chan struct{})
	go func() {
		defer close(done)
		doReq(t, srv, "POST", "/api/v1/tasks/slow", "")
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(startedFlag); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(startedFlag); err != nil {
		t.Fatal("第一个请求未开始执行")
	}

	// 请求进行中热更新上限：旧请求应能正常完成并归还旧信号量。
	srv.SetMaxConcurrent(2)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("热更新后旧请求未完成（可能在错误信号量上阻塞）")
	}

	// 新上限下请求正常执行
	os.Remove(startedFlag)
	rec, _ := doReq(t, srv, "POST", "/api/v1/tasks/slow", "")
	if rec.Code != 200 {
		t.Fatalf("热更新后执行失败, got %d", rec.Code)
	}
}
