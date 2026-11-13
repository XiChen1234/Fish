package hub

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Frame 协议统一信封，见 proto/PROTOCOL.md §3。
type Frame struct {
	V    int             `json:"v"`
	Type string          `json:"type"`
	ID   string          `json:"id,omitempty"`
	TS   int64           `json:"ts,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Handler 业务帧处理器（阶段2实现消息逻辑）。
type Handler interface {
	Handle(c *Conn, f Frame)
}

// Conn 一个已鉴权的 WS 连接。Endpoint 为 "A" 或 "B"；
// DeviceID 是客户端生成的唯一设备标识（一个设备对应一个 token）。
type Conn struct {
	Endpoint  string // "A" / "B"
	DeviceID  string
	ws        *websocket.Conn
	send      chan Frame
	closeOnce sync.Once
}

// NewConn 创建一个已鉴权连接（api 层完成 WS 升级与 auth 握手后调用）。
func NewConn(endpoint, deviceID string, ws *websocket.Conn) *Conn {
	return &Conn{Endpoint: endpoint, DeviceID: deviceID, ws: ws, send: make(chan Frame, SendBufferCap)}
}

func (c *Conn) Send(f Frame) {
	defer func() {
		if r := recover(); r != nil {
			// channel 已关闭：连接已断开，忽略
		}
	}()
	select {
	case c.send <- f:
	default:
		log.Printf("[hub] endpoint=%s device=%s 发送队列满，断开连接", c.Endpoint, c.DeviceID)
		go c.Close()
	}
}

func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		close(c.send)
	})
}

// Hub 管理在线连接：身份 -> 连接集合（同一身份允许多端同时在线）。
type Hub struct {
	mu      sync.RWMutex
	conns   map[string]map[*Conn]struct{}
	handler Handler

	// PeerOf 身份->对端解析；LastSeq 当前最大消息 seq（阶段2接入存储后注入）。
	PeerOf  func(id string) (string, bool)
	LastSeq func() int64
}

func New(h Handler) *Hub {
	return &Hub{conns: make(map[string]map[*Conn]struct{}), handler: h}
}

// SetHandler 后置注入业务处理器（业务处理器与 Hub 存在互相引用）。
func (h *Hub) SetHandler(hd Handler) { h.handler = hd }

// sendReady 连接建立后的首帧，见协议 §4.2。
func (h *Hub) sendReady(c *Conn) {
	var peer string
	if h.PeerOf != nil {
		peer, _ = h.PeerOf(c.Endpoint)
	}
	var lastSeq int64
	if h.LastSeq != nil {
		lastSeq = h.LastSeq()
	}
	d, err := json.Marshal(struct {
		You        string `json:"you"`
		Peer       string `json:"peer"`
		PeerOnline bool   `json:"peer_online"`
		LastSeq    int64  `json:"last_seq"`
	}{c.Endpoint, peer, h.Online(peer), lastSeq})
	if err != nil {
		return
	}
	c.Send(Frame{V: 1, Type: "ready", TS: time.Now().UnixMilli(), Data: d})
}

func (h *Hub) Register(c *Conn) {
	h.mu.Lock()
	if h.conns[c.Endpoint] == nil {
		h.conns[c.Endpoint] = make(map[*Conn]struct{})
	}
	// 该身份由离线转为在线（首条连接）
	wentOnline := len(h.conns[c.Endpoint]) == 0
	h.conns[c.Endpoint][c] = struct{}{}
	h.mu.Unlock()

	if wentOnline {
		h.broadcastPresence(c.Endpoint, true)
	}
}

func (h *Hub) Unregister(c *Conn) {
	h.mu.Lock()
	set, ok := h.conns[c.Endpoint]
	if ok {
		delete(set, c)
	}
	// 该身份最后一条连接断开，转为离线
	wentOffline := ok && len(set) == 0
	if wentOffline {
		delete(h.conns, c.Endpoint)
	}
	h.mu.Unlock()

	if wentOffline {
		h.broadcastPresence(c.Endpoint, false)
	}
}

// broadcastPresence 向对端推送在线状态变化，见协议 §4.2 presence。
// 必须在释放 h.mu 后调用（SendTo 内部会再次加读锁）。
func (h *Hub) broadcastPresence(endpoint string, online bool) {
	if h.PeerOf == nil {
		return
	}
	peer, ok := h.PeerOf(endpoint)
	if !ok {
		return
	}
	d, err := json.Marshal(struct {
		Peer   string `json:"peer"`
		Online bool   `json:"online"`
	}{endpoint, online})
	if err != nil {
		return
	}
	h.SendTo(peer, Frame{V: 1, Type: "presence", TS: time.Now().UnixMilli(), Data: d})
}

// Online 身份是否至少有一条在线连接。
func (h *Hub) Online(endpoint string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns[endpoint]) > 0
}

// SendTo 向某身份的全部在线连接非阻塞投递帧。
func (h *Hub) SendTo(endpoint string, f Frame) {
	h.SendToExcept(endpoint, nil, f)
}

// SendToExcept 向某身份的全部在线连接投递帧，但跳过 except（通常为发送方自身连接）。
// 用于同角色多设备场景：本角色其他设备需要收到自己发出的消息。
func (h *Hub) SendToExcept(endpoint string, except *Conn, f Frame) {
	h.mu.RLock()
	conns := make([]*Conn, 0, len(h.conns[endpoint]))
	for c := range h.conns[endpoint] {
		if c == except {
			continue
		}
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	for _, c := range conns {
		c.Send(f)
	}
}

const (
	WriteWait     = 10 * time.Second
	PongWait      = 90 * time.Second // 与协议 §8 一致：90s 无读事件断开
	SendBufferCap = 64
)

// Run 连接主循环。鉴权（auth 帧）由 API 层在进入 Run 前完成，
// Run 内只负责 Register → 消息读循环 → Unregister。
// 首帧 ready 已废弃：所有首帧信息（role、peer_online、last_seq）
// 由 API 层在 auth 成功后通过 auth.result 一并下发。
func (h *Hub) Run(c *Conn) {
	h.Register(c)
	defer h.Unregister(c)

	done := make(chan struct{})
	go writePump(c, done)

	c.ws.SetReadLimit(1 << 20) // 帧上限 1MB（含图片引用元数据足够）
	_ = c.ws.SetReadDeadline(time.Now().Add(PongWait))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(PongWait))
	})

	for {
		var f Frame
		if err := c.ws.ReadJSON(&f); err != nil {
			break // 正常关闭或网络错误
		}
		_ = c.ws.SetReadDeadline(time.Now().Add(PongWait))
		h.dispatch(c, f)
	}
	c.Close()
	<-done
}

func (h *Hub) dispatch(c *Conn, f Frame) {
	switch f.Type {
	case "ping":
		c.Send(Frame{V: 1, Type: "pong"})
	default:
		if h.handler != nil {
			h.handler.Handle(c, f)
		}
	}
}

func writePump(c *Conn, done chan struct{}) {
	defer close(done)
	for f := range c.send {
		c.ws.SetWriteDeadline(time.Now().Add(WriteWait))
		if err := c.ws.WriteJSON(f); err != nil {
			return
		}
	}
	// send chan 关闭：发关闭帧后由读侧结束
	c.ws.SetWriteDeadline(time.Now().Add(WriteWait))
	_ = c.ws.WriteMessage(websocket.CloseMessage, []byte{})
}
