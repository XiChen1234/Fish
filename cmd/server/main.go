package main

import (
	"context"
	"crypto/rand"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"fish/internal/api"
	"fish/internal/auth"
	"fish/internal/config"
	"fish/internal/hub"
	"fish/internal/msg"
	"fish/internal/store"
)

func main() {
	// 配置文件是可选的：不指定也能启动（默认值 + 环境变量）。
	cfgPath := flag.String("config", config.ConfigPath("config.json"), "配置文件路径（可选，不存在时使用默认值）")
	listen := flag.String("listen", "", "监听地址，覆盖配置文件/环境变量，如 :19090")
	dataDir := flag.String("data-dir", "", "数据目录，覆盖配置文件/环境变量")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	cfgFile := "无（默认值 + 环境变量）"
	if *cfgPath != "" {
		if _, statErr := os.Stat(*cfgPath); statErr == nil {
			cfgFile = *cfgPath
		}
	}
	log.Printf("配置就绪: listen=%s data_dir=%s max_text=%dB max_file=%dB（配置文件: %s）",
		cfg.Listen, cfg.DataDir, cfg.MaxTextBytes, cfg.MaxFileBytes, cfgFile)

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}

	st, err := store.Open(filepath.Join(cfg.DataDir, "fish.db"))
	if err != nil {
		log.Fatalf("打开存储失败: %v", err)
	}
	defer st.Close()
	log.Printf("存储就绪: 当前最大 seq=%d", st.LastSeq())

	jwtKey := loadOrGenJWTKey(filepath.Join(cfg.DataDir, "jwt.key"))
	a := auth.New(st, jwtKey)
	log.Printf("鉴权就绪（动态配对模式，JWT key 持久化）")

	h := hub.New(nil) // handler 在下方接线
	handler := msg.New(st, h, cfg)
	h.SetHandler(handler)
	h.LastSeq = func() int64 { return st.LastSeq() }

	srv := api.New(cfg, a, h, st)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("服务启动: http://%s  (健康检查 /healthz, WS /ws)", cfg.Listen)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("服务异常退出: %v", err)
		}
	}()

	// 优雅退出
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Println("收到退出信号，正在关闭...")
	_ = httpSrv.Shutdown(context.Background())
	log.Println("已退出")
}

// loadOrGenJWTKey 从 path 读取 HMAC 密钥，不存在则生成并写入（32 字节）。
// 持久化后服务重启 JWT 仍有效。
func loadOrGenJWTKey(path string) []byte {
	if data, err := os.ReadFile(path); err == nil && len(data) >= 16 {
		return data
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		log.Fatalf("生成 JWT key 失败: %v", err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		log.Fatalf("写入 JWT key 失败: %v", err)
	}
	log.Printf("已生成并保存 JWT key 到 %s", path)
	return key
}
