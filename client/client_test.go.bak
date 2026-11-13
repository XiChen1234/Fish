package client_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"testing"
	"time"

	"fish/client"
	"fish/internal/api"
	"fish/internal/auth"
	"fish/internal/config"
	"fish/internal/hub"
	"fish/internal/msg"
	"fish/internal/store"
)

// makeTestPNG 生成真实 PNG 字节（可过服务端魔数嗅探）。
func makeTestPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for x := 0; x < 16; x++ {
		for y := 0; y < 16; y++ {
			img.Set(x, y, color.RGBA{B: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func startServer(t *testing.T) string {
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
		t.Fatal(err)
	}
	h := hub.New(nil)
	h.SetHandler(msg.New(st, h, a, cfg))
	h.LastSeq = func() int64 { return st.LastSeq() }
	s := api.New(cfg, a, h, st)
	ts := newTestSrv(t, s.Routes())
	t.Cleanup(func() { ts.Close(); st.Close() })
	return ts.URL
}

func newClient(t *testing.T, url, token string) *client.Client {
	t.Helper()
	c := client.New(url, token)
	if err := c.Connect(); err != nil {
		t.Fatalf("Connect 失败: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// collector 收集不匹配的事件供后续等待复用，避免事件被中途消费丢弃。
type collector struct {
	c   *client.Client
	buf []client.Event
}

func newCollector(c *client.Client) *collector { return &collector{c: c} }

func (col *collector) wait(t *testing.T, want client.EventType, timeout time.Duration) client.Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		for i, e := range col.buf {
			if e.Type == want {
				col.buf = append(col.buf[:i], col.buf[i+1:]...)
				return e
			}
		}
		select {
		case e := <-col.c.Events():
			if e.Type == want {
				return e
			}
			col.buf = append(col.buf, e)
		case <-deadline:
			t.Fatalf("等待事件 %s 超时", want)
		}
	}
}

// A、B 实时互发文本 + 送达回执
func TestClient_TextExchange(t *testing.T) {
	url := startServer(t)
	ca := newCollector(newClient(t, url, "tok-a"))
	cb := newCollector(newClient(t, url, "tok-b"))

	seq, err := ca.c.SendText("你好，B")
	if err != nil || seq <= 0 {
		t.Fatalf("SendText: seq=%d err=%v", seq, err)
	}

	e := cb.wait(t, client.EventMessage, 3*time.Second)
	if e.Message.Body != "你好，B" || e.Message.From != "a" || e.Message.Seq != seq {
		t.Fatalf("B 收到异常消息: %+v", e.Message)
	}

	// B 回复，A 接收
	seq2, err := cb.c.SendText("收到，A")
	if err != nil {
		t.Fatalf("B SendText: %v", err)
	}
	e2 := ca.wait(t, client.EventMessage, 3*time.Second)
	if e2.Message.Body != "收到，A" || e2.Message.Seq != seq2 {
		t.Fatalf("A 收到异常消息: %+v", e2.Message)
	}

	// AutoAck 后 A 应收到对首条消息的 delivered
	e3 := ca.wait(t, client.EventDelivered, 3*time.Second)
	if e3.Seq != seq {
		t.Fatalf("delivered seq 不符: got %d want %d", e3.Seq, seq)
	}
}

// 图片：上传 → 发送 → 对端下载内容一致
func TestClient_ImageExchange(t *testing.T) {
	url := startServer(t)
	ca := newCollector(newClient(t, url, "tok-a"))
	cb := newCollector(newClient(t, url, "tok-b"))

	payload := makeTestPNG(t)
	fileID, err := ca.c.UploadImage(payload, "pic.png")
	if err != nil {
		t.Fatalf("UploadImage: %v", err)
	}

	seq, err := ca.c.SendImage(fileID, 10, 10, int64(len(payload)))
	if err != nil || seq <= 0 {
		t.Fatalf("SendImage: seq=%d err=%v", seq, err)
	}

	e := cb.wait(t, client.EventMessage, 3*time.Second)
	if e.Message.Kind != "image" || e.Message.FileID != fileID {
		t.Fatalf("B 收到的图片消息异常: %+v", e.Message)
	}

	got, err := cb.c.DownloadImage(e.Message.FileID)
	if err != nil {
		t.Fatalf("DownloadImage: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("下载内容与上传不一致")
	}
}

// 离线补投 + lastSeq 游标幂等
func TestClient_SyncOffline(t *testing.T) {
	url := startServer(t)
	ca := newClient(t, url, "tok-a")

	// B 不连接，A 连发 3 条（全部落库）
	var wantBodies []string
	for _, s := range []string{"m1", "m2", "m3"} {
		if _, err := ca.SendText(s); err != nil {
			t.Fatalf("SendText %s: %v", s, err)
		}
		wantBodies = append(wantBodies, s)
	}

	cb := newClient(t, url, "tok-b")
	res, err := cb.Sync(0, 100)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Msgs) != 3 || res.HasMore {
		t.Fatalf("应补投 3 条: got %d hasMore=%v", len(res.Msgs), res.HasMore)
	}
	for i, m := range res.Msgs {
		if m.Body != wantBodies[i] {
			t.Fatalf("补投顺序错误: [%d]=%s want %s", i, m.Body, wantBodies[i])
		}
	}

	// 游标幂等：以 lastSeq 再 sync 应为空
	res2, err := cb.Sync(cb.LastSeq(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Msgs) != 0 {
		t.Fatalf("游标之后不应有新消息: %+v", res2.Msgs)
	}
}

// 错误路径：空文本
func TestClient_SendText_Empty(t *testing.T) {
	url := startServer(t)
	ca := newClient(t, url, "tok-a")

	if _, err := ca.SendText(""); err == nil {
		t.Fatal("空文本应报错")
	}
}

// S3 回归 1：对端上线/下线时，在线状态应实时推送（不依赖重连）
func TestClient_Presence(t *testing.T) {
	url := startServer(t)
	ca := newCollector(newClient(t, url, "tok-a"))

	// B 上线 → A 收到 presence {peer:"b", online:true}
	cb := newClient(t, url, "tok-b")
	on := ca.wait(t, client.EventPresence, 3*time.Second)
	if on.Peer != "b" || !on.Online {
		t.Fatalf("应收到 B 上线推送，got peer=%s online=%v", on.Peer, on.Online)
	}

	// B 断开 → A 收到 presence {peer:"b", online:false}
	if err := cb.Close(); err != nil {
		t.Fatal(err)
	}
	off := ca.wait(t, client.EventPresence, 5*time.Second)
	if off.Peer != "b" || off.Online {
		t.Fatalf("应收到 B 下线推送，got peer=%s online=%v", off.Peer, off.Online)
	}
}

// S3 回归 2：全新客户端（本地游标为 0，等价于新窗口）应能拉到全部历史，
// 不受 ready.last_seq 影响。
func TestClient_NewClientFullHistory(t *testing.T) {
	url := startServer(t)
	ca := newCollector(newClient(t, url, "tok-a"))

	var want []string
	for _, s := range []string{"offline-1", "offline-2"} {
		if _, err := ca.c.SendText(s); err != nil {
			t.Fatalf("SendText %s: %v", s, err)
		}
		want = append(want, s)
	}

	// 全新 B：本地游标必须为 0（不得被 ready.last_seq 覆盖）
	cb := newClient(t, url, "tok-b")
	if got := cb.LastSeq(); got != 0 {
		t.Fatalf("全新客户端本地游标应为 0，got %d", got)
	}
	if got := cb.ServerSeq(); got != 2 {
		t.Fatalf("服务端 last_seq 应为 2，got %d", got)
	}
	res, err := cb.Sync(0, 200)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Msgs) != len(want) {
		t.Fatalf("全新客户端应拉到 %d 条历史，got %d", len(want), len(res.Msgs))
	}
	for i, body := range want {
		if res.Msgs[i].Body != body {
			t.Fatalf("第 %d 条应为 %s，got %s", i, body, res.Msgs[i].Body)
		}
	}
}
