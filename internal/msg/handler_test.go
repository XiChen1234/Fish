package msg_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"fish/internal/hub"
)

type readyData struct {
	You        string `json:"you"`
	Peer       string `json:"peer"`
	PeerOnline bool   `json:"peer_online"`
	LastSeq    int64  `json:"last_seq"`
}

type msgData struct {
	Seq  int64  `json:"seq"`
	From string `json:"from"`
	Kind string `json:"kind"`
	Body string `json:"body"`
	TS   int64  `json:"ts"`
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
			continue
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

const testPassword = "test-secret-1234"

// ---- 场景 1：在线互发 ----

func TestE2E_OnlineTextFlow(t *testing.T) {
	startFullServer(t)
	wsA := dialClientWS(t, serverURL(), "dev-A", testPassword, "init", "")
	wsB := dialClientWS(t, serverURL(), "dev-B", testPassword, "login", "")

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
	if ackD.Ref != "r1" || ackD.Seq <= 0 {
		t.Fatalf("msg.ack 异常: %+v", ackD)
	}

	// B 实时收到
	newF := mustType(t, wsB, "msg.new")
	m := parse[msgData](t, newF)
	if m.Seq != ackD.Seq || m.From != "A" || m.Body != "hello" {
		t.Fatalf("msg.new 异常: %+v", m)
	}

	// B 确认 → A 收到送达回执
	send(t, wsB, hub.Frame{V: 1, Type: "msg.ack", Data: json.RawMessage(`{"seq":` + itoa(m.Seq) + `}`)})
	del := mustType(t, wsA, "msg.delivered")
	delD := parse[struct {
		Seq int64 `json:"seq"`
	}](t, del)
	if delD.Seq != m.Seq {
		t.Fatalf("delivered seq 不符")
	}
}

// ---- 场景 2：离线补投 ----

func TestE2E_OfflineCatchUp(t *testing.T) {
	startFullServer(t)
	wsA := dialClientWS(t, serverURL(), "dev-A", testPassword, "init", "")

	// B 先连一次再断开
	wsB := dialClientWS(t, serverURL(), "dev-B", testPassword, "login", "")
	wsB.Close()

	// A 连发两条
	var seqs []int64
	for _, body := range []string{"offline-1", "offline-2"} {
		send(t, wsA, hub.Frame{V: 1, Type: "msg.send", ID: body,
			Data: json.RawMessage(`{"kind":"text","body":"` + body + `"}`)})
		ack := mustType(t, wsA, "msg.ack")
		seqs = append(seqs, parse[struct{ Seq int64 }](t, ack).Seq)
	}

	// B 重连 → sync 补投
	wsB2 := dialClientWS(t, serverURL(), "dev-B", testPassword, "login", "")
	send(t, wsB2, hub.Frame{V: 1, Type: "sync", ID: "s1",
		Data: json.RawMessage(`{"after":0,"limit":50}`)})
	done := mustType(t, wsB2, "sync.done")
	sd := parse[struct {
		Msgs    []msgData `json:"msgs"`
		HasMore bool      `json:"has_more"`
	}](t, done)
	if len(sd.Msgs) != 2 || sd.HasMore {
		t.Fatalf("sync.done 应含 2 条: %+v", sd)
	}
	if sd.Msgs[0].Body != "offline-1" || sd.Msgs[1].Body != "offline-2" {
		t.Fatalf("补投顺序错")
	}
}

// ---- 场景 3：多设备同一 role 同时在线，都能收到对端消息 ----

func TestE2E_MultiDeviceBroadcastToRole(t *testing.T) {
	startFullServer(t)
	wsA1 := dialClientWS(t, serverURL(), "dev-A1", testPassword, "init", "")
	// A1 已占 A，A2 必须显式指定 A
	wsA2 := dialClientWS(t, serverURL(), "dev-A2", testPassword, "login", "A")
	wsB := dialClientWS(t, serverURL(), "dev-B", testPassword, "login", "")

	// B 发一条，A1 和 A2 都应收到
	send(t, wsB, hub.Frame{V: 1, Type: "msg.send", ID: "from-b",
		Data: json.RawMessage(`{"kind":"text","body":"to-as"}`)})

	// 排空 A1 和 A2 各收到的一帧
	a1Frames := drainMsgs(t, wsA1, 3, "msg.new") // + ack 排空其他帧
	a2Frames := drainMsgs(t, wsA2, 3, "msg.new")
	if len(a1Frames) == 0 {
		t.Fatal("A1 应收到 msg.new")
	}
	if len(a2Frames) == 0 {
		t.Fatal("A2 应收到 msg.new")
	}
	m1 := parse[msgData](t, a1Frames[0])
	m2 := parse[msgData](t, a2Frames[0])
	if m1.Body != "to-as" || m2.Body != "to-as" {
		t.Fatalf("消息内容不符: A1=%v A2=%v", m1.Body, m2.Body)
	}
	if m1.Seq != m2.Seq {
		t.Fatalf("A1 和 A2 收到的 seq 应相同: %d vs %d", m1.Seq, m2.Seq)
	}
}

// ---- 场景 4：错误路径 ----

func TestE2E_ErrorFrames(t *testing.T) {
	startFullServer(t)
	wsA := dialClientWS(t, serverURL(), "dev-A", testPassword, "init", "")

	cases := []struct {
		name     string
		frame    string
		wantCode int
	}{
		{"未知帧类型", `{"v":1,"type":"hello"}`, 4004},
		{"空 body", `{"v":1,"type":"msg.send","data":{"kind":"text","body":""}}`, 4003},
		{"ack 不存在 seq", `{"v":1,"type":"msg.ack","data":{"seq":99999}}`, 4003},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = wsA.WriteMessage(websocket.TextMessage, []byte(tc.frame))
			f := mustType(t, wsA, "error")
			d := parse[struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}](t, f)
			if d.Code != tc.wantCode {
				t.Fatalf("错误码 %d != %d (%s)", d.Code, tc.wantCode, d.Message)
			}
		})
	}
}

// ---- helpers ----

func drainMsgs(t *testing.T, ws *websocket.Conn, total int, want string) []hub.Frame {
	t.Helper()
	out := []hub.Frame{}
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	for i := 0; i < total; i++ {
		var f hub.Frame
		if err := ws.ReadJSON(&f); err != nil {
			break
		}
		if f.Type == want {
			out = append(out, f)
		}
	}
	return out
}
