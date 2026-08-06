// Package config 负责加载并校验 "接口-脚本映射" 配置。
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Param 定义单个脚本参数。
type Param struct {
	Name        string `yaml:"name"`
	Required    bool   `yaml:"required"`
	Default     string `yaml:"default"`
	Description string `yaml:"description"`
}

// Mapping 定义一条 接口 -> 脚本 的映射关系。
type Mapping struct {
	Name           string  `yaml:"name"`
	Method         string  `yaml:"method"`
	Script         string  `yaml:"script"`
	TimeoutSeconds int     `yaml:"timeout_seconds"`
	Params         []Param `yaml:"params"`
}

// ServerConfig 定义 HTTP 服务配置。
type ServerConfig struct {
	Addr          string `yaml:"addr"`
	AuthToken     string `yaml:"auth_token"`
	StdoutFormat  string `yaml:"stdout_format"`  // text=原样字符串（默认）；lines=按行拆分为数组
	MaxConcurrent int    `yaml:"max_concurrent"` // 并发执行上限（0=不限制，达到上限返回 503）
}

// HistoryConfig 定义执行历史与日志持久化配置。
type HistoryConfig struct {
	Enabled        bool   `yaml:"enabled"`          // 总开关，默认 false
	Dir            string `yaml:"dir"`              // 历史存储目录（自动创建，权限 0750）
	MaxFileSizeMB  int    `yaml:"max_file_size_mb"` // 单个文件滚动阈值（MB）
	MaxFiles       int    `yaml:"max_files"`        // 保留文件数兜底（超出删除最旧的）
	RetentionDays  *int   `yaml:"retention_days"`   // 记录保留天数；nil=默认 60，0=不按时间清理
	MaxOutputBytes int    `yaml:"max_output_bytes"` // 每条记录持久化的 stdout/stderr 上限（字节）
	// StdoutFormat 历史落盘与查询接口中 stdout 的形态：text=原样字符串（默认）；
	// lines=按行拆分为数组。空值（未配置）时跟随 server.stdout_format。
	// 与 server.stdout_format 相互独立：后者只影响任务执行响应。
	StdoutFormat string `yaml:"stdout_format"`
}

// RetentionDaysValue 返回保留天数（nil 视为默认 60 天）。
func (h HistoryConfig) RetentionDaysValue() int {
	if h.RetentionDays == nil {
		return 60
	}
	return *h.RetentionDays
}

// Equal 比较历史配置是否变化（供热更新检测；RetentionDays 为指针无法直接比较）。
// StdoutFormat 不参与比较：stdout 形态转换发生在 server 层（每次记录/查询读取
// 当前配置），存储本身不依赖该字段，变更时无需重建 Store。
func (h HistoryConfig) Equal(o HistoryConfig) bool {
	return h.Enabled == o.Enabled &&
		h.Dir == o.Dir &&
		h.MaxFileSizeMB == o.MaxFileSizeMB &&
		h.MaxFiles == o.MaxFiles &&
		h.RetentionDaysValue() == o.RetentionDaysValue() &&
		h.MaxOutputBytes == o.MaxOutputBytes
}

// Config 是整体配置结构。
type Config struct {
	Server              ServerConfig  `yaml:"server"`
	History             HistoryConfig `yaml:"history"`
	ScriptsDir          string        `yaml:"scripts_dir"`
	WorkDir             string        `yaml:"work_dir"`
	AllowOutsideScripts bool          `yaml:"allow_outside_scripts"` // 允许执行脚本目录之外的脚本（默认 false）
	Mappings            []Mapping     `yaml:"mappings"`
}

// Load 从 YAML 文件加载配置，应用默认值并校验。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	return Parse(data)
}

// Parse 解析 YAML 配置内容，应用默认值并校验。
// 供配置热更新（Watcher）复用：解析逻辑与启动加载保持一致。
func Parse(data []byte) (*Config, error) {
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}
	applyDefaults(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyDefaults 填充未配置字段的默认值。
func applyDefaults(cfg *Config) {
	if cfg.Server.Addr == "" {
		cfg.Server.Addr = ":8080"
	}
	if cfg.Server.StdoutFormat == "" {
		cfg.Server.StdoutFormat = "text"
	}
	if cfg.History.RetentionDays == nil {
		d := 60
		cfg.History.RetentionDays = &d
	}
	if cfg.History.Dir == "" {
		cfg.History.Dir = "./history"
	}
	if cfg.History.StdoutFormat == "" {
		cfg.History.StdoutFormat = cfg.Server.StdoutFormat // 默认跟随 server.stdout_format
	}
	if cfg.History.MaxFileSizeMB <= 0 {
		cfg.History.MaxFileSizeMB = 50
	}
	if cfg.History.MaxFiles <= 0 {
		cfg.History.MaxFiles = 25
	}
	if cfg.History.MaxOutputBytes <= 0 {
		cfg.History.MaxOutputBytes = 64 << 10 // 64KB
	}
	if cfg.ScriptsDir == "" {
		cfg.ScriptsDir = "./scripts"
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = "."
	}
	for i := range cfg.Mappings {
		m := &cfg.Mappings[i]
		if m.Method == "" {
			m.Method = "POST"
		}
		if m.TimeoutSeconds <= 0 {
			m.TimeoutSeconds = 30
		}
	}
}

// Validate 校验配置合法性，非法配置应拒绝启动。
func (c *Config) Validate() error {
	switch c.Server.StdoutFormat {
	case "text", "lines":
	default:
		return fmt.Errorf("server.stdout_format 不支持: %s（可选 text/lines）", c.Server.StdoutFormat)
	}
	switch c.History.StdoutFormat {
	case "text", "lines":
	default:
		return fmt.Errorf("history.stdout_format 不支持: %s（可选 text/lines）", c.History.StdoutFormat)
	}
	if c.Server.MaxConcurrent < 0 {
		return fmt.Errorf("server.max_concurrent 不能为负（0=不限制）")
	}

	if c.History.Enabled {
		if c.History.MaxFileSizeMB <= 0 {
			return fmt.Errorf("history.max_file_size_mb 必须为正整数")
		}
		if c.History.MaxFiles <= 0 {
			return fmt.Errorf("history.max_files 必须为正整数")
		}
		if c.History.RetentionDaysValue() < 0 {
			return fmt.Errorf("history.retention_days 不能为负（0=不按时间清理）")
		}
		if c.History.MaxOutputBytes <= 0 {
			return fmt.Errorf("history.max_output_bytes 必须为正整数")
		}
		if c.History.MaxOutputBytes > 1<<20 {
			// 执行器对单次输出上限即为 1MB，历史截断阈值更大没有意义；
			// 且过大的单行记录会接近读取端的扫描行上限。
			return fmt.Errorf("history.max_output_bytes 不能超过 1MB（与脚本输出上限一致）")
		}
	}

	seen := make(map[string]bool)
	for _, m := range c.Mappings {
		if m.Name == "" {
			return fmt.Errorf("mapping 的 name 不能为空")
		}
		if seen[m.Name] {
			return fmt.Errorf("mapping name 重复: %s", m.Name)
		}
		seen[m.Name] = true

		if m.Script == "" {
			return fmt.Errorf("mapping %q 的 script 不能为空", m.Name)
		}
		switch strings.ToUpper(m.Method) {
		case "GET", "POST", "PUT", "DELETE":
		default:
			return fmt.Errorf("mapping %q 的 method 不支持: %s", m.Name, m.Method)
		}

		paramSeen := make(map[string]bool)
		for _, p := range m.Params {
			if p.Name == "" {
				return fmt.Errorf("mapping %q 存在未命名的参数", m.Name)
			}
			key := EnvKey(p.Name)
			if paramSeen[key] {
				return fmt.Errorf("mapping %q 参数 %q 与已有参数在环境变量键 PARAM_%s 上冲突", m.Name, p.Name, key)
			}
			paramSeen[key] = true
		}
	}
	return nil
}

// EnvKey 将参数名转换为环境变量后缀：小写转大写，其他字符转下划线。
// 如 "targetDir" -> "TARGETDIR"，"target-dir" -> "TARGET_DIR"。
// 供参数校验（避免不同参数映射到同一 PARAM_<NAME>）与 server 注入环境变量使用。
func EnvKey(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - ('a' - 'A'))
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// FindMapping 按任务名查找映射，不存在时返回 nil。
func (c *Config) FindMapping(name string) *Mapping {
	for i := range c.Mappings {
		if c.Mappings[i].Name == name {
			return &c.Mappings[i]
		}
	}
	return nil
}
