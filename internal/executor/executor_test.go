package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecuteSuccess(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "ok.sh", "#!/bin/bash\necho \"hello $1\"\n")
	ex, err := New(dir, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ex.Execute(context.Background(), script, []string{"world"}, nil, 5*time.Second)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.ExitCode != 0 || res.Stdout != "hello world\n" {
		t.Fatalf("结果不符: %+v", res)
	}
}

func TestExecuteScriptExitCode(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, "fail.sh", "#!/bin/bash\necho boom >&2\nexit 3\n")
	ex, err := New(dir, dir, false)
	if err != nil {
		t.Fatal(err)
	}
	res, err := ex.Execute(context.Background(), script, nil, nil, 5*time.Second)
	if err != nil {
		t.Fatalf("脚本自身退出码非 0 不应返回系统错误: %v", err)
	}
	if res.ExitCode != 3 || res.Stderr != "boom\n" {
		t.Fatalf("结果不符: %+v", res)
	}
}

// TestExecuteTimeoutKillsChildProcesses 验证超时后：
//  1. Execute 不会被残留子进程的管道拖住（应快速返回）；
//  2. bash 派生的子进程会被一并终止（进程组强杀）。
func TestExecuteTimeoutKillsChildProcesses(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := writeScript(t, dir, "spawn.sh",
		"#!/bin/bash\nsleep 120 &\necho $! > "+pidFile+"\nwait $!\n")
	ex, err := New(dir, dir, false)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	res, err := ex.Execute(ctx, script, nil, nil, 500*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("应返回超时错误, res=%+v", res)
	}
	if !res.TimedOut {
		t.Fatalf("TimedOut 应为 true, res=%+v", res)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("Execute 被残留子进程拖住: 耗时 %v", elapsed)
	}

	pidBytes, _ := os.ReadFile(pidFile)
	pid := strings.TrimSpace(string(pidBytes))
	if pid == "" {
		t.Fatal("脚本未写出子进程 PID")
	}
	waitProcDead(t, pid)
}

// waitProcDead 轮询等待进程退出（Z 状态或已不存在），最多 5 秒。
func waitProcDead(t *testing.T, pid string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, ok := procState(pid)
		if !ok || (state != "R" && state != "S" && state != "D") {
			return // 已退出（含僵尸 Z）或已不存在
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("子进程 %s 在超时后 5 秒内仍未终止（残留）", pid)
}

// procState 读取 /proc/<pid>/stat 中的进程状态字符；文件不存在时 ok=false。
func procState(pid string) (string, bool) {
	data, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return "", false
	}
	// 进程名可能含空格/括号，从最后一个 ')' 之后取状态字段。
	s := string(data)
	idx := strings.LastIndexByte(s, ')')
	if idx < 0 || idx+2 >= len(s) {
		return "", true
	}
	return s[idx+2 : idx+3], true
}
