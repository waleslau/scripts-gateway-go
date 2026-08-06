package history

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	filePrefix          = "run-"   // 历史文件名前缀
	fileSuffix          = ".jsonl" // 历史文件后缀
	maxConsecutiveFails = 3        // 单个任务连续失败超过该次数后不再记录
	truncatedMarker     = "\n... [记录输出已截断]"
	defaultCleanupEvery = 6 * time.Hour
)

// Options 是创建历史存储的参数。
type Options struct {
	Dir            string // 存储目录（自动创建，权限 0750）
	MaxFileSize    int64  // 单个文件滚动阈值（字节）
	MaxFiles       int    // 保留文件数（0 = 不限制）
	RetentionDays  int    // 记录保留天数（0 = 不按时间清理）
	MaxOutputBytes int    // 每条记录持久化的 stdout/stderr 上限（字节）
}

// Store 是执行历史存储：JSONL 追加写 + 文件滚动 + 保留清理 + 查询。
// 并发安全：所有写操作经同一把锁串行化。
type Store struct {
	mu            sync.Mutex
	dir           string
	opts          Options
	cur           *os.File // 当前追加文件
	curName       string   // 当前追加文件名（清理时跳过）
	curSize       int64
	failCount     map[string]int // 每个任务的连续失败次数（仅内存，重启清零）
	now           func() time.Time
	cleanupEvery  time.Duration // 后台清理周期（测试可调）
	stopCh        chan struct{}
	doneCh        chan struct{}
	cleanupClosed bool
}

// New 创建历史存储：创建目录、恢复/打开当前追加文件、执行一次清理，并启动后台清理。
func New(opts Options) (*Store, error) {
	applyDefaults(&opts)
	absDir, err := filepath.Abs(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("解析历史目录失败: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o750); err != nil {
		return nil, fmt.Errorf("创建历史目录失败: %w", err)
	}
	s := &Store{
		dir:       absDir,
		opts:      opts,
		failCount: make(map[string]int),
		now:       time.Now,
		stopCh:    make(chan struct{}),
		doneCh:    make(chan struct{}),
	}
	if err := s.openCurrent(); err != nil {
		return nil, err
	}
	s.Cleanup()
	go s.cleanupLoop()
	return s, nil
}

func applyDefaults(opts *Options) {
	if opts.Dir == "" {
		opts.Dir = "./history"
	}
	if opts.MaxFileSize <= 0 {
		opts.MaxFileSize = 50 << 20
	}
	if opts.MaxFiles <= 0 {
		opts.MaxFiles = 25
	}
	if opts.RetentionDays < 0 {
		opts.RetentionDays = 0
	}
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = 64 << 10
	}
}

// Append 持久化一条执行记录，返回是否实际写入（失败且超过连续失败上限时跳过）。
// 记录策略见包注释；ID/Time 由本方法填充；stdout/stderr 按 MaxOutputBytes 截断。
func (s *Store) Append(rec *Record) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if rec.Failed() {
		if s.failCount[rec.Task] >= maxConsecutiveFails {
			return false, nil
		}
		s.failCount[rec.Task]++
	} else {
		delete(s.failCount, rec.Task)
	}

	rec.ID = s.newID()
	if rec.Time.IsZero() {
		rec.Time = s.now()
	}
	rec.Stdout = truncateStdout(rec.Stdout, s.opts.MaxOutputBytes)
	rec.Stderr = truncateOutput(rec.Stderr, s.opts.MaxOutputBytes)

	data, err := json.Marshal(rec)
	if err != nil {
		return false, fmt.Errorf("序列化执行记录失败: %w", err)
	}
	data = append(data, '\n')

	// 写前检查滚动：避免单条记录超过阈值时反复滚动（curSize==0 时不滚）。
	if s.curSize > 0 && s.curSize+int64(len(data)) > s.opts.MaxFileSize {
		if err := s.rotate(); err != nil {
			return false, err
		}
	}
	n, err := s.cur.Write(data)
	if err != nil {
		return false, fmt.Errorf("写入执行记录失败: %w", err)
	}
	s.curSize += int64(n)
	return true, nil
}

// Query 是历史查询参数。
type Query struct {
	Task          string    // 任务名精确匹配
	ExitCode      *int      // 退出码精确匹配（nil 不限；-1 表示系统级失败）
	From          time.Time // 起始时间（含）
	To            time.Time // 结束时间（含）
	Limit         int       // 每页条数（<=0 视为 20，上限 500）
	Offset        int       // 跳过条数
	IncludeOutput bool      // 是否返回 stdout/stderr（默认不含）
}

// List 按条件查询执行历史，按时间倒序返回，并给出过滤后的总数。
func (s *Store) List(q Query) ([]Record, int, error) {
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Limit > 500 {
		q.Limit = 500
	}

	s.mu.Lock()
	files, err := s.listFiles()
	s.mu.Unlock()
	if err != nil {
		return nil, 0, err
	}

	var matched []Record
	total := 0
	for i := len(files) - 1; i >= 0; i-- { // 新文件在前
		var fileMatches []Record
		err := s.scanFile(filepath.Join(s.dir, files[i]), func(r Record) bool {
			if matchRecord(r, q) {
				fileMatches = append(fileMatches, r)
			}
			return true
		})
		// 文件可能在扫描前被清理/清空删除：跳过，不视为错误。
		if err != nil && !os.IsNotExist(err) {
			return nil, 0, err
		}
		for j := len(fileMatches) - 1; j >= 0; j-- { // 文件内倒序，保证全局时间倒序
			r := fileMatches[j]
			total++
			if total > q.Offset && len(matched) < q.Limit {
				if !q.IncludeOutput {
					// stdout 为 any 类型：置 nil（而非空串）才能在 omitempty 下省略。
					r.Stdout, r.Stderr = nil, ""
				}
				matched = append(matched, r)
			}
		}
	}
	if matched == nil {
		matched = []Record{}
	}
	return matched, total, nil
}

// Get 按 ID 查询单条执行记录（含完整输出），不存在时返回 nil。
func (s *Store) Get(id string) (*Record, error) {
	s.mu.Lock()
	files, err := s.listFiles()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	for i := len(files) - 1; i >= 0; i-- {
		var found *Record
		err := s.scanFile(filepath.Join(s.dir, files[i]), func(r Record) bool {
			if r.ID == id {
				found = &r
				return false // 命中后停止扫描当前文件
			}
			return true
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if found != nil {
			return found, nil
		}
	}
	return nil, nil
}

// Clear 清空全部执行历史（关闭并删除所有历史文件，重新创建当前文件）。
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil {
		if err := s.cur.Close(); err != nil {
			return fmt.Errorf("关闭历史文件失败: %w", err)
		}
		s.cur = nil
	}
	files, err := s.listFiles()
	if err != nil {
		return err
	}
	for _, name := range files {
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil {
			return fmt.Errorf("删除历史文件失败: %w", err)
		}
	}
	return s.rotate()
}

// Cleanup 按保留策略清理历史文件：
//   - retention_days > 0 时删除超过保留天数的文件（按文件修改时间）；
//   - 文件数超过 max_files 时删除最旧的，直到数量达标。
func (s *Store) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := s.listFiles()
	if err != nil {
		return
	}
	cutoff := s.now().Add(-time.Duration(s.opts.RetentionDays) * 24 * time.Hour)
	keep := make([]string, 0, len(files))
	for _, name := range files {
		if name == s.curName {
			// 当前正在追加的文件不参与清理：删除后 Append 会写进已删除的 inode。
			keep = append(keep, name)
			continue
		}
		path := filepath.Join(s.dir, name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if s.opts.RetentionDays > 0 && info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
			continue
		}
		keep = append(keep, name)
	}
	if s.opts.MaxFiles > 0 && len(keep) > s.opts.MaxFiles {
		for _, name := range keep[:len(keep)-s.opts.MaxFiles] {
			_ = os.Remove(filepath.Join(s.dir, name))
		}
	}
}

// Dir 返回存储目录的绝对路径。
func (s *Store) Dir() string { return s.dir }

// Close 停止后台清理任务并关闭当前文件。
func (s *Store) Close() error {
	s.mu.Lock()
	if s.cur != nil {
		err := s.cur.Close()
		s.cur = nil
		if err != nil {
			s.mu.Unlock()
			return fmt.Errorf("关闭历史文件失败: %w", err)
		}
	}
	s.mu.Unlock()

	s.mu.Lock()
	if !s.cleanupClosed {
		s.cleanupClosed = true
		close(s.stopCh)
	}
	s.mu.Unlock()
	<-s.doneCh
	return nil
}

// --- 内部实现 ---

func (s *Store) cleanupLoop() {
	interval := s.cleanupEvery
	if interval <= 0 {
		interval = defaultCleanupEvery
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			close(s.doneCh)
			return
		case <-t.C:
			s.Cleanup()
		}
	}
}

// openCurrent 打开当前追加文件：优先复用最新的 run-*.jsonl（修复崩溃残留的
// 末尾半行），否则创建新文件。
func (s *Store) openCurrent() error {
	files, err := s.listFiles()
	if err != nil {
		return err
	}
	if len(files) > 0 {
		path := filepath.Join(s.dir, files[len(files)-1])
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("读取历史文件失败: %w", err)
		}
		if info.Size() > 0 {
			if err := truncatePartialTail(path); err != nil {
				return err
			}
		}
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o640)
		if err != nil {
			return fmt.Errorf("打开历史文件失败: %w", err)
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return fmt.Errorf("读取历史文件信息失败: %w", err)
		}
		s.cur = f
		s.curName = files[len(files)-1]
		s.curSize = fi.Size()
		return nil
	}
	return s.rotate()
}

// rotate 关闭当前文件并创建新的追加文件。
func (s *Store) rotate() error {
	if s.cur != nil {
		if err := s.cur.Close(); err != nil {
			return fmt.Errorf("关闭历史文件失败: %w", err)
		}
		s.cur = nil
	}
	name := fmt.Sprintf("%s%s-%s%s", filePrefix, s.now().Format("20060102T150405"), randHex(4), fileSuffix)
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("创建历史文件失败: %w", err)
	}
	s.cur = f
	s.curName = name
	s.curSize = 0
	return nil
}

// truncatePartialTail 将文件末尾不完整的 JSONL 行截断（文件以换行结尾视为完整）。
//
// 从文件末尾向前分块扫描，找到最后一个换行符后截断到其之后。
// 单个记录行可能超过 64KB（输出转义放大、参数较大），因此必须分块向前
// 扫描，而不是只看末尾固定大小——否则会把整个文件误截为 0。
func truncatePartialTail(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o640)
	if err != nil {
		return fmt.Errorf("打开历史文件失败: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	const chunk = 64 << 10
	buf := make([]byte, chunk)
	pos := size
	for pos > 0 {
		read := int64(len(buf))
		if pos < read {
			read = pos
		}
		n, err := f.ReadAt(buf[:read], pos-read)
		if err != nil && err != io.EOF {
			return err
		}
		for i := n - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				tail := pos - read + int64(i) + 1
				if tail == size {
					return nil // 末尾完整
				}
				return f.Truncate(tail)
			}
		}
		if n == 0 {
			break // 防御：理论上不会走到
		}
		pos -= read
	}
	// 整个文件没有任何换行：全部视为不完整残留，清空。
	if size == 0 {
		return nil
	}
	return f.Truncate(0)
}

// listFiles 列出目录中按文件名排序（即时间序，新的在后）的历史文件。
func (s *Store) listFiles() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("读取历史目录失败: %w", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, filePrefix) && strings.HasSuffix(name, fileSuffix) {
			files = append(files, name)
		}
	}
	sort.Strings(files)
	return files, nil
}

func (s *Store) newID() string {
	return fmt.Sprintf("%s-%s", s.now().Format("20060102T150405"), randHex(4))
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// truncateOutput 将持久化的输出限制在 max 字节内，超出部分截断并加标记。
func truncateOutput(str string, max int) string {
	if max <= 0 || len(str) <= max {
		return str
	}
	return str[:max] + truncatedMarker
}

// normalizeStdout 将 JSONL 反序列化后的 stdout 归一化为调用方写入时的形态：
// JSON 数组会解析为 []interface{}（每元素为 string），转为 []string，
// 保证读取方拿到的 lines 形态是 []string，与写入时一致。
func normalizeStdout(v any) any {
	arr, ok := v.([]any)
	if !ok {
		return v
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		} else {
			out = append(out, fmt.Sprint(e))
		}
	}
	return out
}

// truncateStdout 按 max 字节截断持久化的 stdout。stdout 的落盘形态由调用方
// 决定（string 或 []string）：
//   - string：直接按字节截断（见 truncateOutput）；
//   - []string：先把行按 \n 连接后按同一规则截断，再重新拆分为行数组
//     （截断标记成为最后一个元素，保持 lines 形态）。
func truncateStdout(v any, max int) any {
	switch t := v.(type) {
	case string:
		return truncateOutput(t, max)
	case []string:
		if max <= 0 {
			return t
		}
		joined := strings.Join(t, "\n")
		if len(joined) <= max {
			return t // 未超限，保持原数组
		}
		return SplitLines(truncateOutput(joined, max))
	default:
		return v
	}
}

// scanFile 逐行扫描一个历史文件，对每条合法记录调用 fn；
// fn 返回 false 时提前停止。损坏行跳过（理论上仅崩溃残留）。
// 返回的错误包含底层路径信息，调用方可借助 os.IsNotExist 判断文件被删除。
func (s *Store) scanFile(path string, fn func(Record) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("读取历史文件失败: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	// 单行上限 16MB：记录行含 params 与 JSON 转义后的输出，可能远大于输出上限。
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		r.Stdout = normalizeStdout(r.Stdout)
		if !fn(r) {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("读取历史文件失败: %w", err)
	}
	return nil
}

func matchRecord(r Record, q Query) bool {
	if q.Task != "" && r.Task != q.Task {
		return false
	}
	if q.ExitCode != nil && r.ExitCode != *q.ExitCode {
		return false
	}
	if !q.From.IsZero() && r.Time.Before(q.From) {
		return false
	}
	if !q.To.IsZero() && r.Time.After(q.To) {
		return false
	}
	return true
}
