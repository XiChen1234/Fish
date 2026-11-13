// Package client Fish 客户端 SDK，供手机端 / Windows 端 / CLI agent 端复用。
// 基于帧内 auth 握手协议（init / login / resume）。
//
// 并发模型：
//   - 所有 WS 写入通过 sendCh 投递，由 writePump 单协程串行 WriteJSON，
//     满足 gorilla/websocket 的串行写入要求；
//   - readLoop 单协程读 WS，handleFrame 串行处理入站帧；
//   - SendText / SendImage / Sync / 自动 ack / pong 回复 / 心跳 ping 全部走 sendCh；
//   - pendingAck / pendingSync 是 Client 私有字段（带各自的 mutex），
//     多 Client 实例之间完全隔离，互不干扰。
package client

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	Seq     int64
	Peer    string
	Online  bool
	Code    int
	Text    string
}

// ReadyInfo 鉴权成功后首帧信息。
type ReadyInfo struct {
	Role       string // "A" / "B"
	Peer       string
	DeviceID   string
	Token      string
	PeerOnline bool
	LastSeq    int64 // 服务端当前最大 seq（仅作参考，客户端本地游标另存）
}

// Message 消息载荷（同步/实时同构）。
type Message struct {
	Seq    int64  `json:"seq"`
	From   string `json:"from"`
	Kind   string `json:"kind"`
	Body   string `json:"body,omitempty"`
	FileID string `json:"file_id,omitempty"`
	W      int    `json:"w,omitempty"`
	H      int    `json:"h,omitempty"`
	Size   int64  `json:"size,omitempty"`
	TS     int64  `json:"ts"`
}

// SyncResult 增量拉取结果。
type SyncResult struct {
	Msgs    []Message `json:"msgs"`
	HasMore bool      `json:"has_more"`
}

// ackWaiter 等待 msg.ack 的协程入口。
type ackWaiter struct {
	ch  chan ackResult
	ref string
}

type ackResult struct {
	Seq int64
	Err error
}

const (
	sendBufferCap = 64              // sendCh 容量
	pingInterval  = 25 * time.Second // 心跳，略短于服务端 90s 超时的 1/3
	writeWait     = 10 * time.Second // WriteJSON 单次超时
	ackTimeout    = 15 * time.Second // msg.send 等待 ack 超时
	syncTimeout   = 30 * time.Second // sync 等待超时
)

// Client SDK 主结构。非线程安全：
// Connect / Close 必须在其它方法调用之前 / 之后；
// Send / Sync / Upload 之间可并发（内部通过 sendCh 串行化）。
type Client struct {
	server  string
	httpCli *http.Client
	cb      func(Event)

	ws       *websocket.Conn
	sendCh   chan hub.Frame // write pump 输入
	closeCh  chan struct{}  // 全局关闭信号
	closeOnce sync.Once

	// 身份 & 状态
	deviceID string
	token    string
	role     string
	lastSeq  int64
	connected atomic.Bool

	// 等待器（Client 私有，多实例隔离）
	muAck      sync.Mutex
	pendingAck map[string]*ackWaiter
	muSync     sync.Mutex
	pendingSync map[string]chan *SyncResult
}

// New 构造一个未连接的 Client。server 不带末尾 /。
func New(server string, cb func(Event)) *Client {
	return &Client{
		server:      strings.TrimRight(server, "/"),
		cb:          cb,
		httpCli:     &http.Client{Timeout: 30 * time.Second},
		pendingAck:  make(map[string]*ackWaiter),
		pendingSync: make(map[string]chan *SyncResult),
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

// Connect 建立 WS + auth 握手。成功后 readLoop / writePump / ping 协程全部拉起，
// 返回后连接处于就绪状态（已过 auth 并 emit EventReady）。
func (c *Client) Connect(opts ConnectOptions) error {
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
	c.sendCh = make(chan hub.Frame, sendBufferCap)
	c.closeCh = make(chan struct{})
	c.connected.Store(true)

	// 发送 auth 帧
	authData, _ := json.Marshal(map[string]any{
		"device_id":    c.deviceID,
		"action":       string(opts.Action),
		"password":     opts.Password,
		"desired_role": opts.DesiredRole,
		"token":        c.token,
	})
	_ = ws.SetWriteDeadline(time.Now().Add(writeWait))
	if err := ws.WriteJSON(hub.Frame{V: 1, Type: "auth", TS: time.Now().UnixMilli(), Data: authData}); err != nil {
		ws.Close()
		c.connected.Store(false)
		return fmt.Errorf("发送 auth: %w", err)
	}
	_ = ws.SetWriteDeadline(time.Time{})

	_ = ws.SetReadDeadline(time.Now().Add(30 * time.Second))
	var f hub.Frame
	if err := ws.ReadJSON(&f); err != nil {
		ws.Close()
		c.connected.Store(false)
		return fmt.Errorf("读 auth.result: %w", err)
	}
	_ = ws.SetReadDeadline(time.Time{})
	if f.Type != "auth.result" {
		ws.Close()
		c.connected.Store(false)
		return fmt.Errorf("首帧应为 auth.result, got %s", f.Type)
	}
	var res authResult
	if err := json.Unmarshal(f.Data, &res); err != nil {
		ws.Close()
		c.connected.Store(false)
		return fmt.Errorf("auth.result 解析: %w", err)
	}
	if !res.Success {
		ws.Close()
		c.connected.Store(false)
		return errors.New(res.Error)
	}

	c.role = res.Role
	if res.DeviceID != "" {
		c.deviceID = res.DeviceID
	}
	c.token = res.Token

	// 就绪后拉起协程
	go c.writePump()
	go c.readLoop()

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
	return nil
}

// ---------- Close ----------

// Close 关闭连接。可重入，多次调用只生效一次。
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.connected.Store(false)
		close(c.closeCh)        // 通知 writePump / readLoop
		if c.ws != nil {
			_ = c.ws.Close()    // 强制断开，readLoop 会收到错误退出
		}
		c.emit(Event{Type: EventClosed})
	})
}

// ---------- writePump ----------

// writePump 串行化所有 WS 写入（业务帧 + 心跳），单协程 goroutine 安全。
// 退出条件：closeCh 关闭 或 WS 写入失败。
func (c *Client) writePump() {
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-c.closeCh:
			return

		case frame, ok := <-c.sendCh:
			if !ok {
				return // sendCh 被 close（当前实现不会手动 close，防御性分支）
			}
			if c.writeFrame(frame) {
				return // 写入失败，退出
			}

		case <-pingTicker.C:
			if c.writeFrame(hub.Frame{V: 1, Type: "ping"}) {
				return
			}
		}
	}
}

// writeFrame 单次 WriteJSON，返回 true 表示写入失败。
func (c *Client) writeFrame(f hub.Frame) bool {
	if c.ws == nil {
		return true
	}
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	err := c.ws.WriteJSON(f)
	_ = c.ws.SetWriteDeadline(time.Time{})
	if err != nil {
		log.Printf("[client] 写入失败 type=%s: %v", f.Type, err)
		return true
	}
	return false
}

// sendFrame 投递到 sendCh。若 channel 已满或已关闭（closedOnce）则丢弃，
// 返回 false 表示未投递成功。
func (c *Client) sendFrame(f hub.Frame) bool {
	if !c.connected.Load() {
		return false
	}
	select {
	case c.sendCh <- f:
		return true
	case <-c.closeCh:
		return false
	default:
		// 队列满（writePump 卡死），直接丢弃；readLoop / Close 会触发整体退出
		log.Printf("[client] sendCh 队列满，丢弃帧 type=%s", f.Type)
		return false
	}
}

// ---------- readLoop ----------

func (c *Client) readLoop() {
	defer func() {
		// 网络异常 / 服务端主动断开时触发；若已 Close() 则 closeOnce.Do 无效
		c.Close()
	}()

	for {
		select {
		case <-c.closeCh:
			return
		default:
		}
		if c.ws == nil {
			return
		}
		var f hub.Frame
		_ = c.ws.SetReadDeadline(time.Now().Add(120 * time.Second)) // 略长于 pingInterval*3
		if err := c.ws.ReadJSON(&f); err != nil {
			return // EOF / 超时 / 网络错误：退出，defer 会 Close()
		}
		c.handleFrame(f)
	}
}

func (c *Client) handleFrame(f hub.Frame) {
	switch f.Type {
	case "msg.new":
		var m Message
		if err := json.Unmarshal(f.Data, &m); err != nil {
			log.Printf("[client] msg.new 解析失败: %v", err)
			return
		}
		if m.Seq > atomic.LoadInt64(&c.lastSeq) {
			atomic.StoreInt64(&c.lastSeq, m.Seq)
		}
		// 自动回 ack（幂等），走 sendCh 串行化
		ackD, _ := json.Marshal(map[string]any{"seq": m.Seq})
		c.sendFrame(hub.Frame{V: 1, Type: "msg.ack", Data: ackD})
		c.emit(Event{Type: EventMessage, Message: &m})

	case "msg.ack":
		var d struct {
			Ref string `json:"ref"`
			Seq int64  `json:"seq"`
		}
		if err := json.Unmarshal(f.Data, &d); err != nil {
			return
		}
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
		if d.Ref != "" {
			c.resolvePendingAck(d.Ref, 0, errors.New(d.Message))
		}
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
			if m.Seq > atomic.LoadInt64(&c.lastSeq) {
				atomic.StoreInt64(&c.lastSeq, m.Seq)
			}
		}
		// 唤醒 waitForSync
		if f.ID != "" {
			c.muSync.Lock()
			ch, ok := c.pendingSync[f.ID]
			if ok {
				select {
				case ch <- &SyncResult{Msgs: d.Msgs, HasMore: d.HasMore}:
				default:
				}
				delete(c.pendingSync, f.ID)
			}
			c.muSync.Unlock()
		}

	case "ping":
		c.sendFrame(hub.Frame{V: 1, Type: "pong"})

	case "pong":
		// writePump 的心跳回包，忽略
	}
}

// ---------- SendText / SendImage ----------

// SendText 发送文本。返回服务端分配的 seq（>0）。
func (c *Client) SendText(body string) (int64, error) {
	if !c.Connected() {
		return 0, errors.New("未连接")
	}
	d, _ := json.Marshal(map[string]any{"kind": "text", "body": body})
	id := c.genRef()
	c.sendFrame(hub.Frame{V: 1, Type: "msg.send", ID: id, TS: time.Now().UnixMilli(), Data: d})
	return c.waitForAck(id)
}

// UploadImage HTTP 上传图片，返回 file_id、size（字节）。
func (c *Client) UploadImage(filename string, data []byte) (fileID string, size int64, err error) {
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
		Size   int64  `json:"size"`
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
	id := c.genRef()
	c.sendFrame(hub.Frame{V: 1, Type: "msg.send", ID: id, TS: time.Now().UnixMilli(), Data: d})
	return c.waitForAck(id)
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
	reqID := c.genRef()
	c.muSync.Lock()
	ch := make(chan *SyncResult, 1)
	c.pendingSync[reqID] = ch
	c.muSync.Unlock()

	if !c.sendFrame(hub.Frame{V: 1, Type: "sync", ID: reqID, TS: time.Now().UnixMilli(), Data: d}) {
		c.muSync.Lock()
		delete(c.pendingSync, reqID)
		c.muSync.Unlock()
		return nil, errors.New("发送 sync 失败（连接可能已关闭）")
	}
	return c.waitForSync(reqID)
}

// ---------- ack / sync 等待机制（Client 私有） ----------

func (c *Client) genRef() string {
	return "ref-" + strconv.FormatInt(time.Now().UnixNano(), 10)
}

func (c *Client) waitForAck(ref string) (int64, error) {
	ch := make(chan ackResult, 1)
	entry := &ackWaiter{ch: ch, ref: ref}
	c.muAck.Lock()
	c.pendingAck[ref] = entry
	c.muAck.Unlock()
	defer func() {
		c.muAck.Lock()
		delete(c.pendingAck, ref)
		c.muAck.Unlock()
	}()

	select {
	case r := <-ch:
		return r.Seq, r.Err
	case <-c.closeCh:
		return 0, errors.New("连接已关闭")
	case <-time.After(ackTimeout):
		return 0, errors.New("等待 msg.ack 超时")
	}
}

func (c *Client) resolvePendingAck(ref string, seq int64, err error) {
	if ref == "" {
		return
	}
	c.muAck.Lock()
	entry, ok := c.pendingAck[ref]
	c.muAck.Unlock()
	if !ok {
		return
	}
	select {
	case entry.ch <- ackResult{Seq: seq, Err: err}:
	default:
	}
}

func (c *Client) waitForSync(id string) (*SyncResult, error) {
	c.muSync.Lock()
	ch, ok := c.pendingSync[id]
	c.muSync.Unlock()
	if !ok {
		return nil, errors.New("sync 入口已失效")
	}
	select {
	case r := <-ch:
		return r, nil
	case <-c.closeCh:
		return nil, errors.New("连接已关闭")
	case <-time.After(syncTimeout):
		c.muSync.Lock()
		delete(c.pendingSync, id)
		c.muSync.Unlock()
		return nil, errors.New("等待 sync.done 超时")
	}
}

// ---------- 只读访问器 ----------

func (c *Client) Role() string    { return c.role }
func (c *Client) DeviceID() string { return c.deviceID }
func (c *Client) Token() string   { return c.token }
func (c *Client) Server() string  { return c.server }
func (c *Client) Connected() bool { return c.connected.Load() }
func (c *Client) LastSeq() int64  { return atomic.LoadInt64(&c.lastSeq) }

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

// multipartBody 简易 multipart/form-data writer，满足 http.Request.Body。
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
