package api

import (
	_ "embed"
	"encoding/json"
	"net/http"

	"github.com/gorilla/websocket"

	"fish/internal/auth"
	"fish/internal/config"
	"fish/internal/hub"
	"fish/internal/store"
)

//go:embed web/index.html
var indexHTML []byte

// Server HTTP + WS 入口。存储与业务逻辑由 api 依赖的 hub.Handler 承担。
type Server struct {
	cfg   *config.Config
	auth  *auth.Auth
	hub   *hub.Hub
	store *store.Store
}

func New(cfg *config.Config, a *auth.Auth, h *hub.Hub, st *store.Store) *Server {
	h.PeerOf = a.Peer
	return &Server{cfg: cfg, auth: a, hub: h, store: st}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("POST /api/files", s.handleFileUpload)
	mux.HandleFunc("GET /api/files/{id}", s.handleFileDownload)
	mux.HandleFunc("GET /{$}", s.handleIndex)
	return mux
}

// handleIndex 托管内置网页客户端（协议适配演示端）。
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleWS WS 握手 + 鉴权（Bearer 头或 ?token=），成功后进入连接主循环。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	token := bearer(r)
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	id, ok := s.auth.Verify(token)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid token"})
		return
	}

	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := hub.NewConn(id, ws)
	s.hub.Run(c)
}

func bearer(r *http.Request) string {
	const prefix = "Bearer "
	v := r.Header.Get("Authorization")
	if len(v) > len(prefix) && v[:len(prefix)] == prefix {
		return v[len(prefix):]
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
