// Package executor 负责在受控环境中执行工作目录下的 shell 脚本。
package executor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// maxOutputSize 限制脚本 stdout/stderr 的返回大小，防止内存膨胀。
const maxOutputSize = 1 << 20 // 1MB

// Result 是脚本执行结果。
type Result struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out"`
}

// Executor 封装脚本路径解析与执行。
type Executor struct {
	scriptsDir          string // 脚本根目录（绝对路径）
	workDir             string // 脚本进程工作目录（绝对路径）
	allowOutsideScripts bool   // 是否允许执行脚本目录之外的脚本（配置文件 allow_outside_scripts 开启）
}

// New 创建执行器，并将 scriptsDir / workDir 解析为绝对路径。
// allowOutsideScripts 为 true 时，ResolveScript 不再限制脚本必须位于脚本目录内。
func New(scriptsDir, workDir string, allowOutsideScripts bool) (*Executor, error) {
	absScripts, err := filepath.Abs(scriptsDir)
	if err != nil {
		return nil, fmt.Errorf("解析脚本目录失败: %w", err)
	}
	absWork, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("解析工作目录失败: %w", err)
	}
	for _, d := range []struct{ name, path string }{{"脚本目录", absScripts}, {"工作目录", absWork}} {
		info, err := os.Stat(d.path)
		if err != nil || !info.IsDir() {
			return nil, fmt.Errorf("%s不存在或不是目录: %s", d.name, d.path)
		}
	}
	return &Executor{scriptsDir: absScripts, workDir: absWork, allowOutsideScripts: allowOutsideScripts}, nil
}

// ResolveScript 将脚本名解析为绝对路径。
//
// 默认（严格模式）：仅允许脚本目录内的脚本，拒绝路径穿越——
// 脚本名不得包含 ".."，解析结果必须位于脚本目录内。
// 开启 allowOutsideScripts 后：绝对路径直接使用，相对路径基于脚本目录解析，
// 允许指向脚本目录之外（如 ../other/foo.sh 或 /opt/scripts/foo.sh）。
func (e *Executor) ResolveScript(name string) (string, error) {
	if e.allowOutsideScripts {
		if filepath.IsAbs(name) {
			return filepath.Clean(name), nil
		}
		abs, err := filepath.Abs(filepath.Join(e.scriptsDir, name))
		if err != nil {
			return "", fmt.Errorf("解析脚本路径失败: %w", err)
		}
		return abs, nil
	}

	if filepath.IsAbs(name) {
		return "", fmt.Errorf("脚本名不允许使用绝对路径（如需越出脚本目录，请在配置中开启 allow_outside_scripts）: %s", name)
	}
	if strings.Contains(name, "..") {
		return "", fmt.Errorf("脚本名包含非法路径: %s", name)
	}
	abs, err := filepath.Abs(filepath.Join(e.scriptsDir, name))
	if err != nil {
		return "", fmt.Errorf("解析脚本路径失败: %w", err)
	}
	rel, err := filepath.Rel(e.scriptsDir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("脚本路径超出脚本目录: %s", name)
	}
	return abs, nil
}

// Execute 执行脚本：bash <script> [args...]，工作目录为 workDir。
//
// args 按顺序作为脚本位置参数；env 作为额外环境变量注入（不覆盖进程已有环境变量）。
// 返回的 err 仅表示系统级错误（超时、启动失败等）；
// 脚本自身退出码非 0 时通过 Result.ExitCode 反映，此时 err 为 nil。
func (e *Executor) Execute(ctx context.Context, scriptAbs string, args []string, env map[string]string, timeout time.Duration) (Result, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmdArgs := append([]string{scriptAbs}, args...)
	cmd := exec.CommandContext(cmdCtx, "/bin/bash", cmdArgs...)
	cmd.Dir = e.workDir
	// 将脚本进程放入独立的进程组：超时终止时连同其所有子进程一起杀掉，
	// 避免 bash 被杀死后子进程残留（如 tar/rsync/后台任务）继续运行。
	// 若不设置，子进程还会继承 stdout/stderr 管道，导致 cmd.Run() 等待
	// I/O 拷贝 goroutine 而无法返回（详见 WaitDelay 说明）。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// 杀整个进程组；bash 已退出（ESRCH）时退化为仅杀直接进程。
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			if err == syscall.ESRCH {
				return cmd.Process.Kill()
			}
			return err
		}
		return nil
	}
	// 进程组被终止后管道会立即关闭；WaitDelay 作为兜底，防止极端情况下
	// （如子进程自行脱离进程组）Run 无限等待 I/O 完成。
	cmd.WaitDelay = 5 * time.Second

	envList := os.Environ()
	for k, v := range env {
		envList = append(envList, k+"="+v)
	}
	cmd.Env = envList

	var stdout, stderr limitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	durationMS := time.Since(start).Milliseconds()

	res := Result{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		DurationMS: durationMS,
	}

	if cmdCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.Stderr = strings.TrimSpace(res.Stderr) + "\n[执行超时，进程已被终止]"
		return res, fmt.Errorf("脚本执行超时（%s）", timeout)
	}
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			// 脚本自身退出码非 0：属于业务结果，不是系统错误。
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("执行脚本失败: %w", runErr)
	}
	return res, nil
}

// limitedBuffer 限制写入总量，超过上限后丢弃多余输出并在末尾标记截断。
type limitedBuffer struct {
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len() >= maxOutputSize {
		return len(p), nil
	}
	remain := maxOutputSize - b.buf.Len()
	if len(p) > remain {
		b.buf.Write(p[:remain])
		b.buf.WriteString("\n... [输出已截断]")
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string { return b.buf.String() }
