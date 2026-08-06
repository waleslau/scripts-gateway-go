// Package history 提供脚本执行历史的持久化存储：
// 以 JSONL（每行一条 JSON 记录）追加写入，支持文件滚动、保留清理与查询。
//
// 记录策略：
//   - 成功执行（退出码 0 且无系统错误）：始终记录；
//   - 失败执行（退出码非 0 或系统级错误）：单个任务连续失败 3 次以内记录，
//     第 4 次起跳过，直到该任务下一次成功执行后重置计数（计数仅存内存，重启清零）。
package history

import (
	"strings"
	"time"
)

// Record 是单次脚本执行的完整记录。
type Record struct {
	ID         string            `json:"id"` // 时间戳+随机后缀，如 20260807T004533-a1b2c3d4
	Time       time.Time         `json:"time"`
	Task       string            `json:"task"`
	Method     string            `json:"method"`
	Script     string            `json:"script"`
	Params     map[string]string `json:"params,omitempty"`
	RemoteAddr string            `json:"remote_addr"`
	ExitCode   int               `json:"exit_code"` // -1 表示系统级失败（超时/启动失败等）
	DurationMS int64             `json:"duration_ms"`
	TimedOut   bool              `json:"timed_out"`
	HTTPStatus int               `json:"http_status"`
	// Stdout 的落盘形态由调用方（server 层）按 server.stdout_format 决定：
	// text 时为原始字符串；lines 时为行数组 []string。读取方按落盘形态获取，
	// 不做格式转换。
	Stdout any    `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Failed 判断该记录是否属于失败执行：脚本退出码非 0 或发生系统级错误。
func (r Record) Failed() bool { return r.ExitCode != 0 || r.Error != "" }

// SplitLines 将 stdout 字符串按行拆分（stdout_format=lines 时使用）：
//   - 结尾换行符不产生多余的空元素（"a\nb\n" -> ["a","b"]）；
//   - 保留行内空白与空行，仅去除行尾的 \r（兼容 CRLF）；
//   - 空输出返回空数组。
func SplitLines(s string) []string {
	if s == "" {
		return []string{}
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}
