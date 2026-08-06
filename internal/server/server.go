// Package server 实现 RESTful HTTP 服务：
// 将请求路由到预定义的 接口-脚本 映射，并执行对应脚本。
package server

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"scripts-gateway-go/internal/config"
	"scripts-gateway-go/internal/executor"
	"scripts-gateway-go/internal/history"
)

const (
	maxBodyBytes   = 1 << 20 // JSON 请求体上限 1MB
	codeSuccess    = 0
	codeScriptFail = 1001 // 脚本执行完成但退出码非 0
)

// Response 是统一响应结构。
type Response struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Server 持有 HTTP 服务所需依赖。
// 配置与执行器均支持运行时热更新：配置经 Store 原子读取（每次请求取当前生效值），
// 执行器经 atomic 指针整体替换（scripts_dir/work_dir/allow_outside_scripts 变更时），
// 执行历史存储经 atomic 指针整体替换（history 配置变更时，nil 表示未启用）。
type Server struct {
	store   *config.Store
	exec    atomic.Pointer[executor.Executor]
	history atomic.Pointer[history.Store]
	logger  *slog.Logger
	handler http.Handler

	// 并发执行上限（0 = 不限）：达到上限时新请求直接返回 503。
	maxConcurrent atomic.Int64
	sem           atomic.Pointer[chan struct{}]
}

// New 构建路由并返回服务实例（实现 http.Handler）。
// historyStore 为 nil 时执行历史未启用（/api/v1/history 返回 404）。
func New(store *config.Store, ex *executor.Executor, logger *slog.Logger, historyStore *history.Store) *Server {
	s := &Server{store: store, logger: logger}
	s.exec.Store(ex)
	s.history.Store(historyStore)
	s.SetMaxConcurrent(store.Load().Server.MaxConcurrent)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /api/v1/tasks", s.handleListTasks)
	mux.HandleFunc("POST /api/v1/tasks/{name}", s.handleRunTask)
	mux.HandleFunc("GET /api/v1/tasks/{name}", s.handleRunTask)
	mux.HandleFunc("PUT /api/v1/tasks/{name}", s.handleRunTask)
	mux.HandleFunc("DELETE /api/v1/tasks/{name}", s.handleRunTask)
	mux.HandleFunc("GET /api/v1/history", s.handleListHistory)
	mux.HandleFunc("GET /api/v1/history/{id}", s.handleGetHistory)
	mux.HandleFunc("DELETE /api/v1/history", s.handleClearHistory)

	s.handler = s.withAuth(s.withLogging(mux))
	return s
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// SetExecutor 热更新执行器（scripts_dir / work_dir / allow_outside_scripts 变更时调用）。
func (s *Server) SetExecutor(ex *executor.Executor) {
	s.exec.Store(ex)
}

// SetHistoryStore 热更新执行历史存储（history 配置变更时调用；传 nil 表示停用）。
func (s *Server) SetHistoryStore(st *history.Store) {
	s.history.Store(st)
}

// SetMaxConcurrent 热更新并发执行上限（server.max_concurrent 变更时调用）。
// n <= 0 表示不限制。
func (s *Server) SetMaxConcurrent(n int) {
	s.maxConcurrent.Store(int64(n))
	if n > 0 {
		ch := make(chan struct{}, n)
		s.sem.Store(&ch)
	} else {
		s.sem.Store(nil)
	}
}

// handleHealthz 健康检查。
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, Response{Code: codeSuccess, Message: "ok"})
}

// handleListTasks 列出所有任务及其元信息。
func (s *Server) handleListTasks(w http.ResponseWriter, _ *http.Request) {
	cfg := s.store.Load()
	type taskInfo struct {
		Name           string   `json:"name"`
		Method         string   `json:"method"`
		Script         string   `json:"script"`
		TimeoutSeconds int      `json:"timeout_seconds"`
		Params         []string `json:"params"`
	}
	tasks := make([]taskInfo, 0, len(cfg.Mappings))
	for _, m := range cfg.Mappings {
		params := make([]string, 0, len(m.Params))
		for _, p := range m.Params {
			desc := fmt.Sprintf("%s(required=%v", p.Name, p.Required)
			if p.Default != "" {
				desc += fmt.Sprintf(", default=%s", p.Default)
			}
			if p.Description != "" {
				desc += fmt.Sprintf(", %s", p.Description)
			}
			params = append(params, desc+")")
		}
		tasks = append(tasks, taskInfo{
			Name:           m.Name,
			Method:         m.Method,
			Script:         m.Script,
			TimeoutSeconds: m.TimeoutSeconds,
			Params:         params,
		})
	}
	writeJSON(w, http.StatusOK, Response{Code: codeSuccess, Message: "success", Data: tasks})
}

// handleRunTask 执行任务：查映射 -> 收集参数 -> 校验 -> 执行脚本 -> 返回结果。
func (s *Server) handleRunTask(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Load()
	ex := s.exec.Load()
	name := r.PathValue("name")
	m := cfg.FindMapping(name)
	if m == nil {
		writeError(w, http.StatusNotFound, http.StatusNotFound, fmt.Sprintf("任务 %q 不存在", name))
		return
	}
	if !strings.EqualFold(r.Method, m.Method) {
		w.Header().Set("Allow", m.Method)
		writeError(w, http.StatusMethodNotAllowed, http.StatusMethodNotAllowed,
			fmt.Sprintf("任务 %q 仅支持 %s 方法", name, m.Method))
		return
	}

	provided, err := s.collectParams(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, http.StatusBadRequest, err.Error())
		return
	}

	// 合并默认值并校验必填参数。
	values := make(map[string]string, len(m.Params))
	for _, p := range m.Params {
		v, ok := provided[p.Name]
		if !ok {
			v = p.Default
		}
		if p.Required && v == "" {
			writeError(w, http.StatusBadRequest, http.StatusBadRequest,
				fmt.Sprintf("缺少必填参数: %s", p.Name))
			return
		}
		values[p.Name] = v
	}

	scriptAbs, err := ex.ResolveScript(m.Script)
	if err != nil {
		writeError(w, http.StatusBadRequest, http.StatusBadRequest, err.Error())
		return
	}

	// 位置参数按配置顺序；环境变量以 PARAM_<NAME> 注入。
	args := make([]string, 0, len(m.Params))
	env := make(map[string]string, len(m.Params))
	for _, p := range m.Params {
		args = append(args, values[p.Name])
		env["PARAM_"+config.EnvKey(p.Name)] = values[p.Name]
	}

	// 并发上限：占满时快速失败返回 503（不占用执行槽，也不进入执行历史）。
	slot, ok := s.acquireSlot()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, http.StatusServiceUnavailable,
			"服务器繁忙：并发任务数已达上限，请稍后重试")
		return
	}
	defer s.releaseSlot(slot)

	timeout := time.Duration(m.TimeoutSeconds) * time.Second
	res, err := ex.Execute(r.Context(), scriptAbs, args, env, timeout)
	if err != nil {
		s.recordRun(r, m, values, res, http.StatusInternalServerError, err.Error())
		s.logger.Error("任务执行失败", "task", name, "script", m.Script, "error", err)
		writeError(w, http.StatusInternalServerError, http.StatusInternalServerError, err.Error())
		return
	}

	s.logger.Info("任务执行完成", "task", name, "script", m.Script,
		"exit_code", res.ExitCode, "duration_ms", res.DurationMS, "timed_out", res.TimedOut)

	if res.ExitCode != 0 {
		s.recordRun(r, m, values, res, http.StatusOK, "")
		writeJSON(w, http.StatusOK, Response{
			Code:    codeScriptFail,
			Message: fmt.Sprintf("脚本执行失败（退出码 %d）", res.ExitCode),
			Data:    s.formatResult(res, cfg),
		})
		return
	}
	s.recordRun(r, m, values, res, http.StatusOK, "")
	writeJSON(w, http.StatusOK, Response{Code: codeSuccess, Message: "success", Data: s.formatResult(res, cfg)})
}

// acquireSlot 尝试获取一个并发执行槽。未启用上限或槽位可用时返回 (nil, true)；
// 达到上限返回 (nil, false)。返回的通道必须原样传给 releaseSlot：
// 热更新重建信号量后，旧请求仍要释放回自己获取时的那个信号量，
// 否则会在新信号量上无限等待。
func (s *Server) acquireSlot() (chan struct{}, bool) {
	if s.maxConcurrent.Load() <= 0 {
		return nil, true
	}
	ch := s.sem.Load()
	if ch == nil {
		return nil, true // 上限刚被关闭，尚未重建信号量
	}
	select {
	case *ch <- struct{}{}:
		return *ch, true
	default:
		return nil, false
	}
}

func (s *Server) releaseSlot(ch chan struct{}) {
	if ch != nil {
		<-ch
	}
}

// historyStdoutFormat 返回历史 stdout 形态：优先取 history.stdout_format，
// 未配置（空值）时跟随 server.stdout_format。两者相互独立：
// history.stdout_format 控制历史落盘 JSONL 与历史查询接口；
// server.stdout_format 只控制任务执行响应。
func historyStdoutFormat(cfg *config.Config) string {
	if cfg.History.StdoutFormat != "" {
		return cfg.History.StdoutFormat
	}
	return cfg.Server.StdoutFormat
}

// recordRun 将一次脚本执行写入执行历史（未启用时不记录）。
// 系统级错误（超时/启动失败）以 ExitCode=-1 + Error 落盘，并计入该任务连续失败次数。
// Stdout 的落盘形态由 history.stdout_format 决定（未配置时跟随 server.stdout_format）：
// lines 时拆为行数组，text 时为原始字符串。
func (s *Server) recordRun(r *http.Request, m *config.Mapping, values map[string]string,
	res executor.Result, httpStatus int, errMsg string) {
	st := s.history.Load()
	if st == nil {
		return
	}
	cfg := s.store.Load()
	rec := history.Record{
		Task:       m.Name,
		Method:     m.Method,
		Script:     m.Script,
		Params:     values,
		RemoteAddr: r.RemoteAddr,
		ExitCode:   res.ExitCode,
		DurationMS: res.DurationMS,
		TimedOut:   res.TimedOut,
		HTTPStatus: httpStatus,
		Stderr:     res.Stderr,
		Error:      errMsg,
	}
	if historyStdoutFormat(cfg) == "lines" {
		rec.Stdout = history.SplitLines(res.Stdout)
	} else {
		rec.Stdout = res.Stdout
	}
	if rec.Error != "" && rec.ExitCode == 0 {
		rec.ExitCode = -1 // 系统级失败：脚本未正常完成
	}
	written, err := st.Append(&rec)
	if err != nil {
		s.logger.Error("写入执行历史失败", "task", m.Name, "error", err)
		return
	}
	if written {
		s.logger.Debug("执行历史已记录", "task", m.Name, "id", rec.ID)
	}
}

// taskResult 是任务执行结果在响应体中的形态：
// stdout_format=lines 时 stdout 为行数组，否则为原始字符串。
type taskResult struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     any    `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMS int64  `json:"duration_ms"`
	TimedOut   bool   `json:"timed_out"`
}

// formatResult 按配置将执行结果转为响应数据：stdout_format=lines 时
// 把 stdout 按行拆分为数组，其余字段原样返回；默认（text）直接返回原结果。
func (s *Server) formatResult(res executor.Result, cfg *config.Config) any {
	if cfg.Server.StdoutFormat != "lines" {
		return res
	}
	return taskResult{
		ExitCode:   res.ExitCode,
		Stdout:     history.SplitLines(res.Stdout),
		Stderr:     res.Stderr,
		DurationMS: res.DurationMS,
		TimedOut:   res.TimedOut,
	}
}

// historyRecordResult 是执行历史记录在响应体中的形态：
// stdout_format=lines 时 stdout 为行数组，否则为原始字符串。
// 字段与 history.Record 保持一致，仅 Stdout 类型不同（string -> any）。
type historyRecordResult struct {
	ID         string            `json:"id"`
	Time       time.Time         `json:"time"`
	Task       string            `json:"task"`
	Method     string            `json:"method"`
	Script     string            `json:"script"`
	Params     map[string]string `json:"params,omitempty"`
	RemoteAddr string            `json:"remote_addr"`
	ExitCode   int               `json:"exit_code"`
	DurationMS int64             `json:"duration_ms"`
	TimedOut   bool              `json:"timed_out"`
	HTTPStatus int               `json:"http_status"`
	Stdout     any               `json:"stdout,omitempty"`
	Stderr     string            `json:"stderr,omitempty"`
	Error      string            `json:"error,omitempty"`
}

// formatHistoryRecord 按历史配置将记录转为响应数据：history.stdout_format=lines 时
// 保证 stdout 为行数组，默认（text）直接返回原记录。
// 新写入的记录落盘时已按配置转换（见 recordRun），此处仅对历史遗留的
// string 形态 stdout 做兼容拆分（如旧配置下落盘的记录）。
func (s *Server) formatHistoryRecord(rec *history.Record, cfg *config.Config) any {
	if historyStdoutFormat(cfg) != "lines" {
		return rec
	}
	out := historyRecordResult{
		ID:         rec.ID,
		Time:       rec.Time,
		Task:       rec.Task,
		Method:     rec.Method,
		Script:     rec.Script,
		Params:     rec.Params,
		RemoteAddr: rec.RemoteAddr,
		ExitCode:   rec.ExitCode,
		DurationMS: rec.DurationMS,
		TimedOut:   rec.TimedOut,
		HTTPStatus: rec.HTTPStatus,
		Stderr:     rec.Stderr,
		Error:      rec.Error,
	}
	switch v := rec.Stdout.(type) {
	case string:
		out.Stdout = history.SplitLines(v)
	default:
		out.Stdout = rec.Stdout // []string 或 nil 原样返回
	}
	return out
}

// collectParams 从 Query String 与请求体收集参数，请求体优先于 Query String。
// 请求体支持 JSON 对象与表单（application/x-www-form-urlencoded）两种格式：
//   - 请求体以 { 开头时一律按 JSON 解析——即使未携带 Content-Type 或携带的是
//     curl 默认的 form 类型（curl -d 未加 -H "Content-Type: application/json"），
//     也能正确读取参数；
//   - 否则按 Content-Type 决定：form 走表单解析，未知类型宽松尝试表单解析。
func (s *Server) collectParams(w http.ResponseWriter, r *http.Request) (map[string]string, error) {
	params := make(map[string]string)

	for k, vs := range r.URL.Query() {
		if len(vs) > 0 {
			params[k] = vs[len(vs)-1]
		}
	}

	if r.Body == nil {
		return params, nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("读取请求体失败: %v", err)
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return params, nil
	}

	ct := r.Header.Get("Content-Type")
	isJSON := strings.HasPrefix(ct, "application/json")
	isForm := strings.HasPrefix(ct, "application/x-www-form-urlencoded")

	switch {
	case trimmed[0] == '{' || isJSON:
		// JSON 对象：请求体以 { 开头即按 JSON 解析，兼容未指定 Content-Type
		// 或默认 form 类型但实际发送 JSON 的调用（如 curl -d 未加 -H）。
		obj := make(map[string]any)
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		dec.UseNumber() // 数字保持原文，避免 float64 丢失大整数精度
		if err := dec.Decode(&obj); err != nil {
			return nil, fmt.Errorf("请求体不是合法 JSON 对象: %v", err)
		}
		for k, v := range obj {
			params[k] = stringify(v)
		}
	case isForm:
		vals, err := url.ParseQuery(string(trimmed))
		if err != nil {
			return nil, fmt.Errorf("请求体不是合法表单: %v", err)
		}
		for k, vs := range vals {
			if len(vs) > 0 {
				params[k] = vs[len(vs)-1]
			}
		}
	default:
		// 未知 Content-Type 且请求体不是 JSON：宽松尝试表单解析，失败则忽略。
		if vals, err := url.ParseQuery(string(trimmed)); err == nil {
			for k, vs := range vals {
				if len(vs) > 0 {
					params[k] = vs[len(vs)-1]
				}
			}
		}
	}
	return params, nil
}

// stringify 将 JSON 值转为字符串：标量直接转，嵌套结构序列化为 JSON。
func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	case float64, bool:
		return fmt.Sprint(t)
	case json.Number:
		return t.String()
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}

// --- 执行历史接口 ---

// handleListHistory 查询执行历史：分页 + 过滤（task/exit_code/from/to），
// 默认不含 stdout/stderr 输出（详情见 GET /api/v1/history/{id}）。
func (s *Server) handleListHistory(w http.ResponseWriter, r *http.Request) {
	st := s.history.Load()
	if st == nil {
		writeError(w, http.StatusNotFound, http.StatusNotFound, "执行历史未启用（history.enabled=false）")
		return
	}
	q, err := parseHistoryQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, http.StatusBadRequest, err.Error())
		return
	}
	items, total, err := st.List(q)
	if err != nil {
		s.logger.Error("查询执行历史失败", "error", err)
		writeError(w, http.StatusInternalServerError, http.StatusInternalServerError, "查询执行历史失败")
		return
	}
	writeJSON(w, http.StatusOK, Response{
		Code:    codeSuccess,
		Message: "success",
		Data:    map[string]any{"total": total, "items": items},
	})
}

// handleGetHistory 查询单条执行记录详情（含完整 stdout/stderr）。
func (s *Server) handleGetHistory(w http.ResponseWriter, r *http.Request) {
	st := s.history.Load()
	if st == nil {
		writeError(w, http.StatusNotFound, http.StatusNotFound, "执行历史未启用（history.enabled=false）")
		return
	}
	id := r.PathValue("id")
	rec, err := st.Get(id)
	if err != nil {
		s.logger.Error("查询执行历史失败", "error", err)
		writeError(w, http.StatusInternalServerError, http.StatusInternalServerError, "查询执行历史失败")
		return
	}
	if rec == nil {
		writeError(w, http.StatusNotFound, http.StatusNotFound, fmt.Sprintf("执行记录 %q 不存在", id))
		return
	}
	// stdout 形态遵循 server.stdout_format（lines 时为行数组），与任务执行响应一致。
	writeJSON(w, http.StatusOK, Response{Code: codeSuccess, Message: "success", Data: s.formatHistoryRecord(rec, s.store.Load())})
}

// handleClearHistory 清空全部执行历史。
func (s *Server) handleClearHistory(w http.ResponseWriter, _ *http.Request) {
	st := s.history.Load()
	if st == nil {
		writeError(w, http.StatusNotFound, http.StatusNotFound, "执行历史未启用（history.enabled=false）")
		return
	}
	if err := st.Clear(); err != nil {
		s.logger.Error("清空执行历史失败", "error", err)
		writeError(w, http.StatusInternalServerError, http.StatusInternalServerError, "清空执行历史失败")
		return
	}
	writeJSON(w, http.StatusOK, Response{Code: codeSuccess, Message: "success"})
}

// parseHistoryQuery 解析执行历史查询参数：
// task / exit_code / from / to（RFC3339）/ limit（默认 20，上限 500）/ offset。
func parseHistoryQuery(r *http.Request) (history.Query, error) {
	q := history.Query{Limit: 20}
	vals := r.URL.Query()
	q.Task = vals.Get("task")
	if v := vals.Get("exit_code"); v != "" {
		code, err := strconv.Atoi(v)
		if err != nil {
			return q, fmt.Errorf("exit_code 必须是整数: %q", v)
		}
		q.ExitCode = &code
	}
	parseTime := func(name string) (time.Time, error) {
		v := vals.Get(name)
		if v == "" {
			return time.Time{}, nil
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return time.Time{}, fmt.Errorf("%s 必须是 RFC3339 时间: %q", name, v)
		}
		return t, nil
	}
	var err error
	if q.From, err = parseTime("from"); err != nil {
		return q, err
	}
	if q.To, err = parseTime("to"); err != nil {
		return q, err
	}
	if v := vals.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return q, fmt.Errorf("limit 必须是正整数: %q", v)
		}
		q.Limit = n
	}
	if q.Limit > 500 {
		q.Limit = 500
	}
	if v := vals.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return q, fmt.Errorf("offset 必须是非负整数: %q", v)
		}
		q.Offset = n
	}
	return q, nil
}

// --- 中间件与响应工具 ---

// withAuth 可选鉴权：配置了 auth_token 时要求请求携带相同令牌。
// 令牌每次请求从当前配置读取，修改 auth_token 无需重启即可生效。
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 健康检查免鉴权：负载均衡/探活通常无法携带令牌，且该接口不泄露任何信息。
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		token := s.store.Load().Server.AuthToken
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" {
			got = r.Header.Get("X-Auth-Token")
		}
		// 常量时间比较，避免通过响应时间差猜测令牌。
		if !constantTimeEqual(got, token) {
			writeError(w, http.StatusUnauthorized, http.StatusUnauthorized,
				"未授权：缺少或错误的访问令牌")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// constantTimeEqual 以常量时间方式比较两个字符串（长度不同时直接返回 false，
// 长度本身不敏感，不构成有效侧信道）。
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// statusRecorder 记录响应状态码，供访问日志使用。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// withLogging 输出结构化访问日志。
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.logger.Info("访问",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
	})
}

// writeJSON 输出统一 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, resp Response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// writeError 输出统一错误响应（HTTP 状态码与业务码一致）。
func writeError(w http.ResponseWriter, status int, code int, msg string) {
	writeJSON(w, status, Response{Code: code, Message: msg})
}
