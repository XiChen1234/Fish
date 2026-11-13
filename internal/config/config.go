package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

// Config 服务端运行参数。
// 每个字段都有默认值，可通过配置文件或环境变量覆盖；
// 配置文件是可选的，服务不依赖 config.json 即可启动。
//
// 优先级（低 → 高）：默认值 < 配置文件 < 环境变量 < 命令行参数。
type Config struct {
	Listen       string `json:"listen"`         // 监听地址，如 ":8080"
	DataDir      string `json:"data_dir"`       // 数据目录（SQLite + 图片文件 + jwt.key）
	MaxTextBytes int    `json:"max_text_bytes"` // 文本消息上限（字节）
	MaxFileBytes int64  `json:"max_file_bytes"` // 图片上传上限（字节）
}

const (
	DefaultListen       = ":8080"
	DefaultDataDir      = "./data"
	DefaultMaxTextBytes = 8 * 1024
	DefaultMaxFileBytes = 10 * 1024 * 1024
)

// 环境变量名，统一 FISH_ 前缀，便于 systemd / 容器注入。
const (
	EnvListen       = "FISH_LISTEN"
	EnvDataDir      = "FISH_DATA_DIR"
	EnvMaxTextBytes = "FISH_MAX_TEXT_BYTES"
	EnvMaxFileBytes = "FISH_MAX_FILE_BYTES"
	EnvConfigPath   = "FISH_CONFIG"
)

// Default 返回零配置启动所需的默认参数。
func Default() *Config {
	return &Config{
		Listen:       DefaultListen,
		DataDir:      DefaultDataDir,
		MaxTextBytes: DefaultMaxTextBytes,
		MaxFileBytes: DefaultMaxFileBytes,
	}
}

// Load 组装配置：默认值 < path 指向的 JSON 文件（可选）< 环境变量。
// path 为空或文件不存在时不报错，直接返回默认值；
// 文件只写部分字段时，其余字段保持默认值。
func Load(path string) (*Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			// 容忍 UTF-8 BOM（Windows 记事本/PowerShell 写配置常见）
			data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
			if uerr := json.Unmarshal(data, cfg); uerr != nil {
				return nil, fmt.Errorf("解析配置 %s 失败: %w", path, uerr)
			}
		case os.IsNotExist(err):
			// 配置文件可选：没有文件即零配置启动
		default:
			return nil, fmt.Errorf("读取配置 %s 失败: %w", path, err)
		}
	}

	if err := applyEnv(cfg); err != nil {
		return nil, err
	}
	cfg.normalize()
	return cfg, nil
}

// ConfigPath 返回 FISH_CONFIG 指定的配置文件路径，未设置时返回 def。
func ConfigPath(def string) string {
	if v := os.Getenv(EnvConfigPath); v != "" {
		return v
	}
	return def
}

// applyEnv 用环境变量覆盖配置；数值非法时直接报错，避免静默使用错误上限。
func applyEnv(c *Config) error {
	if v := os.Getenv(EnvListen); v != "" {
		c.Listen = v
	}
	if v := os.Getenv(EnvDataDir); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv(EnvMaxTextBytes); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("环境变量 %s 非法（需为正整数）: %q", EnvMaxTextBytes, v)
		}
		c.MaxTextBytes = n
	}
	if v := os.Getenv(EnvMaxFileBytes); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("环境变量 %s 非法（需为正整数）: %q", EnvMaxFileBytes, v)
		}
		c.MaxFileBytes = n
	}
	return nil
}

// normalize 把配置文件里显式写成空串/0 的字段回退到默认值。
func (c *Config) normalize() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.DataDir == "" {
		c.DataDir = DefaultDataDir
	}
	if c.MaxTextBytes <= 0 {
		c.MaxTextBytes = DefaultMaxTextBytes
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = DefaultMaxFileBytes
	}
}
