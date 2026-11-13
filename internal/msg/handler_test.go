package msg_test

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"fish/internal/api"
	"fish/internal/auth"
	"fish/internal/config"
	"fish/internal/hub"
	"fish/internal/msg"
	"fish/internal/store"
)

// ---- 测试基础设施：起完整服务端 + 双 WS 客户端 ----

func startFullServer(t *testing.T) *config.Config {
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
	h := hub.New(nil)
	h.SetHandler(msg.New(st, h, a, cfg))
	h.LastSeq = func() int64 { return st.LastSeq() }
	s := api.New(cfg, a, h, st)
	ts := newHTTPTestServer(t, s.Routes())
	t.Cleanup(func() { ts.Close(); st.Close() })
	return cfg
}

type readyData struct {
	You        string `json:"you"`
	Peer       string `json:"peer"`
	PeerOnline bool   `json:"peer_online"`
	LastSeq    int64  `json:"last_seq"`
}

type msgData struct {
	Seq    int64  `json:"seq"`
	From   string `json:"from"`
	Kind   string `json:"kind"`
	Body   string `json:"body"`
	TS     int64  `json:"ts"`
}

func dialClient(t *testing.T, tsURL, token string) *websocket.Conn {
	t.Helper()
	u := "ws" + trimHTTP(tsURL) + "/ws?token=" + token
	ws, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("连接失败 token=%s: %v (http=%v)", token, err, resp)
	}
	t.Cleanup(func() { ws.Close() })

	var f hub.Frame
	if err := ws.ReadJSON(&f); err != nil || f.Type != "ready" {
		t.Fatalf("首帧应为 ready: type=%s err=%v", f.Type, err)
	}
	return ws
}

func send(t *testing.T, ws *websocket.Conn, f hub.Frame) {
	t.Helper()
	if err := ws.WriteJSON(f); err != nil {
		t.Fatalf("发送帧失败: %v", err)
	}
}

func recv(t *testing.T, ws *websocket.Conn) hub.Frame {
	t.Helper()
	var f hub.Frame
	if err := ws.ReadJSON(&f); err != nil {
		t.Fatalf("读帧失败: %v", err)
	}
	return f
}

func mustType(t *testing.T, ws *websocket.Conn, want string) hub.Frame {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		f := recv(t, ws)
		if f.Type == "presence" {
			continue // 在线状态推送与消息断言无关，跳过
		}
		if f.Type != want {
			t.Fatalf("期望帧 %s，实际 %s", want, f.Type)
		}
		return f
	}
}

func parse[T any](t *testing.T, f hub.Frame) T {
	t.Helper()
	var d T
	if err := json.Unmarshal(f.Data, &d); err != nil {
		t.Fatalf("解析 %s.data 失败: %v (%s)", f.Type, err, f.Data)
	}
	return d
}

// ---- 场景 1：在线互发（A→B→回执链路完整）----

func TestE2E_OnlineTextFlow(t *testing.T) {
	cfg := startFullServer(t)
	wsA := dialClient(t, serverURL(), "tok-a")
	wsB := dialClient(t, serverURL(), "tok-b")

	// A 发送
	send(t, wsA, hub.Frame{V: 1, Type: "msg.send", ID: "r1",
		Data: json.RawMessage(`{"kind":"text","body":"hello"}`)})

	// A 收到持久化回执
	ack := mustType(t, wsA, "msg.ack")
	ackD := parse[struct {
		Ref string `json:"ref"`
		Seq int64  `json:"seq"`
		TS  int64  `json:"ts"`
	}](t, ack)
	if ackD.Ref != "r1" || ackD.Seq <= 0 || ackD.TS <= 0 {
		t.Fatalf("msg.ack 数据异常: %+v", ackD)
	}

	// B 实时收到
	newF := mustType(t, wsB, "msg.new")
	m := parse[msgData](t, newF)
	if m.Seq != ackD.Seq || m.From != "a" || m.Kind != "text" || m.Body != "hello" {
		t.Fatalf("msg.new 数据异常: %+v", m)
	}

	// B 确认 → A 收到送达回执
	send(t, wsB, hub.Frame{V: 1, Type: "msg.ack", Data: json.RawMessage(`{"seq":` + itoa(m.Seq) + `}`)})
	del := mustType(t, wsA, "msg.delivered")
	delD := parse[struct {
		Seq int64 `json:"seq"`
	}](t, del)
	if delD.Seq != m.Seq {
		t.Fatalf("msg.delivered seq 不符: got %d want %d", delD.Seq, m.Seq)
	}

	// 双向：B → A
	send(t, wsB, hub.Frame{V: 1, Type: "msg.send", ID: "r2",
		Data: json.RawMessage(`{"kind":"text","body":"world"}`)})
	_ = mustType(t, wsB, "msg.ack")
	n := mustType(t, wsA, "msg.new")
	m2 := parse[msgData](t, n)
	if m2.From != "b" || m2.Body != "world" {
		t.Fatalf("反向消息异常: %+v", m2)
	}
	_ = cfg
}

// ---- 场景 2：离线补投 ----

func TestE2E_OfflineCatchUp(t *testing.T) {
	startFullServer(t)
	wsA := dialClient(t, serverURL(), "tok-a")

	// B 先连一次再断开（模拟离线）
	wsB := dialClient(t, serverURL(), "tok-b")
	wsB.Close()

	// A 连发两条（B 离线期间，应全部落库）
	var seqs []int64
	for _, body := range []string{"offline-1", "offline-2"} {
		send(t, wsA, hub.Frame{V: 1, Type: "msg.send", ID: body,
			Data: json.RawMessage(`{"kind":"text","body":"` + body + `"}`)})
		ack := mustType(t, wsA, "msg.ack")
		d := parse[struct {
			Seq int64 `json:"seq"`
		}](t, ack)
		seqs = append(seqs, d.Seq)
	}

	// B 重连：ready.last_seq 应为 2，且应感知离线
	wsB2 := dialClientRaw(t, serverURL(), "tok-b")
	var rf hub.Frame
	_ = wsB2.ReadJSON(&rf)
	rd := parse[readyData](t, rf)
	if rd.LastSeq != 2 {
		t.Fatalf("ready.last_seq 应为 2，got %d", rd.LastSeq)
	}

	// B sync 补投
	send(t, wsB2, hub.Frame{V: 1, Type: "sync", ID: "s1",
		Data: json.RawMessage(`{"after":0,"limit":50}`)})
	done := mustType(t, wsB2, "sync.done")
	sd := parse[struct {
		Msgs    []msgData `json:"msgs"`
		HasMore bool      `json:"has_more"`
	}](t, done)
	if len(sd.Msgs) != 2 || sd.HasMore {
		t.Fatalf("sync.done 应含 2 条离线消息: %+v", sd)
	}
	if sd.Msgs[0].Body != "offline-1" || sd.Msgs[1].Body != "offline-2" {
		t.Fatalf("补投顺序错误: %+v", sd.Msgs)
	}
	if sd.Msgs[0].Seq != seqs[0] || sd.Msgs[1].Seq != seqs[1] {
		t.Fatalf("补投 seq 与落库不符: %+v want %v", sd.Msgs, seqs)
	}

	// B 补投后确认 → A（保持在线）收到送达回执
	send(t, wsB2, hub.Frame{V: 1, Type: "msg.ack", Data: json.RawMessage(`{"seq":` + itoa(seqs[0]) + `}`)})
	del := mustType(t, wsA, "msg.delivered")
	dd := parse[struct {
		Seq int64 `json:"seq"`
	}](t, del)
	if dd.Seq != seqs[0] {
		t.Fatalf("离线消息回执 seq 不符: got %d want %d", dd.Seq, seqs[0])
	}
}

// ---- 场景 3：sync 分页 ----

func TestE2E_SyncPagination(t *testing.T) {
	startFullServer(t)
	wsA := dialClient(t, serverURL(), "tok-a")
	wsB := dialClient(t, serverURL(), "tok-b")

	// 发 5 条，B 侧只读 ack 侧消息流向（msg.new 都到 B，需排空）
	var lastSeq int64
	for i := 0; i < 5; i++ {
		send(t, wsA, hub.Frame{V: 1, Type: "msg.send", ID: itoa(int64(i)),
			Data: json.RawMessage(`{"kind":"text","body":"m` + itoa(int64(i)) + `"}`)})
		ack := mustType(t, wsA, "msg.ack")
		d := parse[struct {
			Seq int64 `json:"seq"`
		}](t, ack)
		lastSeq = d.Seq
		_ = mustType(t, wsB, "msg.new") // B 排空实时投递
	}

	// B 重连后分页拉取
	wsB.Close()
	wsB2 := dialClientRaw(t, serverURL(), "tok-b")
	// 排空 ready
	var rf hub.Frame
	_ = wsB2.ReadJSON(&rf)

	page1 := syncPage(t, wsB2, 0, 2)
	if len(page1.msgs) != 2 || !page1.hasMore {
		t.Fatalf("第1页应 2 条且有更多: %+v", page1)
	}
	page2 := syncPage(t, wsB2, page1.msgs[1].Seq, 2)
	if len(page2.msgs) != 2 || !page2.hasMore {
		t.Fatalf("第2页应 2 条且有更多: %+v", page2)
	}
	page3 := syncPage(t, wsB2, page2.msgs[1].Seq, 2)
	if len(page3.msgs) != 1 || page3.hasMore {
		t.Fatalf("第3页应 1 条且无更多: %+v", page3)
	}
	if page3.msgs[0].Seq != lastSeq {
		t.Fatalf("末页 seq 应为 %d，got %d", lastSeq, page3.msgs[0].Seq)
	}
}

type page struct {
	msgs    []msgData
	hasMore bool
}

func syncPage(t *testing.T, ws *websocket.Conn, after int64, limit int) page {
	t.Helper()
	send(t, ws, hub.Frame{V: 1, Type: "sync",
		Data: json.RawMessage(`{"after":` + itoa(after) + `,"limit":` + itoa(int64(limit)) + `}`)})
	done := mustType(t, ws, "sync.done")
	d := parse[struct {
		Msgs    []msgData `json:"msgs"`
		HasMore bool      `json:"has_more"`
	}](t, done)
	return page{msgs: d.Msgs, hasMore: d.HasMore}
}

// ---- 场景 4：错误路径 ----

func TestE2E_ErrorFrames(t *testing.T) {
	startFullServer(t)
	wsA := dialClient(t, serverURL(), "tok-a")

	cases := []struct {
		name     string
		frame    string
		wantCode int
	}{
		{"未知帧类型", `{"v":1,"type":"hello"}`, 4004},
		{"空 body", `{"v":1,"type":"msg.send","data":{"kind":"text","body":""}}`, 4003},
		{"不支持 kind", `{"v":1,"type":"msg.send","data":{"kind":"video","body":"x"}}`, 4003},
		{"ack 不存在的消息", `{"v":1,"type":"msg.ack","data":{"seq":99999}}`, 4003},
		{"ack 非法参数", `{"v":1,"type":"msg.ack","data":{"seq":0}}`, 4003},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := wsA.WriteMessage(websocket.TextMessage, []byte(tc.frame)); err != nil {
				t.Fatal(err)
			}
			f := mustType(t, wsA, "error")
			d := parse[struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}](t, f)
			if d.Code != tc.wantCode {
				t.Fatalf("错误码不符: got %d want %d (%s)", d.Code, tc.wantCode, d.Message)
			}
		})
	}
}

// ---- 场景 5：发送方自己确认无效 ----

func TestE2E_SelfAck_Ignored(t *testing.T) {
	startFullServer(t)
	wsA := dialClient(t, serverURL(), "tok-a")
	wsB := dialClient(t, serverURL(), "tok-b")

	send(t, wsA, hub.Frame{V: 1, Type: "msg.send", ID: "r9",
		Data: json.RawMessage(`{"kind":"text","body":"x"}`)})
	_ = mustType(t, wsA, "msg.ack")
	m := parse[msgData](t, mustType(t, wsB, "msg.new"))

	// A 对自己发的消息 ack → 服务端静默忽略，不产生 delivered 给任何一方
	send(t, wsA, hub.Frame{V: 1, Type: "msg.ack", Data: json.RawMessage(`{"seq":` + itoa(m.Seq) + `}`)})
	// 此时 B 再发一条正常消息验证链路仍正常（无错误帧干扰）
	send(t, wsB, hub.Frame{V: 1, Type: "msg.send", ID: "r10",
		Data: json.RawMessage(`{"kind":"text","body":"y"}`)})
	f := mustType(t, wsA, "msg.new")
	if parse[msgData](t, f).Body != "y" {
		t.Fatal("self-ack 后链路应保持正常")
	}
}
