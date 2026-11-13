package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		DataDir:      t.TempDir(), // 避免测试产物（files/）落到包目录
		MaxTextBytes: config.DefaultMaxTextBytes,
		MaxFileBytes: config.DefaultMaxFileBytes,
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	a := auth.New(st, []byte("test-key-32bytes!!abcdefghij"))
	h := hub.New(nil)
	h.SetHandler(msg.New(st, h, cfg))
	h.LastSeq = func() int64 { return st.LastSeq() }
	s := New(cfg, a, h, st)
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	return ts
}

func wsURL(ts *httptest.Server) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
}

func readFrame(t *testing.T, ws *websocket.Conn) hub.Frame {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var f hub.Frame
	if err := ws.ReadJSON(&f); err != nil {
		t.Fatalf("读帧失败: %v", err)
	}
	_ = ws.SetReadDeadline(time.Time{})
	return f
}

// dialWS 握手并发送 auth 帧，返回已就绪 WS 连接。
// 调用方负责 close。
func dialWS(t *testing.T, ts *httptest.Server, deviceID, password, action, desiredRole string) *websocket.Conn {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatal(err)
	}
	authData, _ := json.Marshal(map[string]any{
		"device_id":    deviceID,
		"password":     password,
		"action":       action,
		"desired_role": desiredRole,
	})
	_ = ws.WriteJSON(hub.Frame{V: 1, Type: "auth", Data: authData})
	f := readFrame(t, ws)
	if f.Type != "auth.result" {
		t.Fatalf("期望 auth.result, got %s", f.Type)
	}
	var res struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
		Role    string `json:"role"`
	}
	_ = json.Unmarshal(f.Data, &res)
	if !res.Success {
		t.Fatalf("auth 失败: %s", res.Error)
	}
	return ws
}

// init 首台设备应拿到 role A。
func TestWS_Init_BecomesA(t *testing.T) {
	ts := newTestServer(t)
	ws := dialWS(t, ts, "dev-1", "secret1234", "init", "")
	defer ws.Close()
	// 房间已占，再次 init 应失败
	ws2, _, _ := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	authData, _ := json.Marshal(map[string]any{
		"device_id": "dev-2", "password": "secret1234", "action": "init",
	})
	_ = ws2.WriteJSON(hub.Frame{V: 1, Type: "auth", Data: authData})
	f := readFrame(t, ws2)
	var res struct {
		Success bool `json:"success"`
	}
	_ = json.Unmarshal(f.Data, &res)
	if res.Success {
		t.Fatal("重复 init 应失败")
	}
	ws2.Close()
}

// login 第二台设备应拿到 role B。
func TestWS_Login_SecondIsB(t *testing.T) {
	ts := newTestServer(t)
	wsA := dialWS(t, ts, "dev-A", "secret1234", "init", "")
	defer wsA.Close()
	wsB := dialWS(t, ts, "dev-B", "secret1234", "login", "")
	defer wsB.Close()

	// B Register 时 broadcastPresence → A 收到 presence 帧
	// 过滤掉，只等 msg.new
	drainUntil(t, wsA, "presence")

	// B 发给 A，A 应收到
	sendData, _ := json.Marshal(map[string]any{"kind": "text", "body": "hi-A"})
	_ = wsB.WriteJSON(hub.Frame{V: 1, Type: "msg.send", ID: "x", Data: sendData})

	// B 自己会回 msg.ack；再等 B Register 的 presence 被 A 收到后，A 应收到 msg.new
	var gotMsg bool
	for i := 0; i < 5; i++ {
		f := readFrame(t, wsA)
		if f.Type == "msg.new" {
			gotMsg = true
			break
		}
	}
	if !gotMsg {
		t.Fatal("A 应收到 msg.new")
	}
}

// 同角色多设备：A1 发送后，同角色的 A2 也应收到 msg.new。
func TestWS_SameRoleEcho(t *testing.T) {
	ts := newTestServer(t)
	wsA1 := dialWS(t, ts, "dev-a1", "secret1234", "init", "")
	defer wsA1.Close()
	wsB := dialWS(t, ts, "dev-b", "secret1234", "login", "")
	defer wsB.Close()
	wsA2 := dialWS(t, ts, "dev-a2", "secret1234", "login", "A")
	defer wsA2.Close()

	sendData, _ := json.Marshal(map[string]any{"kind": "text", "body": "from-a1"})
	_ = wsA1.WriteJSON(hub.Frame{V: 1, Type: "msg.send", ID: "r1", Data: sendData})

	for i := 0; i < 5; i++ {
		f := readFrame(t, wsA2)
		if f.Type != "msg.new" {
			continue
		}
		var m struct {
			From string `json:"from"`
			Body string `json:"body"`
		}
		_ = json.Unmarshal(f.Data, &m)
		if m.From != "A" || m.Body != "from-a1" {
			t.Fatalf("回显内容不符: %+v", m)
		}
		return
	}
	t.Fatal("同角色设备 A2 未收到 msg.new")
}

// drainUntil 读掉 ws 上直到遇到 skip 帧类型之前的所有帧。
func drainUntil(t *testing.T, ws *websocket.Conn, skip string) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		var f hub.Frame
		if err := ws.ReadJSON(&f); err != nil {
			return
		}
		if f.Type == skip {
			return
		}
	}
}

// 错误密码应失败。
func TestWS_BadPassword(t *testing.T) {
	ts := newTestServer(t)
	_ = dialWS(t, ts, "dev-1", "secret1234", "init", "")

	ws, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	authData, _ := json.Marshal(map[string]any{
		"device_id": "dev-2", "password": "wrongpw", "action": "login",
	})
	_ = ws.WriteJSON(hub.Frame{V: 1, Type: "auth", Data: authData})
	f := readFrame(t, ws)
	var res struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(f.Data, &res)
	if res.Success {
		t.Fatal("坏密码应失败")
	}
}

// 首帧非 auth 应失败。
func TestWS_FirstFrameNotAuth(t *testing.T) {
	ts := newTestServer(t)
	ws, _, err := websocket.DefaultDialer.Dial(wsURL(ts), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	_ = ws.WriteJSON(hub.Frame{V: 1, Type: "ping"})
	f := readFrame(t, ws)
	var res struct {
		Success bool `json:"success"`
	}
	_ = json.Unmarshal(f.Data, &res)
	if res.Success {
		t.Fatal("首帧非 auth 应失败")
	}
}

// 健康检查端点。
func TestHealthz(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz 应 200，got %d", resp.StatusCode)
	}
}

// ping → pong 心跳往返。
func TestWS_PingPong(t *testing.T) {
	ts := newTestServer(t)
	ws := dialWS(t, ts, "dev-1", "secret1234", "init", "")
	defer ws.Close()

	if err := ws.WriteJSON(hub.Frame{V: 1, Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	f := readFrame(t, ws)
	if f.Type != "pong" {
		t.Fatalf("ping 应答 pong，got %s", f.Type)
	}
}
