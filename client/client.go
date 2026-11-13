// Package client Fish 客户端 SDK，供手机端 / Windows 端 / CLI agent 端复用。
// 基于帧内 auth 握手协议（init / login / resume）。
package client

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"fish/internal/hub"
)

// EventType 事件类型。
type EventType string

const (
	EventReady     EventType = "ready"     // auth 成功，收到首帧（含 role、peer_online、last_seq）
	EventMessage   EventType = "message"   // 收到 msg.new
	EventDelivered EventType = "delivered" // 对端已 ack 我方消息
	EventPresence  EventType = "presence"  // 对端在线状态变更
	EventError     EventType = "error"     // 服务端主动推送 error 帧
	EventClosed    EventType = "closed"    // WS 连接断开
)

// Event 上层事件。
type Event struct {
	Type    EventType
	Ready   *ReadyInfo
	Message *Message
	Seq     int64 // delivered / presence? 不适用
	Peer    string
	Online  bool
	Code    int
	Text    string
}

// ReadyInfo 鉴权成功后首帧信息。
type ReadyInfo struct {
	Role        string // "A" / "B"
	Peer        string
	DeviceID    string
	Token       string
	PeerOnline  bool
	LastSeq     int64 // 服务端当前最大 seq
}

// Message 消息载荷（同步/实时同构）。
type Message struct {
	Seq    int64  `json:"seq"`
	From   string `json:"from"`
	Kind   string `json:"kind"`
	Body   string `json:"body"`
	FileID string `json:"file_id,omitempty"`
	W      int    `json:"w,omitempty"`
	H      int    `json:"h,omitempty"`
	Size   int    `json:"size,omitempty"`
	TS     int64  `json:"ts"`
}

// SyncResult 增量拉取结果。
type SyncResult struct {
	Msgs    []Message `json:"msgs"`
	HasMore bool      `json:"has_more"`
}

// Client SDK 主结构。非线程安全：Send/Sync/Upload 之间可并发，
// 但 Connect/Close 必须在其它调用之前/之后。
type Client struct {
	server    string   // http(s)://host:port
	deviceID  string
	token     string   // JWT，auth 成功后填入
	role      string   // "A" / "B"
	lastSeq   int64    // 本地已收游标（服务端历史位置）
	cb        func(Event)
	ws        *websocket.Conn
	closed    bool
	closeOnce sync.Once
	mu        sync.Mutex
	httpCli   *http.Client
}

// New 构造一个未连接的 Client。server 不带末尾 /。
func New(server string, cb func(Event)) *Client {
	return &Client{
		server:  strings.TrimRight(server, "/"),
		cb:      cb,
		httpCli: &http.Client{Timeout: 30 * time.Second},
	}
}

// ---------- Connect ----------

// ConnectAction 鉴权动作。
type ConnectAction string

const (
	ActionInit  ConnectAction = "init"
	ActionLogin ConnectAction = "login"
	ActionResume ConnectAction = "resume"
)

// ConnectOptions 连接参数。deviceID 为空时自动生成；token 留空时 Resume 会自动用已有值。
type ConnectOptions struct {
	DeviceID    string
	Password    string
	Action      ConnectAction // init / login / resume
	DesiredRole string        // 留空自动分配；A、B 都存在时 login 必填
	Token       string        // resume 专用，优先级高于 Client.token
}

// Connect 建立 WS + auth 握手。成功后阻塞直到 Close 被调用。
// 内部逻辑：HTTP → WS 升级 → 发送 auth 帧 → 校验 auth.result 成功 → 进入 Hub.Run 循环（
// 在 SDK 这里就是 readLoop）。
func (c *Client) Connect(opts ConnectOptions) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if opts.Action == "" {
		return errors.New("必须指定 action：init / login / resume")
	}
	if opts.DeviceID != "" {
		c.deviceID = opts.DeviceID
	} else if c.deviceID == "" {
		c.deviceID = RandDeviceID()
	}
	if opts.Action == ActionResume {
		if opts.Token != "" {
			c.token = opts.Token
		}
		if c.token == "" {
			return errors.New("resume 需要 token")
		}
	}

	wsURL := "ws" + strings.TrimPrefix(c.server, "http") + "/ws"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("WS dial: %w", err)
	}
	c.ws = ws
	c.closed = false

	// 发送 auth 帧
	authData, _ := json.Marshal(map[string]any{
		"device_id":    c.deviceID,
		"action":       string(opts.Action),
		"password":     opts.Password,
		"desired_role": opts.DesiredRole,
		"token":        c.token,
	})
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := ws.WriteJSON(hub.Frame{V: 1, Type: "auth", TS: time.Now().UnixMilli(), Data: authData}); err != nil {
		ws.Close()
		return fmt.Errorf("发送 auth: %w", err)
	}
	_ = ws.SetWriteDeadline(time.Time{})

	_ = ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	var f hub.Frame
	if err := ws.ReadJSON(&f); err != nil {
		ws.Close()
		return fmt.Errorf("读 auth.result: %w", err)
	}
	_ = ws.SetReadDeadline(time.Time{})
	if f.Type != "auth.result" {
		ws.Close()
		return fmt.Errorf("首帧应为 auth.result, got %s", f.Type)
	}
	var res authResult
	if err := json.Unmarshal(f.Data, &res); err != nil {
		ws.Close()
		return fmt.Errorf("auth.result 解析: %w", err)
	}
	if !res.Success {
		ws.Close()
		return errors.New(res.Error)
	}

	c.role = res.Role
	if res.DeviceID != "" {
		c.deviceID = res.DeviceID
	}
	c.token = res.Token

	// 分发 Ready 事件
	c.emit(Event{
		Type: EventReady,
		Ready: &ReadyInfo{
			Role:       res.Role,
			Peer:       peerOf(res.Role),
			DeviceID:   c.deviceID,
			Token:      res.Token,
			PeerOnline: res.PeerOnline,
			LastSeq:    res.LastSeq,
		},
	})

	// 启动读循环
	go c.readLoop(ws)

	return nil
}

// Close 关闭连接。可重入。
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.closed = true
		if c.ws != nil {
			_ = c.ws.Close()
		}
		c.emit(Event{Type: EventClosed})
	})
}

// ---------- SendText / SendImage ----------

// SendText 发送文本。返回服务端分配的 seq（>0）。
func (c *Client) SendText(body string) (int64, error) {
	if !c.Connected() {
		return 0, errors.New("未连接")
	}
	d, _ := json.Marshal(map[string]any{"kind": "text", "body": body})
	id := "ref-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := c.ws.WriteJSON(hub.Frame{V: 1, Type: "msg.send", ID: id, TS: time.Now().UnixMilli(), Data: d}); err != nil {
		return 0, err
	}
	_ = c.ws.SetWriteDeadline(time.Time{})
	// 同步等 msg.ack
	ack, err := c.waitForAck(id)
	if err != nil {
		return 0, err
	}
	return ack, nil
}

// UploadImage HTTP 上传图片，返回 file_id。
func (c *Client) UploadImage(filename string, data []byte) (fileID string, size int, err error) {
	if c.token == "" {
		return "", 0, errors.New("未鉴权，无 token")
	}
	u := c.server + "/api/files"
	body := &multipartBody{data: data, filename: filename}
	req, _ := http.NewRequest("POST", u, body)
	req.Header.Set("Content-Type", body.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.httpCli.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("上传失败 %d: %s", resp.StatusCode, raw)
	}
	var res struct {
		FileID string `json:"file_id"`
		Size   int    `json:"size"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", 0, err
	}
	return res.FileID, res.Size, nil
}

// SendImage 先 UploadImage 再发 msg.send。
func (c *Client) SendImage(filename string, data []byte, w, h int) (int64, error) {
	fileID, size, err := c.UploadImage(filename, data)
	if err != nil {
		return 0, err
	}
	d, _ := json.Marshal(map[string]any{
		"kind": "image", "file_id": fileID, "w": w, "h": h, "size": size,
	})
	id := "ref-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := c.ws.WriteJSON(hub.Frame{V: 1, Type: "msg.send", ID: id, TS: time.Now().UnixMilli(), Data: d}); err != nil {
		return 0, err
	}
	_ = c.ws.SetWriteDeadline(time.Time{})
	ack, err := c.waitForAck(id)
	if err != nil {
		return 0, err
	}
	return ack, nil
}

// DownloadImage 按 file_id 下载字节。
func (c *Client) DownloadImage(fileID string) ([]byte, error) {
	if c.token == "" {
		return nil, errors.New("未鉴权")
	}
	u := c.server + "/api/files/" + url.PathEscape(fileID) + "?token=" + c.token
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载失败 %d", resp.StatusCode)
	}
	return raw, nil
}

// Sync 增量拉取 seq > after 的消息。limit 最大 200。hasMore 为 true 时继续拉。
func (c *Client) Sync(after int64, limit int) (*SyncResult, error) {
	if !c.Connected() {
		return nil, errors.New("未连接")
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	d, _ := json.Marshal(map[string]any{"after": after, "limit": limit})
	reqID := "sync-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := c.ws.WriteJSON(hub.Frame{V: 1, Type: "sync", ID: reqID, TS: time.Now().UnixMilli(), Data: d}); err != nil {
		return nil, err
	}
	_ = c.ws.SetWriteDeadline(time.Time{})
	return c.waitForSync(reqID)
}

// ---------- 只读访问器 ----------

func (c *Client) Role() string    { return c.role }
func (c *Client) DeviceID() string { return c.deviceID }
func (c *Client) Token() string   { return c.token }
func (c *Client) Server() string  { return c.server }
func (c *Client) Connected() bool { return c.ws != nil && !c.closed }

// ---------- readLoop ----------

func (c *Client) readLoop(ws *websocket.Conn) {
	defer func() {
		c.closeOnce.Do(func() {
			c.closed = true
			c.emit(Event{Type: EventClosed})
		})
	}()
	for {
		var f hub.Frame
		_ = ws.SetReadDeadline(time.Time{})
		if err := ws.ReadJSON(&f); err != nil {
			return
		}
		c.handleFrame(f)
	}
}

func (c *Client) handleFrame(f hub.Frame) {
	switch f.Type {
	case "msg.new":
		var m Message
		if err := json.Unmarshal(f.Data, &m); err != nil {
			return
		}
		if m.Seq > c.lastSeq {
			c.lastSeq = m.Seq
		}
		// 自动回 ack（幂等）
		ackD, _ := json.Marshal(map[string]any{"seq": m.Seq})
		_ = c.ws.WriteJSON(hub.Frame{V: 1, Type: "msg.ack", Data: ackD})
		c.emit(Event{Type: EventMessage, Message: &m})

	case "msg.ack":
		var d struct {
			Ref string `json:"ref"`
			Seq int64  `json:"seq"`
		}
		if err := json.Unmarshal(f.Data, &d); err != nil {
			return
		}
		// 让 SendText 等的 waitForAck 能返回
		c.resolvePendingAck(d.Ref, d.Seq, nil)

	case "msg.delivered":
		var d struct{ Seq int64 }
		if err := json.Unmarshal(f.Data, &d); err != nil {
			return
		}
		c.emit(Event{Type: EventDelivered, Seq: d.Seq})

	case "presence":
		var d struct {
			Peer   string `json:"peer"`
			Online bool   `json:"online"`
		}
		if err := json.Unmarshal(f.Data, &d); err != nil {
			return
		}
		c.emit(Event{Type: EventPresence, Peer: d.Peer, Online: d.Online})

	case "error":
		var d struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Ref     string `json:"ref"`
		}
		if err := json.Unmarshal(f.Data, &d); err != nil {
			return
		}
		c.resolvePendingAck(d.Ref, 0, errors.New(d.Message))
		c.emit(Event{Type: EventError, Code: d.Code, Text: d.Message})

	case "sync.done":
		var d struct {
			Msgs    []Message `json:"msgs"`
			HasMore bool      `json:"has_more"`
		}
		if err := json.Unmarshal(f.Data, &d); err != nil {
			return
		}
		// 更新游标
		for _, m := range d.Msgs {
			if m.Seq > c.lastSeq {
				c.lastSeq = m.Seq
			}
		}
		// 唤醒 waitForSync
		if f.ID != "" {
			if v, ok := pendingSync.Load(f.ID); ok {
				ch := v.(chan *SyncResult)
				select {
				case ch <- &SyncResult{Msgs: d.Msgs, HasMore: d.HasMore}:
				default:
				}
				pendingSync.Delete(f.ID)
			}
		}

	case "pong":
		// 忽略

	case "ping":
		// 服务端可能主动 ping 保活；回 pong
		_ = c.ws.WriteJSON(hub.Frame{V: 1, Type: "pong"})
	}
}

// ---------- ack / sync 等待机制 ----------

type ackWaiter struct {
	ch  chan ackResult
	ref string
}
type ackResult struct {
	Seq int64
	Err error
}

var pendingAck = sync.Map{} // ref → *ackWaiter

func (c *Client) waitForAck(ref string) (int64, error) {
	ch := make(chan ackResult, 1)
	entry := &ackWaiter{ch: ch, ref: ref}
	pendingAck.Store(ref, entry)
	defer pendingAck.Delete(ref)

	select {
	case r := <-ch:
		return r.Seq, r.Err
	case <-time.After(15 * time.Second):
		return 0, errors.New("等待 msg.ack 超时")
	}
}

func (c *Client) resolvePendingAck(ref string, seq int64, err error) {
	if ref == "" {
		return
	}
	v, ok := pendingAck.Load(ref)
	if !ok {
		return
	}
	entry := v.(*ackWaiter)
	select {
	case entry.ch <- ackResult{Seq: seq, Err: err}:
	default:
	}
}

var pendingSync = sync.Map{} // id → chan *SyncResult

func (c *Client) waitForSync(id string) (*SyncResult, error) {
	ch := make(chan *SyncResult, 1)
	pendingSync.Store(id, ch)
	defer pendingSync.Delete(id)

	// 复用 readLoop——但我们需要在 readLoop 里也处理 sync.done 帧
	// 最简单：让 readLoop 也 resolve pendingSync
	// 所以我们把这段放在 handleFrame 里处理。这里只是等。
	select {
	case r := <-ch:
		return r, nil
	case <-time.After(30 * time.Second):
		return nil, errors.New("等待 sync.done 超时")
	}
}

// ---------- emit ----------

func (c *Client) emit(e Event) {
	if c.cb != nil {
		defer func() { _ = recover() }()
		c.cb(e)
	}
}

// ---------- helpers ----------

type authResult struct {
	Success    bool   `json:"success"`
	Error      string `json:"error"`
	Role       string `json:"role"`
	DeviceID   string `json:"device_id"`
	Token      string `json:"token"`
	PeerOnline bool   `json:"peer_online"`
	LastSeq    int64  `json:"last_seq"`
}

func peerOf(role string) string {
	switch role {
	case "A":
		return "B"
	case "B":
		return "A"
	}
	return ""
}

// RandDeviceID 生成 16 字节随机 hex 字符串，用作首次连接的设备标识。
func RandDeviceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// multipartBody 简易 multipart.Part 包装，满足 http.Request.Body。
type multipartBody struct {
	data        []byte
	filename    string
	boundary    string
	headerBuilt bool
	head        []byte
	foot        []byte
	r           int
}

func (m *multipartBody) FormDataContentType() string {
	if m.boundary == "" {
		m.boundary = hex.EncodeToString(make([]byte, 16))
	}
	return "multipart/form-data; boundary=" + m.boundary
}

func (m *multipartBody) buildOnce() {
	if m.headerBuilt {
		return
	}
	m.headerBuilt = true
	if m.boundary == "" {
		m.boundary = hex.EncodeToString(make([]byte, 16))
	}
	m.head = []byte(fmt.Sprintf(
		"--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"%s\"\r\nContent-Type: application/octet-stream\r\n\r\n",
		m.boundary, m.filename))
	m.foot = []byte("\r\n--" + m.boundary + "--\r\n")
}

func (m *multipartBody) Read(p []byte) (int, error) {
	m.buildOnce()
	total := len(m.head) + len(m.data) + len(m.foot)
	if m.r >= total {
		return 0, io.EOF
	}
	var n int
	if m.r < len(m.head) {
		n = copy(p, m.head[m.r:])
		m.r += n
		return n, nil
	}
	if m.r < len(m.head)+len(m.data) {
		idx := m.r - len(m.head)
		n = copy(p, m.data[idx:])
		m.r += n
		return n, nil
	}
	idx := m.r - len(m.head) - len(m.data)
	n = copy(p, m.foot[idx:])
	m.r += n
	return n, nil
}
