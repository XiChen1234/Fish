package config

import (
	"os"
	"path/filepath"
	"testing"
)

// clearEnv 清空 FISH_* 环境变量，避免开发机上的设置影响断言。
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvListen, EnvDataDir, EnvMaxTextBytes, EnvMaxFileBytes, EnvConfigPath} {
		t.Setenv(k, "")
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("写入测试配置失败: %v", err)
	}
	return path
}

// 没有任何配置文件时也必须能启动，且全部取默认值。
func TestLoadWithoutFile(t *testing.T) {
	clearEnv(t)

	cfg, err := Load(filepath.Join(t.TempDir(), "not-exist.json"))
	if err != nil {
		t.Fatalf("配置文件缺失不应报错: %v", err)
	}
	if cfg.Listen != DefaultListen || cfg.DataDir != DefaultDataDir {
		t.Fatalf("应使用默认地址/目录, got listen=%q data_dir=%q", cfg.Listen, cfg.DataDir)
	}
	if cfg.MaxTextBytes != DefaultMaxTextBytes || cfg.MaxFileBytes != DefaultMaxFileBytes {
		t.Fatalf("应使用默认大小上限, got %d/%d", cfg.MaxTextBytes, cfg.MaxFileBytes)
	}
}

// 空路径表示完全不读文件。
func TestLoadEmptyPath(t *testing.T) {
	clearEnv(t)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("空路径不应报错: %v", err)
	}
	if cfg.Listen != DefaultListen {
		t.Fatalf("应使用默认监听地址, got %q", cfg.Listen)
	}
}

// 部分字段的配置文件：未写的字段保留默认值。
func TestLoadPartialFileKeepsDefaults(t *testing.T) {
	clearEnv(t)

	cfg, err := Load(writeConfig(t, `{"listen": ":19090"}`))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Listen != ":19090" {
		t.Fatalf("listen 应来自配置文件, got %q", cfg.Listen)
	}
	if cfg.DataDir != DefaultDataDir || cfg.MaxFileBytes != DefaultMaxFileBytes {
		t.Fatalf("未配置字段应保留默认值, got data_dir=%q max_file=%d", cfg.DataDir, cfg.MaxFileBytes)
	}
}

// 环境变量优先于配置文件。
func TestEnvOverridesFile(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvListen, ":9999")
	t.Setenv(EnvMaxFileBytes, "2048")

	cfg, err := Load(writeConfig(t, `{"listen": ":19090", "max_file_bytes": 4096}`))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Listen != ":9999" {
		t.Fatalf("环境变量应覆盖文件, got %q", cfg.Listen)
	}
	if cfg.MaxFileBytes != 2048 {
		t.Fatalf("环境变量应覆盖文件, got %d", cfg.MaxFileBytes)
	}
	if cfg.MaxTextBytes != DefaultMaxTextBytes {
		t.Fatalf("未设置的环境变量不应改动默认值, got %d", cfg.MaxTextBytes)
	}
}

// 环境变量数值非法时应直接报错，而不是静默回退。
func TestInvalidEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvMaxTextBytes, "abc")

	if _, err := Load(""); err == nil {
		t.Fatal("非法环境变量应报错")
	}
}

// 兼容旧的 config.json：带 endpoints 字段（已弃用）也能正常解析；
// 带 UTF-8 BOM 也能解析。
func TestLoadLegacyFileWithBOM(t *testing.T) {
	clearEnv(t)

	body := "\xEF\xBB\xBF" + `{"listen": ":18080", "endpoints": [{"id": "a", "token": "x"}]}`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("旧配置应能解析: %v", err)
	}
	if cfg.Listen != ":18080" {
		t.Fatalf("BOM 容忍后应读到 listen, got %q", cfg.Listen)
	}
}

// 显式写 0 / 空串时回退默认值。
func TestNormalizeZeroValues(t *testing.T) {
	clearEnv(t)

	cfg, err := Load(writeConfig(t, `{"listen": "", "data_dir": "", "max_text_bytes": 0, "max_file_bytes": 0}`))
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if cfg.Listen != DefaultListen || cfg.DataDir != DefaultDataDir {
		t.Fatalf("空值应回退默认值, got %q/%q", cfg.Listen, cfg.DataDir)
	}
	if cfg.MaxTextBytes != DefaultMaxTextBytes || cfg.MaxFileBytes != DefaultMaxFileBytes {
		t.Fatalf("0 值应回退默认值, got %d/%d", cfg.MaxTextBytes, cfg.MaxFileBytes)
	}
}

// FISH_CONFIG 提供默认配置文件路径。
func TestConfigPathEnv(t *testing.T) {
	clearEnv(t)
	if got := ConfigPath("config.json"); got != "config.json" {
		t.Fatalf("未设置 FISH_CONFIG 时应返回默认值, got %q", got)
	}
	t.Setenv(EnvConfigPath, "/etc/fish/config.json")
	if got := ConfigPath("config.json"); got != "/etc/fish/config.json" {
		t.Fatalf("应返回 FISH_CONFIG, got %q", got)
	}
}
