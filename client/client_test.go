package client_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
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

// newTestServer 起一个完整服务端（真实 api.Server），返回 httptest.Server URL。
func newTestServer(t *testing.T) (*httptest.Server, *store.Store, *hub.Hub) {
	t.Helper()
	dataDir := t.TempDir()
	cfg := &config.Config{
		DataDir:       dataDir,
		MaxTextBytes:  config.DefaultMaxTextBytes,
		MaxFileBytes:  config.DefaultMaxFileBytes,
	}
	st, err := store.Open(filepath.Join(dataDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	a := auth.New(st, []byte("test-jwt-secret-key-32bytes!!"))
	h := hub.New(nil)
	h.SetHandler(msg.New(st, h, cfg))
	h.LastSeq = func() int64 { return st.LastSeq() }

	srv := api.New(cfg, a, h, st)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts, st, h
}

func TestInitAndLogin(t *testing.T) {
	ts, _, _ := newTestServer(t)

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
	ts, _, _ := newTestServer(t)

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
	ts, _, _ := newTestServer(t)

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
	ts, _, _ := newTestServer(t)
	a := client.New(ts.URL, nil)
	_ = a.Connect(client.ConnectOptions{DeviceID: "dev-a", Password: "secret1234", Action: client.ActionInit})
	defer a.Close()

	b := client.New(ts.URL, nil)
	err := b.Connect(client.ConnectOptions{DeviceID: "dev-b", Password: "wrong", Action: client.ActionLogin})
	if err == nil {
		t.Fatal("坏密码应报错")
	}
}

// ---------- 新测试 ----------

// TestSync 验证 Sync 增量拉取：服务端落库后客户端可拉回历史消息。
func TestSync(t *testing.T) {
	ts, st, _ := newTestServer(t)

	a := client.New(ts.URL, nil)
	_ = a.Connect(client.ConnectOptions{DeviceID: "dev-a", Password: "secret1234", Action: client.ActionInit})
	defer a.Close()
	b := client.New(ts.URL, nil)
	_ = b.Connect(client.ConnectOptions{DeviceID: "dev-b", Password: "secret1234", Action: client.ActionLogin})
	defer b.Close()

	// B 发 3 条消息
	for i := 0; i < 3; i++ {
		seq, err := b.SendText("msg-" + itoa(i))
		if err != nil {
			t.Fatalf("SendText %d: %v", i, err)
		}
		if seq != int64(i+1) {
			t.Fatalf("期望 seq=%d, got %d", i+1, seq)
		}
	}

	// 直接查 store 确认已落库
	msgs, n, err := st.List(0, 10)
	if err != nil || n {
		t.Fatalf("store.List: err=%v hasMore=%v msgs=%d", err, n, len(msgs))
	}
	if len(msgs) != 3 {
		t.Fatalf("store 应有 3 条，got %d", len(msgs))
	}

	// A 还在线，直接 Sync(after:0) 应该拿到全部 3 条（hasMore=false）
	res, err := a.Sync(0, 10)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(res.Msgs) != 3 {
		t.Fatalf("Sync 应返回 3 条，got %d", len(res.Msgs))
	}
	if res.HasMore {
		t.Fatal("HasMore 应 false")
	}
	// 验证 seq 升序、内容正确
	for i, m := range res.Msgs {
		if m.Seq != int64(i+1) || m.From != "B" || m.Body != "msg-"+itoa(i) {
			t.Fatalf("Sync[%d]: %+v", i, m)
		}
	}

	// 再发 2 条，Sync(after:3) 只拿到新增 2 条
	for i := 3; i < 5; i++ {
		_, _ = b.SendText("msg-" + itoa(i))
	}
	time.Sleep(100 * time.Millisecond)
	res2, err := a.Sync(3, 10)
	if err != nil {
		t.Fatalf("Sync after:3: %v", err)
	}
	if len(res2.Msgs) != 2 {
		t.Fatalf("Sync after:3 应返回 2 条，got %d", len(res2.Msgs))
	}
	for i, m := range res2.Msgs {
		if m.Seq != int64(4+i) {
			t.Fatalf("Sync2[%d] seq 应为 %d, got %d", i, 4+i, m.Seq)
		}
	}
}

// TestConcurrentMultiDeviceSend 验证同一 role 多设备并发 SendText：
// Client 私有的 pendingAck 隔离，不会互相 resolve waiters。
func TestConcurrentMultiDeviceSend(t *testing.T) {
	ts, _, _ := newTestServer(t)

	b := client.New(ts.URL, nil)
	_ = b.Connect(client.ConnectOptions{DeviceID: "dev-b", Password: "secret1234", Action: client.ActionLogin})
	defer b.Close()

	// A1 init
	a1 := client.New(ts.URL, nil)
	_ = a1.Connect(client.ConnectOptions{DeviceID: "dev-a1", Password: "secret1234", Action: client.ActionInit})
	defer a1.Close()
	tokenA1 := a1.Token()

	// A2 用同一 token resume（多设备同 role A）
	a2 := client.New(ts.URL, nil)
	_ = a2.Connect(client.ConnectOptions{DeviceID: "dev-a2", Password: "", Action: client.ActionResume, Token: tokenA1})
	defer a2.Close()

	// A1 和 A2 并发各发 10 条
	var wg sync.WaitGroup
	a1Seqs := make([]int64, 10)
	a2Seqs := make([]int64, 10)
	a1Errs := make([]error, 10)
	a2Errs := make([]error, 10)

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			a1Seqs[i], a1Errs[i] = a1.SendText("a1-" + itoa(i))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			a2Seqs[i], a2Errs[i] = a2.SendText("a2-" + itoa(i))
		}
	}()
	wg.Wait()

	// 验证所有 SendText 都成功返回 seq
	for i, err := range a1Errs {
		if err != nil {
			t.Fatalf("a1[%d] err: %v", i, err)
		}
	}
	for i, err := range a2Errs {
		if err != nil {
			t.Fatalf("a2[%d] err: %v", i, err)
		}
	}
	// seq 应该各不相同（服务端分配）
	allSeqs := append(a1Seqs, a2Seqs...)
	seen := make(map[int64]bool)
	for _, s := range allSeqs {
		if seen[s] {
			t.Fatalf("重复 seq: %d", s)
		}
		seen[s] = true
	}
	if len(allSeqs) != 20 {
		t.Fatalf("期望 20 个 seq，got %d", len(allSeqs))
	}
}

// TestImageUploadAndDownload 走通 SendImage → msg.new → DownloadImage 全流程。
func TestImageUploadAndDownload(t *testing.T) {
	ts, _, _ := newTestServer(t)

	msgCh := make(chan *client.Message, 4)
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

	// 构造一个最小的合法 PNG（1x1 红色）
	pngData := []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xDE, 0x00, 0x00, 0x00, 0x0C, 0x49, 0x44, 0x41,
		0x54, 0x08, 0xD7, 0x63, 0xF8, 0xCF, 0xC0, 0x00,
		0x00, 0x00, 0x02, 0x00, 0x01, 0xE2, 0x21, 0xBC,
		0x33, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4E,
		0x44, 0xAE, 0x42, 0x60, 0x82,
	}

	// A 发图片消息给 B（SendImage 内部会 UploadImage + msg.send）
	seq, err := a.SendImage("pic.png", pngData, 1, 1)
	if err != nil {
		t.Fatalf("SendImage: %v", err)
	}
	if seq <= 0 {
		t.Fatalf("seq 应 >0, got %d", seq)
	}

	// B 收到图片消息（拿到 FileID 和 Size）
	var fileID string
	var size int64
	select {
	case m := <-msgCh:
		if m.Kind != "image" {
			t.Fatalf("Kind 不符 got %q want %q", m.Kind, "image")
		}
		if m.FileID == "" {
			t.Fatal("FileID 为空")
		}
		if m.Size != int64(len(pngData)) {
			t.Fatalf("Size 不符 got %d want %d", m.Size, len(pngData))
		}
		fileID = m.FileID
		size = m.Size
		t.Logf("msg.new: FileID=%q Size=%d", fileID, size)
	case <-time.After(3 * time.Second):
		t.Fatal("B 未收到图片消息")
	}

	// B 下载并比对
	got, err := b.DownloadImage(fileID)
	if err != nil {
		t.Fatalf("DownloadImage: %v", err)
	}
	if !bytes.Equal(got, pngData) {
		t.Fatalf("下载内容不符 got len=%d want len=%d", len(got), len(pngData))
	}
}

// TestPresenceAcked 验证 msg.delivered（B ack → A 收到 delivered 通知）。
func TestDeliveredAck(t *testing.T) {
	ts, _, _ := newTestServer(t)

	deliveredCh := make(chan int64, 4)
	cb := func(ev client.Event) {
		if ev.Type == client.EventDelivered {
			deliveredCh <- ev.Seq
		}
	}

	a := client.New(ts.URL, cb)
	_ = a.Connect(client.ConnectOptions{DeviceID: "dev-a", Password: "secret1234", Action: client.ActionInit})
	defer a.Close()
	b := client.New(ts.URL, nil) // B 不关心 delivered
	_ = b.Connect(client.ConnectOptions{DeviceID: "dev-b", Password: "secret1234", Action: client.ActionLogin})
	defer b.Close()

	// A 发给 B
	seq, err := a.SendText("hi")
	if err != nil {
		t.Fatalf("SendText: %v", err)
	}

	// B 收到后自动 ack（client readLoop handleFrame msg.new 里已经做了自动回 msg.ack）
	// 等待服务端把 msg.delivered 推回 A
	select {
	case gotSeq := <-deliveredCh:
		if gotSeq != seq {
			t.Fatalf("delivered seq 不符 got %d want %d", gotSeq, seq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未收到 msg.delivered")
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// 确保 json 包被测试引用（上传用），避免 unused import
var _ = json.Marshal
var _ = http.StatusOK
