package msg_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"fish/internal/auth"
	"fish/internal/config"
	"fish/internal/hub"
	"fish/internal/msg"
	"fish/internal/store"
)

// ---- 测试基础设施 ----

func startFullServer(t *testing.T) (
	cfg *config.Config,
	st *store.Store,
	a *auth.Auth,
	ts *httptest.Server,
) {
	t.Helper()
	cfg = &config.Config{
		MaxTextBytes: config.DefaultMaxTextBytes,
		MaxFileBytes: config.DefaultMaxFileBytes,
	}
	var err error
	st, err = store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	a = auth.New(st, []byte("test-jwt-secret-key-32bytes!"))

	h := hub.New(nil)
	h.SetHandler(msg.New(st, h, cfg))
	h.LastSeq = func() int64 { return st.LastSeq() }
	h.PeerOf = auth.PeerOf

	// 用 httptest.NewServer 起一个只路由 WS 的 handler
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", handleWSForTest(a, h))
	ts = httptest.NewServer(mux)
	testServerURL = ts.URL
	t.Cleanup(ts.Close)
	return cfg, st, a, ts
}

// handleWSForTest 完整复刻 api/server.go 的 handleWS 流程，
// 因为 api.New 依赖完整 Server 类型但测试只需要 WS 入口。
func handleWSForTest(a *auth.Auth, h *hub.Hub) http.HandlerFunc {
	var upgrader = websocket.Upgrader{
		ReadBufferSize: 4096, WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	return func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_ = ws.SetReadDeadline(time.Now().Add(30 * time.Second))
		var f hub.Frame
		if err := ws.ReadJSON(&f); err != nil || f.Type != "auth" {
			d, _ := json.Marshal(map[string]any{"success": false, "error": "握手失败"})
			_ = ws.WriteJSON(hub.Frame{V: 1, Type: "auth.result", Data: d})
			return
		}
		var req struct {
			DeviceID    string `json:"device_id"`
			Password    string `json:"password"`
			Action      string `json:"action"`
			DesiredRole string `json:"desired_role,omitempty"`
			Token       string `json:"token,omitempty"`
		}
		_ = json.Unmarshal(f.Data, &req)
		var role, token string
		switch req.Action {
		case "init":
			role, token, err = a.Init(req.DeviceID, req.Password)
		case "login":
			role, token, err = a.Login(req.DeviceID, req.Password, req.DesiredRole)
		case "resume":
			var ok bool
			role, _, ok = a.Verify(req.Token)
			if ok {
				token = req.Token
			} else {
				err = auth.ErrInvalidToken
			}
		default:
			err = fmt.Errorf("未知 action: %s", req.Action)
		}
		if err != nil {
			d, _ := json.Marshal(map[string]any{"success": false, "error": err.Error()})
			_ = ws.WriteJSON(hub.Frame{V: 1, Type: "auth.result", Data: d})
			return
		}
		d, _ := json.Marshal(map[string]any{
			"success":   true,
			"role":      role,
			"device_id": req.DeviceID,
			"token":     token,
		})
		_ = ws.WriteJSON(hub.Frame{V: 1, Type: "auth.result", TS: time.Now().UnixMilli(), Data: d})
		c := hub.NewConn(role, req.DeviceID, ws)
		h.Run(c)
	}
}

var testServerURL string

func newHTTPTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(h)
	testServerURL = ts.URL
	t.Cleanup(ts.Close)
	return ts
}

func serverURL() string { return testServerURL }

func trimHTTP(u string) string { return strings.TrimPrefix(u, "http") }

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// dialClientWS 建 WS + 发 auth.init 或 auth.login，返回已就绪连接。
func dialClientWS(t *testing.T, tsURL, deviceID, password, action, desiredRole string) *websocket.Conn {
	t.Helper()
	u := "ws" + trimHTTP(tsURL) + "/ws"
	ws, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("WS dial: %v (resp=%v)", err, resp)
	}
	t.Cleanup(func() { ws.Close() })

	authData, _ := json.Marshal(map[string]any{
		"device_id":    deviceID,
		"password":     password,
		"action":       action,
		"desired_role": desiredRole,
	})
	_ = ws.WriteJSON(hub.Frame{V: 1, Type: "auth", Data: authData})

	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	var f hub.Frame
	if err := ws.ReadJSON(&f); err != nil {
		t.Fatalf("读 auth.result: %v", err)
	}
	if f.Type != "auth.result" {
		t.Fatalf("期望 auth.result, got %s", f.Type)
	}
	var res struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Role    string `json:"role"`
		Token   string `json:"token"`
	}
	_ = json.Unmarshal(f.Data, &res)
	if !res.Success {
		t.Fatalf("auth 失败: %s", res.Error)
	}
	return ws
}
