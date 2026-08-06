package config

import (
	"strings"
	"testing"
)

func TestEnvKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"target", "TARGET"},
		{"targetDir", "TARGETDIR"},
		{"target-dir", "TARGET_DIR"},
		{"target.dir", "TARGET_DIR"},
		{"a1b2", "A1B2"},
		{"中文", "__"},
	}
	for _, c := range cases {
		if got := EnvKey(c.in); got != c.want {
			t.Errorf("EnvKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidateParamEnvCollision(t *testing.T) {
	// a-b 与 a_b 归一化后都是 PARAM_A_B，应被拒绝。
	_, err := Parse([]byte(`
mappings:
  - name: x
    script: x.sh
    params:
      - name: a-b
      - name: a_b
`))
	if err == nil || !strings.Contains(err.Error(), "PARAM_A_B") {
		t.Fatalf("应拒绝环境变量键冲突, err=%v", err)
	}

	// 仅大小写不同的参数同样冲突（env / Env -> PARAM_ENV）。
	_, err = Parse([]byte(`
mappings:
  - name: x
    script: x.sh
    params:
      - name: env
      - name: Env
`))
	if err == nil {
		t.Fatal("应拒绝大小写归一化后冲突的参数")
	}

	// 正常配置不受影响。
	cfg, err := Parse([]byte(`
mappings:
  - name: x
    script: x.sh
    params:
      - name: target-dir
      - name: env
`))
	if err != nil {
		t.Fatalf("正常配置不应被拒绝: %v", err)
	}
	if len(cfg.Mappings[0].Params) != 2 {
		t.Fatal("参数数量不符")
	}
}

func TestValidateMaxOutputBytesCap(t *testing.T) {
	_, err := Parse([]byte("history:\n  enabled: true\n  max_output_bytes: 2097152\n"))
	if err == nil || !strings.Contains(err.Error(), "max_output_bytes") {
		t.Fatalf("应拒绝超过 1MB 的 max_output_bytes, err=%v", err)
	}

	cfg, err := Parse([]byte("history:\n  enabled: true\n  max_output_bytes: 65536\n"))
	if err != nil {
		t.Fatalf("合法配置不应被拒绝: %v", err)
	}
	if cfg.History.MaxOutputBytes != 65536 {
		t.Fatal("配置解析不符")
	}
}

func TestValidateMaxConcurrent(t *testing.T) {
	if _, err := Parse([]byte("server:\n  max_concurrent: -1\n")); err == nil {
		t.Fatal("max_concurrent 为负应被拒绝")
	}
	cfg, err := Parse([]byte("server:\n  max_concurrent: 10\n"))
	if err != nil {
		t.Fatalf("合法配置不应被拒绝: %v", err)
	}
	if cfg.Server.MaxConcurrent != 10 {
		t.Fatal("max_concurrent 解析不符")
	}
}

func TestHistoryStdoutFormat(t *testing.T) {
	// 未配置时默认跟随 server.stdout_format。
	cfg, err := Parse([]byte("server:\n  stdout_format: \"lines\"\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.History.StdoutFormat != "lines" {
		t.Errorf("history.stdout_format 应默认跟随 server.stdout_format, got %q", cfg.History.StdoutFormat)
	}

	// 未配置 server 时默认 text。
	cfg, err = Parse([]byte("server:\n  addr: \":8080\"\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.History.StdoutFormat != "text" {
		t.Errorf("history.stdout_format 默认应为 text, got %q", cfg.History.StdoutFormat)
	}

	// 显式配置独立于 server.stdout_format。
	cfg, err = Parse([]byte("server:\n  stdout_format: \"lines\"\nhistory:\n  stdout_format: \"text\"\n"))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.Server.StdoutFormat != "lines" || cfg.History.StdoutFormat != "text" {
		t.Errorf("server/history stdout_format 应独立生效: server=%q history=%q",
			cfg.Server.StdoutFormat, cfg.History.StdoutFormat)
	}

	// 非法值被拒绝。
	_, err = Parse([]byte("history:\n  stdout_format: \"bad\"\n"))
	if err == nil || !strings.Contains(err.Error(), "history.stdout_format") {
		t.Fatalf("应拒绝非法 history.stdout_format, err=%v", err)
	}
}
