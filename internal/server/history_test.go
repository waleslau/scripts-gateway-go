package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"scripts-gateway-go/internal/config"
	"scripts-gateway-go/internal/executor"
	"scripts-gateway-go/internal/history"
)

// newHistoryTestServer 构建一个带（或不带）执行历史的测试服务，
// scripts 目录内置 echo.sh（成功）与 fail.sh（退出码 1）。
func newHistoryTestServer(t *testing.T, histEnabled bool) (*Server, string) {
	t.Helper()
	base := t.TempDir()
	scriptsDir := filepath.Join(base, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scripts := map[string]string{
		"echo.sh":   "#!/bin/bash\necho \"hello $1\"\n",
		"fail.sh":   "#!/bin/bash\necho \"boom\" >&2\nexit 1\n",
		"choose.sh": "#!/bin/bash\n[ \"$1\" = \"fail\" ] && exit 1\nexit 0\n",
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
			{Name: "fail", Method: "POST", Script: "fail.sh", TimeoutSeconds: 5},
			{Name: "choose", Method: "POST", Script: "choose.sh", TimeoutSeconds: 5,
				Params: []config.Param{{Name: "mode", Required: true}}},
		},
	}
	store := config.NewStore(cfg)
	ex, err := executor.New(scriptsDir, base, false)
	if err != nil {
		t.Fatal(err)
	}
	var hs *history.Store
	if histEnabled {
		hs, err = history.New(history.Options{
			Dir:            filepath.Join(base, "history"),
			MaxFileSize:    1 << 20,
			MaxFiles:       10,
			RetentionDays:  0,
			MaxOutputBytes: 4096,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { hs.Close() })
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(store, ex, logger, hs), base
}

// doReq 发送请求并解析统一响应。
func doReq(t *testing.T, s *Server, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v body=%s", err, rec.Body.String())
	}
	return rec, resp
}

// TestHistoryEndToEnd 任务执行 -> 落历史 -> 列表/详情查询。
func TestHistoryEndToEnd(t *testing.T) {
	srv, _ := newHistoryTestServer(t, true)

	rec, resp := doReq(t, srv, "POST", "/api/v1/tasks/echo", `{"name":"world"}`)
	if rec.Code != 200 || resp["code"].(float64) != 0 {
		t.Fatalf("执行失败: code=%d resp=%v", rec.Code, resp)
	}

	rec, resp = doReq(t, srv, "GET", "/api/v1/history?task=echo", "")
	if rec.Code != 200 {
		t.Fatalf("历史查询失败: %d %s", rec.Code, rec.Body.String())
	}
	data := resp["data"].(map[string]any)
	if data["total"].(float64) != 1 {
		t.Fatalf("total=%v, want 1", data["total"])
	}
	items := data["items"].([]any)
	item := items[0].(map[string]any)
	if item["task"] != "echo" || item["exit_code"].(float64) != 0 {
		t.Fatalf("记录内容不符: %v", item)
	}
	if _, has := item["stdout"]; has {
		t.Error("列表不应包含 stdout")
	}
	id, _ := item["id"].(string)

	// 详情：含完整输出与参数
	rec, resp = doReq(t, srv, "GET", "/api/v1/history/"+id, "")
	if rec.Code != 200 {
		t.Fatalf("详情查询失败: %d", rec.Code)
	}
	detail := resp["data"].(map[string]any)
	if detail["stdout"] != "hello world\n" {
		t.Errorf("详情 stdout=%v", detail["stdout"])
	}
	if detail["params"].(map[string]any)["name"] != "world" {
		t.Errorf("详情 params=%v", detail["params"])
	}

	// 不存在的 ID -> 404
	rec, _ = doReq(t, srv, "GET", "/api/v1/history/no-such-id", "")
	if rec.Code != 404 {
		t.Errorf("不存在的 ID 应 404, got %d", rec.Code)
	}
}

// TestHistoryStdoutFormat history.stdout_format 独立控制历史 stdout 形态：
// lines 时历史详情与落盘 JSONL 均为行数组；server.stdout_format 保持默认
// text，任务执行响应仍是原始字符串（两者互不影响）。
func TestHistoryStdoutFormat(t *testing.T) {
	srv, base := newHistoryTestServer(t, true)

	// 热更新语义：切换 history.stdout_format=lines 后无需重启即生效。
	cfg := srv.store.Load()
	cfgCopy := *cfg
	cfgCopy.History.StdoutFormat = "lines"
	srv.store.Update(&cfgCopy)

	// 任务执行：server.stdout_format=text，响应 stdout 为原始字符串。
	rec, resp := doReq(t, srv, "POST", "/api/v1/tasks/echo", `{"name":"world"}`)
	if rec.Code != 200 {
		t.Fatalf("执行失败: %d %s", rec.Code, rec.Body.String())
	}
	if taskStdout := resp["data"].(map[string]any)["stdout"]; taskStdout != "hello world\n" {
		t.Fatalf("任务结果 stdout 应保持原始字符串: %#v", taskStdout)
	}

	// 历史详情 stdout 为行数组。
	_, resp = doReq(t, srv, "GET", "/api/v1/history", "")
	id := resp["data"].(map[string]any)["items"].([]any)[0].(map[string]any)["id"].(string)
	rec, resp = doReq(t, srv, "GET", "/api/v1/history/"+id, "")
	if rec.Code != 200 {
		t.Fatalf("详情查询失败: %d %s", rec.Code, rec.Body.String())
	}
	detailStdout, ok := resp["data"].(map[string]any)["stdout"].([]any)
	if !ok || len(detailStdout) != 1 || detailStdout[0] != "hello world" {
		t.Fatalf("历史详情 stdout 应为行数组: %v", resp["data"])
	}

	// 落盘 JSONL 的 stdout 同样为行数组。
	var diskStdout []any
	entries, err := os.ReadDir(filepath.Join(base, "history"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "run-") || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(base, "history", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				continue
			}
			if m["id"] == id {
				diskStdout, _ = m["stdout"].([]any)
			}
		}
	}
	if len(diskStdout) != 1 || diskStdout[0] != "hello world" {
		t.Errorf("落盘 JSONL stdout 应为行数组: %#v", diskStdout)
	}
}

// TestHistoryStdoutFormatFollowServer history.stdout_format 未配置时默认跟随
// server.stdout_format：只设置 server=lines，任务响应、历史详情、落盘均为行数组。
func TestHistoryStdoutFormatFollowServer(t *testing.T) {
	srv, base := newHistoryTestServer(t, true)
	cfg := srv.store.Load()
	cfgCopy := *cfg
	cfgCopy.Server.StdoutFormat = "lines"
	srv.store.Update(&cfgCopy)

	rec, resp := doReq(t, srv, "POST", "/api/v1/tasks/echo", `{"name":"world"}`)
	if rec.Code != 200 {
		t.Fatalf("执行失败: %d %s", rec.Code, rec.Body.String())
	}
	if taskStdout, ok := resp["data"].(map[string]any)["stdout"].([]any); !ok ||
		len(taskStdout) != 1 || taskStdout[0] != "hello world" {
		t.Fatalf("任务结果 stdout 应为行数组: %#v", resp["data"])
	}

	_, resp = doReq(t, srv, "GET", "/api/v1/history", "")
	id := resp["data"].(map[string]any)["items"].([]any)[0].(map[string]any)["id"].(string)
	_, resp = doReq(t, srv, "GET", "/api/v1/history/"+id, "")
	detailStdout, ok := resp["data"].(map[string]any)["stdout"].([]any)
	if !ok || len(detailStdout) != 1 || detailStdout[0] != "hello world" {
		t.Fatalf("历史详情 stdout 应为行数组: %v", resp["data"])
	}

	// 落盘 JSONL 同样为行数组。
	entries, err := os.ReadDir(filepath.Join(base, "history"))
	if err != nil {
		t.Fatal(err)
	}
	var diskStdout []any
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "run-") || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(base, "history", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				continue
			}
			if m["id"] == id {
				diskStdout, _ = m["stdout"].([]any)
			}
		}
	}
	if len(diskStdout) != 1 || diskStdout[0] != "hello world" {
		t.Errorf("落盘 JSONL stdout 应为行数组: %#v", diskStdout)
	}
}

// TestHistoryFailurePolicyHTTP 通过 HTTP 层验证记录策略（按任务计连续失败）：
// 同一任务连续失败 4 次记录 3 条；该任务自身成功执行后重置计数。
func TestHistoryFailurePolicyHTTP(t *testing.T) {
	srv, _ := newHistoryTestServer(t, true)
	for i := 0; i < 4; i++ {
		rec, _ := doReq(t, srv, "POST", "/api/v1/tasks/choose", `{"mode":"fail"}`)
		if rec.Code != 200 {
			t.Fatalf("第 %d 次执行失败: %d", i, rec.Code)
		}
	}
	_, resp := doReq(t, srv, "GET", "/api/v1/history?task=choose", "")
	data := resp["data"].(map[string]any)
	if data["total"].(float64) != 3 {
		t.Errorf("连续失败 4 次应记录 3 条, total=%v", data["total"])
	}

	// 该任务自身成功执行后重置计数
	doReq(t, srv, "POST", "/api/v1/tasks/choose", `{"mode":"ok"}`)
	for i := 0; i < 3; i++ {
		doReq(t, srv, "POST", "/api/v1/tasks/choose", `{"mode":"fail"}`)
	}
	_, resp = doReq(t, srv, "GET", "/api/v1/history?task=choose", "")
	data = resp["data"].(map[string]any)
	if data["total"].(float64) != 7 { // 3 失败 + 1 成功 + 3 失败
		t.Errorf("重置后 choose 记录数=%v, want 7", data["total"])
	}
}

// TestHistoryQueryValidation 非法查询参数返回 400。
func TestHistoryQueryValidation(t *testing.T) {
	srv, _ := newHistoryTestServer(t, true)
	for _, path := range []string{
		"/api/v1/history?exit_code=abc",
		"/api/v1/history?from=not-a-time",
		"/api/v1/history?limit=0",
		"/api/v1/history?offset=-1",
	} {
		rec, _ := doReq(t, srv, "GET", path, "")
		if rec.Code != 400 {
			t.Errorf("%s 应 400, got %d", path, rec.Code)
		}
	}
}

// TestHistoryClearAndDisabled 清空历史与未启用时的行为。
func TestHistoryClearAndDisabled(t *testing.T) {
	srv, _ := newHistoryTestServer(t, true)
	doReq(t, srv, "POST", "/api/v1/tasks/echo", `{"name":"a"}`)
	rec, _ := doReq(t, srv, "DELETE", "/api/v1/history", "")
	if rec.Code != 200 {
		t.Fatalf("清空失败: %d", rec.Code)
	}
	_, resp := doReq(t, srv, "GET", "/api/v1/history", "")
	data := resp["data"].(map[string]any)
	if data["total"].(float64) != 0 {
		t.Errorf("清空后 total=%v, want 0", data["total"])
	}

	srv2, _ := newHistoryTestServer(t, false)
	for _, path := range []string{"/api/v1/history", "/api/v1/history/x", "/api/v1/history"} {
		rec, _ := doReq(t, srv2, "GET", path, "")
		if rec.Code != 404 {
			t.Errorf("未启用时 GET %s 应 404, got %d", path, rec.Code)
		}
	}
	rec, _ = doReq(t, srv2, "DELETE", "/api/v1/history", "")
	if rec.Code != 404 {
		t.Errorf("未启用时 DELETE 应 404, got %d", rec.Code)
	}
}
