package msg

import (
	"encoding/json"
	"log"
	"time"

	"fish/internal/auth"
	"fish/internal/config"
	"fish/internal/hub"
	"fish/internal/store"
)

// 错误码，见协议 §4.3
const (
	CodeBadRequest  = 4003
	CodeUnknownType = 4004
	CodeInternal    = 5000
)

const (
	defaultSyncLimit = 100
	maxSyncLimit     = 200
)

// Handler 业务帧处理器：消息落库、路由转发、增量同步。
type Handler struct {
	store *store.Store
	hub   *hub.Hub
	cfg   *config.Config
}

func New(st *store.Store, h *hub.Hub, cfg *config.Config) *Handler {
	return &Handler{store: st, hub: h, cfg: cfg}
}

func (h *Handler) Handle(c *hub.Conn, f hub.Frame) {
	switch f.Type {
	case "msg.send":
		h.onSend(c, f)
	case "msg.ack":
		h.onAck(c, f)
	case "sync":
		h.onSync(c, f)
	default:
		h.sendError(c, CodeUnknownType, "不支持的帧类型: "+f.Type, f.ID)
	}
}

// ---- msg.send ----

type sendReq struct {
	Kind   string `json:"kind"`
	Body   string `json:"body"`
	FileID string `json:"file_id"`
	W      int    `json:"w"`
	H      int    `json:"h"`
	Size   int64  `json:"size"`
}

func (h *Handler) onSend(c *hub.Conn, f hub.Frame) {
	var req sendReq
	if err := json.Unmarshal(f.Data, &req); err != nil {
		h.sendError(c, CodeBadRequest, "msg.send.data 解析失败", f.ID)
		return
	}

	var m *store.Message
	switch req.Kind {
	case "text":
		if req.Body == "" {
			h.sendError(c, CodeBadRequest, "body 不能为空", f.ID)
			return
		}
		if len(req.Body) > h.cfg.MaxTextBytes {
			h.sendError(c, CodeBadRequest, "body 超过上限", f.ID)
			return
		}
		m = &store.Message{Sender: c.Endpoint, Kind: "text", Body: req.Body}
	case "image":
		if req.FileID == "" {
			h.sendError(c, CodeBadRequest, "image 消息必须携带 file_id", f.ID)
			return
		}
		meta, err := h.store.GetFile(req.FileID)
		if err != nil {
			h.sendError(c, CodeInternal, "查询文件失败", f.ID)
			return
		}
		if meta == nil {
			h.sendError(c, CodeBadRequest, "file_id 不存在", f.ID)
			return
		}
		m = &store.Message{Sender: c.Endpoint, Kind: "image", FileID: req.FileID,
			W: req.W, H: req.H, Size: meta.Size}
	default:
		h.sendError(c, CodeBadRequest, "不支持的消息 kind: "+req.Kind, f.ID)
		return
	}

	if err := h.store.Save(m); err != nil {
		log.Printf("[msg] 落库失败 sender=%s: %v", c.Endpoint, err)
		h.sendError(c, CodeInternal, "消息存储失败", f.ID)
		return
	}

	// 回执发送方：已持久化（ID 回传请求帧 id，客户端按此关联请求-响应）
	h.sendFrame(c, hub.Frame{V: 1, Type: "msg.ack", ID: f.ID, TS: time.Now().UnixMilli(),
		Data: mustJSON(map[string]any{"ref": f.ID, "seq": m.Seq, "ts": m.TS})})

	// 投递对端（在线实时；离线由 sync 补投）
	peer, ok := auth.PeerOf(c.Endpoint)
	if ok {
		h.hub.SendTo(peer, hub.Frame{V: 1, Type: "msg.new", TS: time.Now().UnixMilli(),
			Data: mustJSON(m)})
	}
}

// ---- msg.ack ----

type ackReq struct {
	Seq int64 `json:"seq"`
}

func (h *Handler) onAck(c *hub.Conn, f hub.Frame) {
	var req ackReq
	if err := json.Unmarshal(f.Data, &req); err != nil || req.Seq <= 0 {
		h.sendError(c, CodeBadRequest, "msg.ack.data 需要正整数 seq", f.ID)
		return
	}
	m, err := h.store.Get(req.Seq)
	if err != nil {
		h.sendError(c, CodeInternal, "查询消息失败", f.ID)
		return
	}
	if m == nil {
		h.sendError(c, CodeBadRequest, "消息不存在", f.ID)
		return
	}
	// 仅消息接收方可确认
	if m.Sender == c.Endpoint {
		return
	}
	h.hub.SendTo(m.Sender, hub.Frame{V: 1, Type: "msg.delivered", TS: time.Now().UnixMilli(),
		Data: mustJSON(map[string]any{"seq": m.Seq})})
}

// ---- sync ----

type syncReq struct {
	After int64 `json:"after"`
	Limit int   `json:"limit"`
}

type syncDoneData struct {
	Msgs    []store.Message `json:"msgs"`
	HasMore bool            `json:"has_more"`
}

func (h *Handler) onSync(c *hub.Conn, f hub.Frame) {
	var req syncReq
	if err := json.Unmarshal(f.Data, &req); err != nil {
		h.sendError(c, CodeBadRequest, "sync.data 解析失败", f.ID)
		return
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultSyncLimit
	}
	if limit > maxSyncLimit {
		limit = maxSyncLimit
	}
	if req.After < 0 {
		req.After = 0
	}
	msgs, hasMore, err := h.store.List(req.After, limit)
	if err != nil {
		h.sendError(c, CodeInternal, "历史查询失败", f.ID)
		return
	}
	if msgs == nil {
		msgs = []store.Message{}
	}
	h.sendFrame(c, hub.Frame{V: 1, Type: "sync.done", ID: f.ID, TS: time.Now().UnixMilli(),
		Data: mustJSON(syncDoneData{Msgs: msgs, HasMore: hasMore})})
}

// ---- helpers ----

func (h *Handler) sendFrame(c *hub.Conn, f hub.Frame) {
	c.Send(f)
}

func (h *Handler) sendError(c *hub.Conn, code int, msg, ref string) {
	c.Send(hub.Frame{V: 1, Type: "error", TS: time.Now().UnixMilli(),
		Data: mustJSON(map[string]any{"code": code, "message": msg, "ref": ref})})
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
