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
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"fish/internal/config"
	"fish/internal/hub"
)

// makeJWT 先 init 一台设备拿到有效 JWT，HTTP Bearer 用。
func makeJWT(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	authData, _ := json.Marshal(map[string]any{
		"device_id": "file-test-" + strings.ReplaceAll(t.Name(), "/", "-"),
		"password":  "secret1234", "action": "init",
	})
	_ = ws.WriteJSON(hub.Frame{V: 1, Type: "auth", Data: authData})
	var f hub.Frame
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_ = ws.ReadJSON(&f)
	var res struct {
		Success bool   `json:"success"`
		Token   string `json:"token"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(f.Data, &res)
	if !res.Success {
		t.Fatalf("auth 失败: %s", res.Error)
	}
	return res.Token
}

// makeJWT2 先 init A，再 login B，返回 (jwtA, jwtB)。
func makeJWT2(t *testing.T, ts *httptest.Server) (string, string) {
	t.Helper()
	jwtA := makeJWT(t, ts)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	authData, _ := json.Marshal(map[string]any{
		"device_id": "file-test-b-" + strings.ReplaceAll(t.Name(), "/", "-"),
		"password":  "secret1234", "action": "login",
	})
	_ = ws.WriteJSON(hub.Frame{V: 1, Type: "auth", Data: authData})
	var f hub.Frame
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_ = ws.ReadJSON(&f)
	var res struct {
		Success bool   `json:"success"`
		Token   string `json:"token"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(f.Data, &res)
	if !res.Success {
		t.Fatalf("auth login 失败: %s", res.Error)
	}
	return jwtA, res.Token
}

// dialWSReady WS 握手并 auth，返回已就绪连接（调用方 close）。
func dialWSReady(t *testing.T, ts *httptest.Server, deviceID, password, action string) *websocket.Conn {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	authData, _ := json.Marshal(map[string]any{
		"device_id": deviceID, "password": password, "action": action,
	})
	_ = ws.WriteJSON(hub.Frame{V: 1, Type: "auth", Data: authData})
	var f hub.Frame
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_ = ws.ReadJSON(&f)
	var res struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(f.Data, &res)
	if !res.Success {
		t.Fatalf("auth 失败: %s", res.Error)
	}
	return ws
}

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

// TestFileUploadDownload_RoundTrip 上传 → 返回 file_id；下载 → 字节一致。
func TestFileUploadDownload_RoundTrip(t *testing.T) {
	ts := newTestServer(t)
	tok := makeJWT(t, ts)
	png := makePNG(t)

	code, body := uploadFile(t, ts, tok, "test.png", png)
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
		t.Fatalf("size 不符: %d vs %d", d.Size, len(png))
	}

	// 下载（query token 方式，模拟 img 标签）
	dl, err := http.Get(ts.URL + "/api/files/" + d.FileID + "?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	got, _ := io.ReadAll(dl.Body)
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("下载应 200，got %d", dl.StatusCode)
	}
	if !bytes.Equal(got, png) {
		t.Fatalf("下载字节不一致")
	}
}

// TestFileUpload_RejectNonImage 非图片上传被嗅探拒绝。
func TestFileUpload_RejectNonImage(t *testing.T) {
	ts := newTestServer(t)
	tok := makeJWT(t, ts)
	code, body := uploadFile(t, ts, tok, "evil.png", []byte("<?php echo 1; ?>"))
	if code != http.StatusBadRequest {
		t.Fatalf("非图片应 400，got %d: %s", code, body)
	}
}

// TestFileUpload_RejectOversize 超过上限的文件必须整体拒绝，
// 不能被 LimitReader 静默截断后当作合法图片落盘。
func TestFileUpload_RejectOversize(t *testing.T) {
	ts := newTestServer(t)
	tok := makeJWT(t, ts)

	// 保留真实 PNG 魔数，确保先通过类型嗅探，再在大小校验处被拒
	payload := make([]byte, int(config.DefaultMaxFileBytes)+1)
	copy(payload, makePNG(t))

	code, body := uploadFile(t, ts, tok, "big.png", payload)
	if code != http.StatusBadRequest {
		t.Fatalf("超限上传应 400，got %d: %s", code, body)
	}
	if !strings.Contains(string(body), "大小上限") {
		t.Fatalf("错误信息应提示大小上限，got: %s", body)
	}
}

// TestFileAuth 401 路径：未带 token 上传应被拒绝。
func TestFileAuth(t *testing.T) {
	ts := newTestServer(t)
	// 先初始化房间让 store ready（HTTP 文件鉴权只验 token 签名，不关心房间状态）
	_ = makeJWT(t, ts)

	// 不带 token
	code, _ := uploadFile(t, ts, "", "x.png", makePNG(t))
	if code != http.StatusUnauthorized {
		t.Fatalf("未鉴权应 401，got %d", code)
	}

	// 带错误 token
	req, _ := http.NewRequest("GET", ts.URL+"/api/files/zzzz?token=wrong-token", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误 token 应 401，got %d", resp.StatusCode)
	}
}

// TestFileDownload_InvalidAndMissing 非法 file_id 和不存在文件。
func TestFileDownload_InvalidAndMissing(t *testing.T) {
	ts := newTestServer(t)
	tok := makeJWT(t, ts)

	// 目录穿越 / 非法字符
	dl, err := http.Get(ts.URL + "/api/files/..%2f..%2fconfig.json?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusBadRequest {
		t.Fatalf("非法 ID 应 400，got %d", dl.StatusCode)
	}

	// 不存在
	dl2, err := http.Get(ts.URL + "/api/files/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa?token=" + tok)
	if err != nil {
		t.Fatal(err)
	}
	defer dl2.Body.Close()
	if dl2.StatusCode != http.StatusNotFound {
		t.Fatalf("不存在应 404，got %d", dl2.StatusCode)
	}
}

// TestE2E_ImageMessageFlow 发送方上传 → msg.send 带 kind=image → 接收方收到 → 下载。
func TestE2E_ImageMessageFlow(t *testing.T) {
	ts := newTestServer(t)
	png := makePNG(t)

	wsA := dialWSReady(t, ts, "e2e-img-a", "secret1234", "init")
	defer wsA.Close()
	wsB := dialWSReady(t, ts, "e2e-img-b", "secret1234", "login")
	defer wsB.Close()

	// A 先上传拿 file_id
	uploadTok := makeJWTLogin(t, ts, "e2e-img-upload-extra")
	code, body := uploadFile(t, ts, uploadTok, "shot.png", png)
	if code != http.StatusOK {
		t.Fatalf("上传应 200，got %d: %s", code, body)
	}
	var upRes struct {
		FileID string `json:"file_id"`
		Size   int    `json:"size"`
	}
	_ = json.Unmarshal(body, &upRes)

	// A 发送 image 消息
	sendData, _ := json.Marshal(map[string]any{
		"kind":    "image",
		"file_id": upRes.FileID,
		"w": 8, "h": 8, "size": upRes.Size,
	})
	_ = wsA.WriteJSON(hub.Frame{V: 1, Type: "msg.send", ID: "img-1", Data: sendData})
	// 排空 ack（A）和 msg.new（B）
	var bMsg hub.Frame
	for i := 0; i < 3; i++ {
		f := readFrameWS(t, wsB)
		if f.Type == "msg.new" {
			bMsg = f
			break
		}
	}
	if bMsg.Type != "msg.new" {
		t.Fatal("B 应收到 msg.new")
	}
	var m struct {
		Kind   string `json:"kind"`
		FileID string `json:"file_id"`
	}
	_ = json.Unmarshal(bMsg.Data, &m)
	if m.Kind != "image" || m.FileID != upRes.FileID {
		t.Fatalf("消息字段不符: %+v", m)
	}

	// B 下载
	dl, err := http.Get(ts.URL + "/api/files/" + m.FileID + "?token=" + uploadTok)
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Body.Close()
	got, _ := io.ReadAll(dl.Body)
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("下载应 200，got %d", dl.StatusCode)
	}
	if !bytes.Equal(got, png) {
		t.Fatal("下载字节不一致")
	}
}

func makeJWTLogin(t *testing.T, ts *httptest.Server, deviceID string) string {
	t.Helper()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	authData, _ := json.Marshal(map[string]any{
		"device_id":    deviceID, "password": "secret1234", "action": "login",
		"desired_role": "B", // A 角色由 init 设备占位，新登录设备默认 B
	})
	_ = ws.WriteJSON(hub.Frame{V: 1, Type: "auth", Data: authData})
	var f hub.Frame
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	_ = ws.ReadJSON(&f)
	var res struct {
		Success bool   `json:"success"`
		Token   string `json:"token"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(f.Data, &res)
	if !res.Success {
		t.Fatalf("auth 失败: %s", res.Error)
	}
	return res.Token
}

func readFrameWS(t *testing.T, ws *websocket.Conn) hub.Frame {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var f hub.Frame
	if err := ws.ReadJSON(&f); err != nil {
		t.Fatalf("读帧失败: %v", err)
	}
	_ = ws.SetReadDeadline(time.Time{})
	return f
}
