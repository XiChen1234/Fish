package config

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Endpoint 固定对端身份。本期仅支持 a/b 两个。
type Endpoint struct {
	ID    string `json:"id"`    // 身份标识，如 "a"、"b"
	Token string `json:"token"` // 该身份的密钥
}

type Config struct {
	Listen       string     `json:"listen"`         // 监听地址，如 ":8080"
	DataDir      string     `json:"data_dir"`       // 数据目录（SQLite + 图片文件）
	Endpoints    []Endpoint `json:"endpoints"`      // 固定身份列表，长度必须为 2
	MaxTextBytes int        `json:"max_text_bytes"` // 文本消息上限（字节）
	MaxFileBytes int64      `json:"max_file_bytes"` // 图片上传上限（字节）
}

const (
	DefaultMaxTextBytes = 8 * 1024
	DefaultMaxFileBytes = 10 * 1024 * 1024
)

// Load 从 path 读取配置；文件不存在时生成默认配置（含随机 token）并写回。
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		cfg, gerr := generate()
		if gerr != nil {
			return nil, gerr
		}
		if werr := save(path, cfg); werr != nil {
			return nil, fmt.Errorf("写入默认配置失败: %w", werr)
		}
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	// 容忍 UTF-8 BOM（Windows 记事本/PowerShell 写配置常见）
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}), &cfg); err != nil {
		return nil, fmt.Errorf("解析配置 %s 失败: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func generate() (*Config, error) {
	tokA, err := randomToken()
	if err != nil {
		return nil, err
	}
	tokB, err := randomToken()
	if err != nil {
		return nil, err
	}
	cfg := &Config{
		Listen:       ":8080",
		DataDir:      "./data",
		MaxTextBytes: DefaultMaxTextBytes,
		MaxFileBytes: DefaultMaxFileBytes,
		Endpoints: []Endpoint{
			{ID: "a", Token: tokA},
			{ID: "b", Token: tokB},
		},
	}
	return cfg, nil
}

func save(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return os.WriteFile(path, data, 0o600)
}

func (c *Config) validate() error {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if len(c.Endpoints) != 2 {
		return fmt.Errorf("endpoints 必须恰好配置 2 个固定身份，当前 %d 个", len(c.Endpoints))
	}
	seen := map[string]bool{}
	for _, ep := range c.Endpoints {
		if ep.ID == "" || ep.Token == "" {
			return fmt.Errorf("endpoint id/token 不能为空")
		}
		if seen[ep.ID] {
			return fmt.Errorf("endpoint id 重复: %s", ep.ID)
		}
		seen[ep.ID] = true
	}
	if c.MaxTextBytes <= 0 {
		c.MaxTextBytes = DefaultMaxTextBytes
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = DefaultMaxFileBytes
	}
	return nil
}

func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
