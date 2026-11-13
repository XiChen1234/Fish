package api

import (
	_ "embed"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

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
	h.PeerOf = auth.PeerOf
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
	// 允许任意 Origin；生产环境可改为白名单
	CheckOrigin: func(r *http.Request) bool { return true },
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// handleWS WS 握手 + 帧内 auth 流程：
// 1. HTTP → WS 升级（不做 HTTP 层鉴权，auth 放到 WS 帧里）
// 2. 读取首个帧，类型必须为 "auth"
//    - action="init"：房间初始化（首次连接）
//    - action="login"：密码登录（房间已存在）
//    - action="resume"：用已有 token 恢复连接
// 3. 鉴权成功后写 auth.result 帧，再 NewConn + Hub.Run 进入业务循环
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	// 握手阶段读一个帧
	_ = ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	var f hub.Frame
	if err := ws.ReadJSON(&f); err != nil {
		s.writeAuthError(ws, "握手超时：未收到 auth 帧")
		return
	}
	if f.Type != "auth" {
		s.writeAuthError(ws, "首帧必须为 auth")
		return
	}

	// 解析请求以拿到 device_id（Auth 方法内部也会再解析一次校验字段）
	var req authReq
	_ = json.Unmarshal(f.Data, &req)

	role, token, err := s.doAuth(ws, f)
	if err != nil {
		s.writeAuthError(ws, err.Error())
		return
	}

	// 写 auth.result 成功帧（集成原 ready 帧的所有首帧信息）
	peerOnline := false
	var lastSeq int64
	if s.hub.PeerOf != nil {
		if peer, ok := s.hub.PeerOf(role); ok {
			peerOnline = s.hub.Online(peer)
		}
	}
	if s.hub.LastSeq != nil {
		lastSeq = s.hub.LastSeq()
	}
	resData, _ := json.Marshal(map[string]any{
		"success":    true,
		"role":       role,
		"device_id":  req.DeviceID,
		"token":      token,
		"peer_online": peerOnline,
		"last_seq":   lastSeq,
	})
	_ = ws.WriteJSON(hub.Frame{V: 1, Type: "auth.result", TS: time.Now().UnixMilli(), Data: resData})

	c := hub.NewConn(role, req.DeviceID, ws)
	s.hub.Run(c)
}

func (s *Server) doAuth(ws *websocket.Conn, f hub.Frame) (role, token string, err error) {
	var req authReq
	if err := json.Unmarshal(f.Data, &req); err != nil {
		return "", "", err
	}
	switch req.Action {
	case "init":
		return s.auth.Init(req.DeviceID, req.Password)
	case "login":
		return s.auth.Login(req.DeviceID, req.Password, req.DesiredRole)
	case "resume":
		// 用已有 token 恢复（重连时避免重复密码校验）
		r, _, ok := s.auth.Verify(req.Token)
		if !ok {
			return "", "", auth.ErrInvalidToken
		}
		return r, req.Token, nil
	default:
		return "", "", errors.New("未知 action: " + req.Action)
	}
}

type authReq struct {
	DeviceID    string `json:"device_id"`
	Password    string `json:"password"`
	Action      string `json:"action"`
	DesiredRole string `json:"desired_role,omitempty"`
	Token       string `json:"token,omitempty"` // resume 专用
}

func (s *Server) writeAuthError(ws *websocket.Conn, msg string) {
	log.Printf("[api] WS auth 失败: %s", msg)
	d, _ := json.Marshal(map[string]any{"success": false, "error": msg})
	_ = ws.WriteJSON(hub.Frame{V: 1, Type: "auth.result", TS: time.Now().UnixMilli(), Data: d})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
