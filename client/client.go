// Package client 是 Fish 协议 v1 的平台无关适配库（自包含，仅依赖 WebSocket 实现）。
//
// 任何 Go 宿主（桌面端、agent、手机端经 gomobile）可直接复用；
// 其他语言按 proto/PROTOCOL.md 实现同等能力即可。
package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Frame 协议信封，与 proto/PROTOCOL.md §3 一致。
type Frame struct {
	V    int             `json:"v"`
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"`
	TS   int64           `json:"ts,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Message 收到的消息载荷（msg.new / sync 条目）。
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

// ReadyInfo ready 帧载荷。
type ReadyInfo struct {
	You        string `json:"you"`
	Peer       string `json:"peer"`
	PeerOnline bool   `json:"peer_online"`
	LastSeq    int64  `json:"last_seq"`
}

// EventType 事件种类。
type EventType string

const (
	EventReady     EventType = "ready"     // 连接就绪
	EventMessage   EventType = "message"   // 收到新消息（含实时与补投）
	EventDelivered EventType = "delivered" // 我发的消息被对端确认
	EventPresence  EventType = "presence"  // 对端在线状态变化
	EventError     EventType = "error"     // 服务端 error 帧
	EventClosed    EventType = "closed"    // 连接断开
)

type Event struct {
	Type    EventType
	Ready   *ReadyInfo
	Message *Message
	Seq     int64  // delivered 的 seq
	Peer    string // presence 的对端身份
	Online  bool   // presence 的在线状态
	Code    int    // error 码
	Text    string
}

const (
	pingInterval  = 30 * time.Second
	replyTimeout  = 10 * time.Second
	syncChunkSize = 200
)

// Client 单端协议适配器。一个实例对应一个固定身份（token 决定）。
type Client struct {
	serverURL string // 如 http://host:8080
	token     string

	// AutoAck 收到 msg.new 后是否自动回 msg.ack（默认 true）
	AutoAck bool

	ws      *websocket.Conn
	wsMu    sync.Mutex
	events  chan Event
	pending sync.Map // ref -> chan Frame
	lastSeq int64    // 本地已收到的最大 seq（本地游标，用于 sync.after）
	serverSeq int64  // 服务端最新 seq（仅用于判断是否落后，不可当本地游标）
	seqMu   sync.Mutex
	refSeq  atomic.Int64
	closed  chan struct{}
	closed1 sync.Once
	http    *http.Client
}

func New(serverURL, token string) *Client {
	return &Client{
		serverURL: strings.TrimRight(serverURL, "/"),
		token:     token,
		AutoAck:   true,
		events:    make(chan Event, 64),
		closed:    make(chan struct{}),
		http:      &http.Client{Timeout: 60 * time.Second},
	}
}

// Events 事件流。Connect 前调用即可，事件按序投递。
func (c *Client) Events() <-chan Event { return c.events }

// LastSeq 本地已见最大 seq（本地游标，用于断线后 sync.after 增量补投）。
// 注意：该值只随实际收到的消息推进，不会被 ready.last_seq 覆盖。
func (c *Client) LastSeq() int64 {
	c.seqMu.Lock()
	defer c.seqMu.Unlock()
	return c.lastSeq
}

// ServerSeq 服务端当前最大 seq（连接建立时由 ready 帧给出），
// 仅用于判断本地是否落后，禁止用作 sync.after。
func (c *Client) ServerSeq() int64 {
	c.seqMu.Lock()
	defer c.seqMu.Unlock()
	return c.serverSeq
}

func (c *Client) setLastSeq(v int64) {
	c.seqMu.Lock()
	if v > c.lastSeq {
		c.lastSeq = v
	}
	c.seqMu.Unlock()
}

// Connect 建立 WS 连接并等待 ready 帧。
func (c *Client) Connect() error {
	wsURL := wsScheme(c.serverURL) + "/ws?token=" + c.token
	ws, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("连接失败: %w (http=%v)", err, statusOf(resp))
	}
	c.ws = ws
	c.closed = make(chan struct{})

	var f Frame
	_ = ws.SetReadDeadline(time.Now().Add(replyTimeout))
	if err := ws.ReadJSON(&f); err != nil {
		return fmt.Errorf("等待 ready 失败: %w", err)
	}
	if f.Type != "ready" {
		return fmt.Errorf("首帧类型异常: %s", f.Type)
	}
	_ = ws.SetReadDeadline(time.Time{}) // 清除 ready 等待超时，恢复长连接读
	var ri ReadyInfo
	_ = json.Unmarshal(f.Data, &ri)
	// ready.last_seq 是服务端游标，不是本地游标；仅记录，绝不覆盖 lastSeq
	c.seqMu.Lock()
	c.serverSeq = ri.LastSeq
	c.seqMu.Unlock()
	c.emit(Event{Type: EventReady, Ready: &ri})

	go c.readLoop(ws)
	go c.heartbeat()
	return nil
}

func (c *Client) readLoop(ws *websocket.Conn) {
	for {
		var f Frame
		if err := ws.ReadJSON(&f); err != nil {
			c.emit(Event{Type: EventClosed, Text: err.Error()})
			c.shutdown()
			return
		}
		c.handleFrame(f)
	}
}

func (c *Client) handleFrame(f Frame) {
	switch f.Type {
	case "msg.new":
		var m Message
		if json.Unmarshal(f.Data, &m) != nil {
			return
		}
		// seq 幂等去重
		if m.Seq <= c.LastSeq() {
			return
		}
		c.setLastSeq(m.Seq)
		c.emit(Event{Type: EventMessage, Message: &m})
		if c.AutoAck {
			_ = c.write(Frame{V: 1, Type: "msg.ack", Data: mustJSON(map[string]int64{"seq": m.Seq})})
		}
	case "msg.delivered":
		var d struct {
			Seq int64 `json:"seq"`
		}
		if json.Unmarshal(f.Data, &d) == nil {
			c.emit(Event{Type: EventDelivered, Seq: d.Seq})
		}
	case "presence":
		var d struct {
			Peer   string `json:"peer"`
			Online bool   `json:"online"`
		}
		if json.Unmarshal(f.Data, &d) == nil {
			c.emit(Event{Type: EventPresence, Peer: d.Peer, Online: d.Online})
		}
	case "error":
		var e struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Ref     string `json:"ref"`
		}
		if json.Unmarshal(f.Data, &e) != nil {
			return
		}
		if e.Ref != "" {
			if ch, ok := c.pending.LoadAndDelete(e.Ref); ok {
				ch.(chan Frame) <- f
				return
			}
		}
		c.emit(Event{Type: EventError, Code: e.Code, Text: e.Message})
	default:
		// msg.ack / sync.done / pong 等请求-响应帧，交给 pending
		if f.ID != "" {
			if ch, ok := c.pending.Load(f.ID); ok {
				select {
				case ch.(chan Frame) <- f:
				default:
				}
			}
		}
	}
}

func (c *Client) heartbeat() {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = c.write(Frame{V: 1, Type: "ping"})
		case <-c.closed:
			return
		}
	}
}

func (c *Client) write(f Frame) error {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	if c.ws == nil {
		return fmt.Errorf("未连接")
	}
	return c.ws.WriteJSON(f)
}

// call 发送请求帧并等待对应响应（按 ID 关联）。
func (c *Client) call(f Frame, timeout time.Duration) (Frame, error) {
	ref := fmt.Sprintf("c%d-%d", time.Now().UnixNano(), c.refSeq.Add(1))
	f.ID = ref
	ch := make(chan Frame, 1)
	c.pending.Store(ref, ch)
	defer c.pending.Delete(ref)

	if err := c.write(f); err != nil {
		return Frame{}, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		return Frame{}, fmt.Errorf("等待 %s 响应超时", f.Type)
	case <-c.closed:
		return Frame{}, fmt.Errorf("连接已断开")
	}
}

// SendText 发送文本消息，返回服务端分配的 seq。
func (c *Client) SendText(body string) (int64, error) {
	resp, err := c.call(Frame{V: 1, Type: "msg.send",
		Data: mustJSON(map[string]string{"kind": "text", "body": body})}, replyTimeout)
	if err != nil {
		return 0, err
	}
	if resp.Type == "error" {
		return 0, decodeError(resp)
	}
	var d struct {
		Seq int64 `json:"seq"`
	}
	if json.Unmarshal(resp.Data, &d) != nil || d.Seq <= 0 {
		return 0, fmt.Errorf("msg.ack 响应异常")
	}
	c.setLastSeq(d.Seq)
	return d.Seq, nil
}

// UploadImage 上传图片字节，返回 file_id。
func (c *Client) UploadImage(data []byte, filename string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(data); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequest("POST", c.serverURL+"/api/files", &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("上传失败 http=%d: %s", resp.StatusCode, truncate(body, 200))
	}
	var d struct {
		FileID string `json:"file_id"`
	}
	if json.Unmarshal(body, &d) != nil || d.FileID == "" {
		return "", fmt.Errorf("上传响应异常")
	}
	return d.FileID, nil
}

// SendImage 发送图片消息（file_id 需先经 UploadImage 获得）。
func (c *Client) SendImage(fileID string, w, h int, size int64) (int64, error) {
	resp, err := c.call(Frame{V: 1, Type: "msg.send",
		Data: mustJSON(map[string]any{"kind": "image", "file_id": fileID, "w": w, "h": h, "size": size})}, replyTimeout)
	if err != nil {
		return 0, err
	}
	if resp.Type == "error" {
		return 0, decodeError(resp)
	}
	var d struct {
		Seq int64 `json:"seq"`
	}
	if json.Unmarshal(resp.Data, &d) != nil || d.Seq <= 0 {
		return 0, fmt.Errorf("msg.ack 响应异常")
	}
	c.setLastSeq(d.Seq)
	return d.Seq, nil
}

// SyncResult 增量拉取结果。
type SyncResult struct {
	Msgs    []Message
	HasMore bool
}

// Sync 拉取 seq > after 的历史消息（内部自动翻页至拉完或 limit 上限）。
func (c *Client) Sync(after int64, limit int) (*SyncResult, error) {
	if limit <= 0 || limit > syncChunkSize {
		limit = syncChunkSize
	}
	out := &SyncResult{}
	for len(out.Msgs) < limit {
		resp, err := c.call(Frame{V: 1, Type: "sync",
			Data: mustJSON(map[string]int64{"after": after, "limit": syncChunkSize})}, replyTimeout)
		if err != nil {
			return nil, err
		}
		if resp.Type == "error" {
			return nil, decodeError(resp)
		}
		var d struct {
			Msgs    []Message `json:"msgs"`
			HasMore bool      `json:"has_more"`
		}
		if json.Unmarshal(resp.Data, &d) != nil {
			return nil, fmt.Errorf("sync.done 响应异常")
		}
		out.Msgs = append(out.Msgs, d.Msgs...)
		out.HasMore = d.HasMore
		if len(d.Msgs) == 0 {
			break
		}
		after = d.Msgs[len(d.Msgs)-1].Seq
		if !d.HasMore {
			break
		}
	}
	if len(out.Msgs) > limit {
		out.Msgs = out.Msgs[:limit]
	}
	for _, m := range out.Msgs {
		c.setLastSeq(m.Seq)
	}
	return out, nil
}

// DownloadImage 下载图片字节。
func (c *Client) DownloadImage(fileID string) ([]byte, error) {
	req, _ := http.NewRequest("GET", c.serverURL+"/api/files/"+fileID, nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下载失败 http=%d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// ImageURL 供 UI 直接引用的下载地址（带 token 查询参数）。
func (c *Client) ImageURL(fileID string) string {
	return c.serverURL + "/api/files/" + fileID + "?token=" + c.token
}

func (c *Client) emit(e Event) {
	select {
	case c.events <- e:
	default: // 事件队列满时丢弃最旧的事件，防止阻塞读循环
		select {
		case <-c.events:
		default:
		}
		select {
		case c.events <- e:
		default:
		}
	}
}

func (c *Client) shutdown() {
	c.closed1.Do(func() { close(c.closed) })
}

// Close 断开连接。
func (c *Client) Close() error {
	c.shutdown()
	c.wsMu.Lock()
	ws := c.ws
	c.ws = nil
	c.wsMu.Unlock()
	if ws != nil {
		return ws.Close()
	}
	return nil
}

// ---- helpers ----

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func decodeError(f Frame) error {
	var e struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(f.Data, &e)
	return fmt.Errorf("服务端错误 %d: %s", e.Code, e.Message)
}

func wsScheme(u string) string {
	if strings.HasPrefix(u, "https://") {
		return "wss://" + strings.TrimPrefix(u, "https://")
	}
	if strings.HasPrefix(u, "ws://") {
		return u
	}
	return "ws://" + strings.TrimPrefix(u, "http://")
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
