package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gorilla/websocket"

	"fish/internal/hub"
)

// 生成 8x8 纯色 PNG（真实图片字节，可过嗅探）
func makePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func uploadFile(t *testing.T, ts *httptest.Server, token string, filename string, payload []byte) (int, []byte) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(payload); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/files", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// 上传 → 返回 file_id；下载 → 字节一致
func TestFileUploadDownload_RoundTrip(t *testing.T) {
	ts := newTestServer(t)
	png := makePNG(t)

	code, body := uploadFile(t, ts, "tok-a", "test.png", png)
	if code != http.StatusOK {
		t.Fatalf("上传应 200，got %d: %s", code, body)
	}
	var d struct {
		FileID string `json:"file_id"`
		Size   int    `json:"size"`
	}
	if err := json.Unmarshal(body, &d); err != nil || d.FileID == "" {
		t.Fatalf("上传响应异常: %s", body)
	}
	if d.Size != len(png) {
		t.Fatalf("size 不符: got %d want %d", d.Size, len(png))
	}

	// 下载（query token，模拟 img 标签）
	dl, err := http.Get(ts.URL + "/api/files/" + d.FileID + "?token=tok-b")
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("下载应 200，got %d", dl.StatusCode)
	}
	if ct := dl.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type 应为 image/png，got %s", ct)
	}
	got, _ := io.ReadAll(dl.Body)
	if !bytes.Equal(got, png) {
		t.Fatalf("下载内容与上传不一致")
	}

	// Bearer 头下载
	req, _ := http.NewRequest("GET", ts.URL+"/api/files/"+d.FileID, nil)
	req.Header.Set("Authorization", "Bearer tok-a")
	dl2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer dl2.Body.Close()
	if dl2.StatusCode != http.StatusOK {
		t.Fatalf("Bearer 下载应 200，got %d", dl2.StatusCode)
	}
}

// 非图片内容应被嗅探拒绝
func TestFileUpload_RejectNonImage(t *testing.T) {
	ts := newTestServer(t)

	code, body := uploadFile(t, ts, "tok-a", "evil.png", []byte("<?php echo 1; ?>"))
	if code != http.StatusBadRequest {
		t.Fatalf("非图片应 400，got %d: %s", code, body)
	}
}

// 未鉴权上传/下载应 401
func TestFileAuth(t *testing.T) {
	ts := newTestServer(t)

	code, _ := uploadFile(t, ts, "", "a.png", makePNG(t))
	if code != http.StatusUnauthorized {
		t.Fatalf("未鉴权上传应 401，got %d", code)
	}

	dl, err := http.Get(ts.URL + "/api/files/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	dl.Body.Close()
	if dl.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未鉴权下载应 401，got %d", dl.StatusCode)
	}
}

// 非法/不存在的 file_id
func TestFileDownload_InvalidAndMissing(t *testing.T) {
	ts := newTestServer(t)

	// 目录穿越尝试
	dl, err := http.Get(ts.URL + "/api/files/..%2f..%2fconfig.json?token=tok-a")
	if err != nil {
		t.Fatal(err)
	}
	dl.Body.Close()
	if dl.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 ID 应 400，got %d", dl.StatusCode)
	}

	// 合法 hex 但不存在
	dl2, err := http.Get(ts.URL + "/api/files/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa?token=tok-a")
	if err != nil {
		t.Fatal(err)
	}
	dl2.Body.Close()
	if dl2.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在文件应 404，got %d", dl2.StatusCode)
	}
}

// ---- E2E：A 上传图片 → 发 image 消息 → B 实时收到 → B 下载内容一致 ----

type msgDataT struct {
	Seq    int64  `json:"seq"`
	From   string `json:"from"`
	Kind   string `json:"kind"`
	Body   string `json:"body"`
	FileID string `json:"file_id"`
	W      int    `json:"w"`
	H      int    `json:"h"`
	Size   int64  `json:"size"`
}

func dialReady(t *testing.T, ts *httptest.Server, token string) *websocket.Conn {
	t.Helper()
	u := "ws" + stringsTrimHTTP(ts.URL) + "/ws?token=" + token
	ws, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("连接失败: %v (resp=%v)", err, resp)
	}
	t.Cleanup(func() { ws.Close() })
	if f := readFrame(t, ws); f.Type != "ready" {
		t.Fatalf("首帧应为 ready，got %s", f.Type)
	}
	return ws
}

func sendFrame(t *testing.T, ws *websocket.Conn, f hub.Frame) {
	t.Helper()
	if err := ws.WriteJSON(f); err != nil {
		t.Fatalf("发送帧失败: %v", err)
	}
}

func mustRecv(t *testing.T, ws *websocket.Conn, wantType string) hub.Frame {
	t.Helper()
	for {
		f := readFrame(t, ws)
		if f.Type == "presence" {
			continue // 在线状态推送与消息断言无关，跳过
		}
		if f.Type != wantType {
			t.Fatalf("期望 %s，实际 %s", wantType, f.Type)
		}
		return f
	}
}

func parseData[T any](t *testing.T, f hub.Frame) T {
	t.Helper()
	var d T
	if err := json.Unmarshal(f.Data, &d); err != nil {
		t.Fatalf("解析 %s.data 失败: %v (%s)", f.Type, err, f.Data)
	}
	return d
}

func stringsTrimHTTP(u string) string {
	if len(u) > 4 && u[:4] == "http" {
		return u[4:]
	}
	return u
}

func TestE2E_ImageMessageFlow(t *testing.T) {
	ts := newTestServer(t)
	wsA := dialReady(t, ts, "tok-a")
	wsB := dialReady(t, ts, "tok-b")
	png := makePNG(t)

	// 上传
	code, body := uploadFile(t, ts, "tok-a", "shot.png", png)
	if code != http.StatusOK {
		t.Fatalf("上传失败: %d %s", code, body)
	}
	var up struct {
		FileID string `json:"file_id"`
		Size   int    `json:"size"`
	}
	_ = json.Unmarshal(body, &up)

	// 发 image 消息
	payload := `{"kind":"image","file_id":"` + up.FileID + `","w":8,"h":8,"size":` + strconv.Itoa(up.Size) + `}`
	sendFrame(t, wsA, hub.Frame{V: 1, Type: "msg.send", ID: "i1", Data: json.RawMessage(payload)})

	_ = mustRecv(t, wsA, "msg.ack")
	m := parseData[msgDataT](t, mustRecv(t, wsB, "msg.new"))
	if m.Kind != "image" || m.FileID != up.FileID || m.W != 8 || m.H != 8 {
		t.Fatalf("image 消息数据异常: %+v", m)
	}

	// B 下载并校验内容
	dl, err := http.Get(ts.URL + "/api/files/" + m.FileID + "?token=tok-b")
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	got, _ := io.ReadAll(dl.Body)
	if !bytes.Equal(got, png) {
		t.Fatal("接收方下载内容与原图不一致")
	}
}

// 不存在的 file_id 发 image 消息应报错
func TestE2E_ImageMessage_MissingFile(t *testing.T) {
	ts := newTestServer(t)
	wsA := dialReady(t, ts, "tok-a")

	sendFrame(t, wsA, hub.Frame{V: 1, Type: "msg.send", ID: "i2",
		Data: json.RawMessage(`{"kind":"image","file_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)})
	f := mustRecv(t, wsA, "error")
	d := parseData[struct {
		Code int `json:"code"`
	}](t, f)
	if d.Code != 4003 {
		t.Fatalf("应 4003，got %d", d.Code)
	}
}
