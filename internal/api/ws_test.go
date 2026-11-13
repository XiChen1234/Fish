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
		Endpoints: []config.Endpoint{
			{ID: "a", Token: "tok-a"},
			{ID: "b", Token: "tok-b"},
		},
		MaxTextBytes: config.DefaultMaxTextBytes,
		MaxFileBytes: config.DefaultMaxFileBytes,
	}
	a := auth.New(map[string]string{"a": "tok-a", "b": "tok-b"})
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	h := hub.New(nil)
	h.SetHandler(msg.New(st, h, a, cfg))
	h.LastSeq = func() int64 { return st.LastSeq() }
	s := New(cfg, a, h, st)
	ts := httptest.NewServer(s.Routes())
	t.Cleanup(ts.Close)
	return ts
}

func wsURL(ts *httptest.Server, query string) string {
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	if query != "" {
		u += "?" + query
	}
	return u
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

// 正确 token（header 方式）应完成握手并收到 ready 帧。
func TestWS_AuthHeader_OK(t *testing.T) {
	ts := newTestServer(t)

	hdr := http.Header{"Authorization": []string{"Bearer tok-a"}}
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL(ts, ""), hdr)
	if err != nil {
		t.Fatalf("握手失败: %v (http=%d)", err, resp.StatusCode)
	}
	defer ws.Close()

	f := readFrame(t, ws)
	if f.Type != "ready" {
		t.Fatalf("首帧应为 ready，got %s", f.Type)
	}
	var d struct {
		You  string `json:"you"`
		Peer string `json:"peer"`
	}
	if err := json.Unmarshal(f.Data, &d); err != nil {
		t.Fatalf("ready.data 解析失败: %v", err)
	}
	if d.You != "a" || d.Peer != "b" {
		t.Fatalf("ready 身份错误: you=%s peer=%s", d.You, d.Peer)
	}
}

// 正确 token（query 方式，供浏览器 WS 使用）应完成握手。
func TestWS_AuthQuery_OK(t *testing.T) {
	ts := newTestServer(t)

	ws, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "token=tok-b"), nil)
	if err != nil {
		t.Fatalf("握手失败: %v", err)
	}
	defer ws.Close()

	f := readFrame(t, ws)
	if f.Type != "ready" {
		t.Fatalf("首帧应为 ready，got %s", f.Type)
	}
	var d struct {
		You  string `json:"you"`
		Peer string `json:"peer"`
	}
	_ = json.Unmarshal(f.Data, &d)
	if d.You != "b" || d.Peer != "a" {
		t.Fatalf("ready 身份错误: you=%s peer=%s", d.You, d.Peer)
	}
}

// 错误 token 应被拒绝（401，握手不升级）。
func TestWS_BadToken_Rejected(t *testing.T) {
	ts := newTestServer(t)

	ws, resp, err := websocket.DefaultDialer.Dial(wsURL(ts, "token=wrong"), nil)
	if err == nil {
		ws.Close()
		t.Fatal("错误 token 不应握手成功")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("应返回 401，got %v", resp)
	}
}

// 未带 token 应被拒绝。
func TestWS_NoToken_Rejected(t *testing.T) {
	ts := newTestServer(t)

	ws, resp, err := websocket.DefaultDialer.Dial(wsURL(ts, ""), nil)
	if err == nil {
		ws.Close()
		t.Fatal("未带 token 不应握手成功")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("应返回 401，got %v", resp)
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

	ws, _, err := websocket.DefaultDialer.Dial(wsURL(ts, "token=tok-a"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	// 先读 ready
	if f := readFrame(t, ws); f.Type != "ready" {
		t.Fatalf("首帧应为 ready，got %s", f.Type)
	}

	if err := ws.WriteJSON(hub.Frame{V: 1, Type: "ping"}); err != nil {
		t.Fatal(err)
	}
	f := readFrame(t, ws)
	if f.Type != "pong" {
		t.Fatalf("ping 应答 pong，got %s", f.Type)
	}
}
