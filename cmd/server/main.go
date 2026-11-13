package main

import (
	"context"
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
	cfgPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	log.Printf("配置加载成功: listen=%s data_dir=%s", cfg.Listen, cfg.DataDir)
	for _, ep := range cfg.Endpoints {
		log.Printf("固定身份: id=%s token=%s", ep.ID, ep.Token)
	}

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}

	tokens := make(map[string]string, len(cfg.Endpoints))
	for _, ep := range cfg.Endpoints {
		tokens[ep.ID] = ep.Token
	}
	a := auth.New(tokens)

	st, err := store.Open(filepath.Join(cfg.DataDir, "fish.db"))
	if err != nil {
		log.Fatalf("打开存储失败: %v", err)
	}
	defer st.Close()
	log.Printf("存储就绪: 当前最大 seq=%d", st.LastSeq())

	h := hub.New(nil) // handler 在下方接线（hub 引用需先创建）
	handler := msg.New(st, h, a, cfg)
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
