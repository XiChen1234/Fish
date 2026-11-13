package api

import (
	_ "embed"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"sync"
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

	// 登录失败限流（按来源 IP）
	authMu    sync.Mutex
	authFails map[string]*authFail
}

// 同一 IP 在窗口期内累计失败达到阈值后暂时拒绝鉴权，避免在线暴力破解密码。
const (
	maxAuthFails   = 10
	authFailWindow = 5 * time.Minute
)

type authFail struct {
	count int
	until time.Time
}

func New(cfg *config.Config, a *auth.Auth, h *hub.Hub, st *store.Store) *Server {
	h.PeerOf = auth.PeerOf
	return &Server{cfg: cfg, auth: a, hub: h, store: st, authFails: make(map[string]*authFail)}
}

// authBlocked 判断来源 IP 是否因失败次数过多被暂时锁定。
func (s *Server) authBlocked(ip string) bool {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	st, ok := s.authFails[ip]
	if !ok {
		return false
	}
	if time.Now().After(st.until) {
		delete(s.authFails, ip)
		return false
	}
	return st.count >= maxAuthFails
}

// noteAuth 记录鉴权结果：成功清零，失败累计。
func (s *Server) noteAuth(ip string, ok bool) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if ok {
		delete(s.authFails, ip)
		return
	}
	st := s.authFails[ip]
	if st == nil || time.Now().After(st.until) {
		st = &authFail{until: time.Now().Add(authFailWindow)}
		s.authFails[ip] = st
	}
	st.count++
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
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
//   - action="init"：房间初始化（首次连接）
//   - action="login"：密码登录（房间已存在）
//   - action="resume"：用已有 token 恢复连接
//
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

	ip := clientIP(r)
	if s.authBlocked(ip) {
		s.writeAuthError(ws, "登录失败次数过多，请稍后再试")
		return
	}

	role, deviceID, token, err := s.doAuth(f)
	s.noteAuth(ip, err == nil)
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
		"success":     true,
		"role":        role,
		"device_id":   deviceID,
		"token":       token,
		"peer_online": peerOnline,
		"last_seq":    lastSeq,
	})
	_ = ws.WriteJSON(hub.Frame{V: 1, Type: "auth.result", TS: time.Now().UnixMilli(), Data: resData})

	c := hub.NewConn(role, deviceID, ws)
	s.hub.Run(c)
}

func (s *Server) doAuth(f hub.Frame) (role, deviceID, token string, err error) {
	var req authReq
	if err := json.Unmarshal(f.Data, &req); err != nil {
		return "", "", "", err
	}
	switch req.Action {
	case "init":
		r, tok, err := s.auth.Init(req.DeviceID, req.Password)
		return r, req.DeviceID, tok, err
	case "login":
		r, tok, err := s.auth.Login(req.DeviceID, req.Password, req.DesiredRole)
		return r, req.DeviceID, tok, err
	case "resume":
		// 用已有 token 恢复，校验设备仍存在且 token 未被换发
		return s.auth.Resume(req.DeviceID, req.Token)
	default:
		return "", "", "", errors.New("未知 action: " + req.Action)
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
