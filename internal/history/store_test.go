package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func newTestStore(t *testing.T, mutate func(*Options)) *Store {
	t.Helper()
	opts := Options{
		Dir:            filepath.Join(t.TempDir(), "history"),
		MaxFileSize:    1 << 20,
		MaxFiles:       10,
		RetentionDays:  0,
		MaxOutputBytes: 4096,
	}
	if mutate != nil {
		mutate(&opts)
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func rec(task string, exitCode int, extra func(*Record)) Record {
	r := Record{
		Task:       task,
		Method:     "POST",
		Script:     task + ".sh",
		Params:     map[string]string{"a": "1"},
		RemoteAddr: "127.0.0.1:1234",
		ExitCode:   exitCode,
		DurationMS: 12,
		HTTPStatus: 200,
		Stdout:     "out-" + task,
		Stderr:     "",
	}
	if extra != nil {
		extra(&r)
	}
	return r
}

// appendRec 便捷封装：Append 接收 *Record 以便填充 ID/Time。
func appendRec(s *Store, r Record) (bool, error) {
	return s.Append(&r)
}

func TestAppendAndList(t *testing.T) {
	s := newTestStore(t, nil)
	var ids []string
	for i := 0; i < 3; i++ {
		r := rec(fmt.Sprintf("task%d", i), 0, nil)
		written, err := s.Append(&r)
		if err != nil || !written {
			t.Fatalf("Append %d: written=%v err=%v", i, written, err)
		}
		if r.ID == "" || r.Time.IsZero() {
			t.Fatalf("Append 未填充 ID/Time: %+v", r)
		}
		ids = append(ids, r.ID)
	}

	// 倒序返回（最新的在前）
	items, total, err := s.List(Query{Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 3 || len(items) != 3 {
		t.Fatalf("total=%d len=%d, want 3/3", total, len(items))
	}
	for i, wantID := range []string{ids[2], ids[1], ids[0]} {
		if items[i].ID != wantID {
			t.Errorf("items[%d].ID=%s, want %s", i, items[i].ID, wantID)
		}
	}
	// 默认不含输出
	if items[0].Stdout != nil || items[0].Stderr != "" {
		t.Errorf("列表默认应不含输出: %+v", items[0])
	}
	// 显式要求输出
	items, _, _ = s.List(Query{Limit: 1, IncludeOutput: true})
	if items[0].Stdout == "" {
		t.Error("IncludeOutput=true 时应返回 stdout")
	}
}

func TestGetByID(t *testing.T) {
	s := newTestStore(t, nil)
	r := rec("deploy", 0, func(r *Record) { r.Stdout = "deploy ok" })
	if _, err := s.Append(&r); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(r.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil || got.Stdout != "deploy ok" || got.Task != "deploy" {
		t.Fatalf("Get 结果不符: %+v", got)
	}
	if got, _ := s.Get("no-such-id"); got != nil {
		t.Error("不存在的 ID 应返回 nil")
	}
}

func TestListFilterAndPagination(t *testing.T) {
	s := newTestStore(t, nil)
	// 生成 25 条：deploy 成功 5 条 + fail 失败 3 条，再补其他任务
	for i := 0; i < 5; i++ {
		appendRec(s, rec("deploy", 0, nil))
	}
	appendRec(s, rec("fail", 1, nil))
	appendRec(s, rec("fail", 2, nil))
	appendRec(s, rec("fail", 3, nil))
	appendRec(s, rec("other", 0, nil))

	// task 过滤
	items, total, _ := s.List(Query{Task: "deploy", Limit: 10})
	if total != 5 || len(items) != 5 {
		t.Errorf("deploy: total=%d len=%d, want 5/5", total, len(items))
	}
	// exit_code 过滤
	code := 2
	items, total, _ = s.List(Query{ExitCode: &code, Limit: 10})
	if total != 1 || items[0].ExitCode != 2 {
		t.Errorf("exit_code=2: total=%d, want 1", total)
	}
	// 分页
	items, total, _ = s.List(Query{Limit: 3, Offset: 0})
	if len(items) != 3 || total != 9 {
		t.Errorf("page1: len=%d total=%d, want 3/9", len(items), total)
	}
	items, total, _ = s.List(Query{Limit: 3, Offset: 6})
	if len(items) != 3 || total != 9 {
		t.Errorf("page3: len=%d total=%d, want 3/9", len(items), total)
	}
	items, _, _ = s.List(Query{Limit: 3, Offset: 100})
	if len(items) != 0 {
		t.Errorf("越界 offset 应返回空列表: %d", len(items))
	}
	// 空结果返回空数组而非 nil
	items, _, _ = s.List(Query{Task: "ghost", Limit: 10})
	if items == nil || len(items) != 0 {
		t.Errorf("空结果应为 []: %#v", items)
	}
}

// TestFailurePolicy 验证记录策略：成功始终记录；单任务连续失败 3 次以内记录，
// 第 4 次起跳过；成功执行后重置计数。
func TestFailurePolicy(t *testing.T) {
	s := newTestStore(t, nil)
	// 连续失败 4 次：记录前 3 次，跳过第 4 次
	for i := 1; i <= 4; i++ {
		r := rec("fragile", 1, nil)
		written, err := s.Append(&r)
		if err != nil {
			t.Fatal(err)
		}
		want := i <= 3
		if written != want {
			t.Errorf("第 %d 次失败: written=%v, want %v", i, written, want)
		}
	}
	items, total, _ := s.List(Query{Task: "fragile", Limit: 10})
	if total != 3 || len(items) != 3 {
		t.Errorf("fragile 失败记录数=%d/%d, want 3/3", total, len(items))
	}

	// 成功执行重置计数：之后连续失败 3 次又会被记录
	appendRec(s, rec("fragile", 0, nil))
	for i := 1; i <= 3; i++ {
		written, _ := appendRec(s, rec("fragile", 1, nil))
		if !written {
			t.Fatalf("重置后第 %d 次失败应被记录", i)
		}
	}
	items, total, _ = s.List(Query{Task: "fragile", Limit: 100})
	if total != 7 || len(items) != 7 { // 3 + 1 成功 + 3
		t.Errorf("重置后总记录数=%d/%d, want 7/7", total, len(items))
	}

	// 不同任务互不影响
	appendRec(s, rec("other", 1, nil))
	items, total, _ = s.List(Query{Task: "other", Limit: 10})
	if total != 1 {
		t.Errorf("other 记录数=%d, want 1", total)
	}
}

// TestSystemErrorIsFailure 系统级错误（ExitCode=-1 + Error）同样计入连续失败。
func TestSystemErrorIsFailure(t *testing.T) {
	s := newTestStore(t, nil)
	for i := 1; i <= 4; i++ {
		r := rec("timeout-task", -1, func(r *Record) { r.Error = "脚本执行超时" })
		written, err := s.Append(&r)
		if err != nil {
			t.Fatal(err)
		}
		if written != (i <= 3) {
			t.Errorf("第 %d 次系统失败: written=%v, want %v", i, written, i <= 3)
		}
	}
}

func TestRotation(t *testing.T) {
	s := newTestStore(t, func(o *Options) { o.MaxFileSize = 256 })
	for i := 0; i < 50; i++ {
		r := rec(fmt.Sprintf("t%d", i%3), 0, func(r *Record) {
			r.Stdout = string(make([]byte, 100))
		})
		if _, err := s.Append(&r); err != nil {
			t.Fatal(err)
		}
	}
	_, total, err := s.List(Query{Limit: 1000})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 50 {
		t.Errorf("滚动后记录总数=%d, want 50", total)
	}
	files, err := s.listFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 2 {
		t.Errorf("应产生多个文件, 实际 %d 个: %v", len(files), files)
	}
}

func TestCleanupByMaxFiles(t *testing.T) {
	s := newTestStore(t, func(o *Options) { o.MaxFiles = 3; o.MaxFileSize = 64 })
	for i := 0; i < 30; i++ {
		r := rec("bulk", 0, func(r *Record) { r.Stdout = string(make([]byte, 200)) })
		s.Append(&r)
	}
	files, _ := s.listFiles()
	if len(files) != 30 {
		t.Fatalf("应产生 30 个文件（滚动不自动清理）, 实际 %d", len(files))
	}
	s.Cleanup()
	files, _ = s.listFiles()
	if len(files) != 3 {
		t.Errorf("Cleanup 后文件数=%d, want 3", len(files))
	}
}

func TestCleanupByRetention(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	os.MkdirAll(dir, 0o750)
	// 手工构造一个"过期"文件与一个"新鲜"文件
	old := filepath.Join(dir, "run-20260101T000000-aaaa.jsonl")
	newf := filepath.Join(dir, "run-20260807T000000-bbbb.jsonl")
	for _, p := range []string{old, newf} {
		if err := os.WriteFile(p, []byte("{\"id\":\"x\"}\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().Add(-90 * 24 * time.Hour)
	os.Chtimes(old, oldTime, oldTime)

	s, err := New(Options{Dir: dir, RetentionDays: 30})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()
	s.Cleanup()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("超过保留天数的文件应被删除")
	}
	if _, err := os.Stat(newf); err != nil {
		t.Error("未过期的文件应保留")
	}
}

// TestTailRecovery 崩溃残留的末尾半行应在启动时被截断。
func TestTailRecovery(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	os.MkdirAll(dir, 0o750)
	full := rec("ok", 0, nil)
	data, _ := json.Marshal(full)
	path := filepath.Join(dir, "run-20260807T000000-aaaa.jsonl")
	content := string(data) + "\n" + string(data)[:20] // 完整一行 + 半行
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}

	s, err := New(Options{Dir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	// 启动恢复后，文件应只剩完整一行（半行被截断）。
	before, _ := os.ReadFile(path)
	if len(before) != len(data)+1 {
		t.Errorf("半行未截断: len=%d, want %d", len(before), len(data)+1)
	}

	// 再追加一条，确保文件仍是合法 JSONL
	appendRec(s, rec("after", 0, nil))
	rest, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) == 0 || rest[len(rest)-1] != '\n' {
		t.Errorf("恢复后文件应以换行结尾")
	}
	var decoded []Record
	for _, line := range splitLinesBytes(rest) {
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("恢复后存在非法行: %v", err)
		}
		decoded = append(decoded, r)
	}
	if len(decoded) != 2 {
		t.Errorf("恢复后应剩 2 条完整记录, 实际 %d", len(decoded))
	}
}

// TestTailTruncationLargePartial 回归：残留半行超过 64KB 时，
// 只截掉半行，不能把整个文件（含之前完整写入的记录）清空。
func TestTailTruncationLargePartial(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "history")
	os.MkdirAll(dir, 0o750)
	path := filepath.Join(dir, "run-20260807T000000-aaaa.jsonl")
	complete := `{"id":"keep-me"}` + "\n"
	partial := fmt.Sprintf(`{"id":"%s`, strings.Repeat("a", 70<<10)) // 70KB 残留半行
	if err := os.WriteFile(path, []byte(complete+partial), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := truncatePartialTail(path); err != nil {
		t.Fatalf("truncatePartialTail: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != complete {
		t.Fatalf("应保留完整记录 %q，实际文件内容 %q", complete, data)
	}
}

// TestCleanupKeepsCurrent 回归：清理不能删除当前正在追加的文件，
// 否则后续 Append 会写进已删除的 inode。
func TestCleanupKeepsCurrent(t *testing.T) {
	s := newTestStore(t, func(o *Options) { o.MaxFiles = 1 })
	appendRec(s, rec("a", 0, nil))
	curName := s.curName
	if curName == "" {
		t.Fatal("curName 未设置")
	}
	// 把当前文件改成"过期"状态（修改时间推到很久以前）
	old := time.Now().Add(-100 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(s.dir, curName), old, old); err != nil {
		t.Fatal(err)
	}
	s.Cleanup()
	if _, err := os.Stat(filepath.Join(s.dir, curName)); err != nil {
		t.Fatalf("当前文件 %s 被清理删除", curName)
	}
	// 清理后仍可正常追加
	if written, err := appendRec(s, rec("b", 0, nil)); err != nil || !written {
		t.Fatalf("清理后追加失败: written=%v err=%v", written, err)
	}
}

func TestClear(t *testing.T) {
	s := newTestStore(t, nil)
	appendRec(s, rec("a", 0, nil))
	appendRec(s, rec("b", 0, nil))
	if err := s.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	_, total, _ := s.List(Query{Limit: 10})
	if total != 0 {
		t.Errorf("清空后 total=%d, want 0", total)
	}
	// 清空后仍可继续追加
	appendRec(s, rec("c", 0, nil))
	_, total, _ = s.List(Query{Limit: 10})
	if total != 1 {
		t.Errorf("清空后追加 total=%d, want 1", total)
	}
}

func TestOutputTruncation(t *testing.T) {
	s := newTestStore(t, func(o *Options) { o.MaxOutputBytes = 10 })
	r := rec("big", 0, func(r *Record) { r.Stdout = "0123456789ABCDEF" })
	if _, err := s.Append(&r); err != nil {
		t.Fatal(err)
	}
	stdout, ok := r.Stdout.(string)
	if !ok {
		t.Fatalf("string 形态 stdout 截断后应保持 string: %T %v", r.Stdout, r.Stdout)
	}
	if len(stdout) > 10+len(truncatedMarker) {
		t.Errorf("stdout 未截断: %q", stdout)
	}
	got, _ := s.Get(r.ID)
	if got == nil {
		t.Fatal("Get 返回 nil")
	}
	gotStdout, ok := got.Stdout.(string)
	if !ok || len(gotStdout) > 10+len(truncatedMarker) {
		t.Errorf("落盘 stdout 未截断: %T %+v", got.Stdout, got)
	}
	if gotStdout != "0123456789"+truncatedMarker {
		t.Errorf("截断结果不符: %q", gotStdout)
	}
}

// TestOutputTruncationLines lines 形态（[]string）的截断：join 后按同一规则
// 截断并重新拆行，截断标记成为最后一个元素；未超限时数组原样落盘。
func TestOutputTruncationLines(t *testing.T) {
	s := newTestStore(t, func(o *Options) { o.MaxOutputBytes = 10 })
	r := rec("big", 0, func(r *Record) { r.Stdout = []string{"0123456789ABCDEF", "tail"} })
	if _, err := s.Append(&r); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(r.ID)
	lines, ok := got.Stdout.([]string)
	if !ok {
		t.Fatalf("lines 形态 stdout 应存为 []string: %T %v", got.Stdout, got.Stdout)
	}
	if joined := strings.Join(lines, "\n"); len(joined) > 10+len(truncatedMarker) {
		t.Errorf("截断后超过上限: %q", joined)
	}
	if last := lines[len(lines)-1]; last != strings.TrimPrefix(truncatedMarker, "\n") {
		t.Errorf("最后一行应为截断标记, got %q", last)
	}

	// 未超限：数组原样落盘（不改变调用方传入的形态）
	s2 := newTestStore(t, func(o *Options) { o.MaxOutputBytes = 1024 })
	r2 := rec("small", 0, func(r *Record) { r.Stdout = []string{"a", "b"} })
	if _, err := s2.Append(&r2); err != nil {
		t.Fatal(err)
	}
	got2, _ := s2.Get(r2.ID)
	if want := []string{"a", "b"}; !reflect.DeepEqual(got2.Stdout, want) {
		t.Errorf("未超限时应原样落盘: got %#v, want %#v", got2.Stdout, want)
	}
}

func splitLinesBytes(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	return out
}

func TestSplitLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"空输出", "", []string{}},
		{"单行无结尾换行", "hello", []string{"hello"}},
		{"单行带结尾换行", "hello\n", []string{"hello"}},
		{"多行带结尾换行", "a\nb\n", []string{"a", "b"}},
		{"多行无结尾换行", "a\nb", []string{"a", "b"}},
		{"保留中间空行", "a\n\nb\n", []string{"a", "", "b"}},
		{"CRLF 兼容", "a\r\nb\r\n", []string{"a", "b"}},
		{"行尾裸 CR", "a\rb\n", []string{"a\rb"}},
		{"纯换行", "\n", []string{""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SplitLines(c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("SplitLines(%q) = %#v, want %#v", c.in, got, c.want)
			}
		})
	}
}
