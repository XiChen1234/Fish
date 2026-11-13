package client_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"fish/internal/auth"
	"fish/internal/config"
	"fish/internal/hub"
	"fish/internal/msg"
	"fish/internal/store"

	"fish/client"
)

// newTestServer 起一个完整服务端，返回 httptest.Server URL。
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		MaxTextBytes: config.DefaultMaxTextBytes,
		MaxFileBytes: config.DefaultMaxFileBytes,
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := auth.New(st, []byte("test-jwt-secret-key-32bytes!!"))
	h := hub.New(nil)
	h.SetHandler(msg.New(st, h, cfg))
	h.LastSeq = func() int64 { return st.LastSeq() }

	var upgrader = websocket.Upgrader{
		ReadBufferSize: 4096, WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_ = ws.SetReadDeadline(time.Now().Add(30 * time.Second))
		var f hub.Frame
		if err := ws.ReadJSON(&f); err != nil || f.Type != "auth" {
			return
		}
		var req struct {
			DeviceID    string `json:"device_id"`
			Password    string `json:"password"`
			Action      string `json:"action"`
			DesiredRole string `json:"desired_role"`
			Token       string `json:"token"`
		}
		_ = json.Unmarshal(f.Data, &req)
		var role, token string
		var err2 error
		switch req.Action {
		case "init":
			role, token, err2 = a.Init(req.DeviceID, req.Password)
		case "login":
			role, token, err2 = a.Login(req.DeviceID, req.Password, req.DesiredRole)
		case "resume":
			var ok bool
			role, _, ok = a.Verify(req.Token)
			if ok {
				token = req.Token
			} else {
				err2 = auth.ErrInvalidToken
			}
		default:
			err2 = auth.ErrInvalidToken
		}
		if err2 != nil {
			d, _ := json.Marshal(map[string]any{"success": false, "error": err2.Error()})
			_ = ws.WriteJSON(hub.Frame{V: 1, Type: "auth.result", Data: d})
			return
		}
		peerOnline := false
		if peer, ok := auth.PeerOf(role); ok {
			peerOnline = h.Online(peer)
		}
		var lastSeq int
		if h.LastSeq != nil {
			lastSeq = int(h.LastSeq())
		}
		d, _ := json.Marshal(map[string]any{
			"success":     true,
			"role":        role,
			"device_id":   req.DeviceID,
			"token":       token,
			"peer_online": peerOnline,
			"last_seq":    lastSeq,
		})
		_ = ws.WriteJSON(hub.Frame{V: 1, Type: "auth.result", TS: time.Now().UnixMilli(), Data: d})
		c := hub.NewConn(role, req.DeviceID, ws)
		h.Run(c)
	})
	return httptest.NewServer(mux)
}

func TestInitAndLogin(t *testing.T) {
	ts := newTestServer(t)

	var gotReadyA, gotReadyB bool
	cb := func(ev client.Event) {
		if ev.Type == client.EventReady {
			if ev.Ready.Role == "A" {
				gotReadyA = true
			} else if ev.Ready.Role == "B" {
				gotReadyB = true
			}
		}
	}

	a := client.New(ts.URL, cb)
	if err := a.Connect(client.ConnectOptions{DeviceID: "dev-a", Password: "secret1234", Action: client.ActionInit}); err != nil {
		t.Fatalf("A init: %v", err)
	}
	defer a.Close()

	b := client.New(ts.URL, cb)
	if err := b.Connect(client.ConnectOptions{DeviceID: "dev-b", Password: "secret1234", Action: client.ActionLogin}); err != nil {
		t.Fatalf("B login: %v", err)
	}
	defer b.Close()

	if !gotReadyA || !gotReadyB {
		t.Fatalf("ready 未触发 A=%v B=%v", gotReadyA, gotReadyB)
	}
	if a.Role() != "A" || b.Role() != "B" {
		t.Fatalf("role 不符 A=%s B=%s", a.Role(), b.Role())
	}
}

func TestSendAndReceive(t *testing.T) {
	ts := newTestServer(t)

	msgCh := make(chan *client.Message, 8)
	cb := func(ev client.Event) {
		if ev.Type == client.EventMessage {
			msgCh <- ev.Message
		}
	}

	a := client.New(ts.URL, cb)
	_ = a.Connect(client.ConnectOptions{DeviceID: "dev-a", Password: "secret1234", Action: client.ActionInit})
	defer a.Close()
	b := client.New(ts.URL, cb)
	_ = b.Connect(client.ConnectOptions{DeviceID: "dev-b", Password: "secret1234", Action: client.ActionLogin})
	defer b.Close()

	// B 发一条给 A
	seq, err := b.SendText("hello-A")
	if err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if seq <= 0 {
		t.Fatalf("seq 应 >0, got %d", seq)
	}

	select {
	case m := <-msgCh:
		if m.From != "B" || m.Body != "hello-A" || m.Seq != seq {
			t.Fatalf("消息不符: %+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("A 未收到消息")
	}
}

func TestResume(t *testing.T) {
	ts := newTestServer(t)

	a := client.New(ts.URL, nil)
	_ = a.Connect(client.ConnectOptions{DeviceID: "dev-a", Password: "secret1234", Action: client.ActionInit})
	defer a.Close()
	tokenA := a.Token()
	if tokenA == "" {
		t.Fatal("A token 为空")
	}

	// 断开后用 resume 重连
	a2 := client.New(ts.URL, nil)
	if err := a2.Connect(client.ConnectOptions{Action: client.ActionResume, Token: tokenA, DeviceID: "dev-a"}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	defer a2.Close()
	if a2.Role() != "A" {
		t.Fatalf("resume 后 role 应仍为 A，got %s", a2.Role())
	}
}

func TestWrongPassword(t *testing.T) {
	ts := newTestServer(t)
	a := client.New(ts.URL, nil)
	_ = a.Connect(client.ConnectOptions{DeviceID: "dev-a", Password: "secret1234", Action: client.ActionInit})
	defer a.Close()

	b := client.New(ts.URL, nil)
	err := b.Connect(client.ConnectOptions{DeviceID: "dev-b", Password: "wrong", Action: client.ActionLogin})
	if err == nil {
		t.Fatal("坏密码应报错")
	}
}
