package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // 纯 Go SQLite 驱动，免 CGO，便于交叉编译
)

// Message 一条已持久化消息。载荷字段与协议 §4.2 msg.new 一致。
type Message struct {
	Seq    int64  `json:"seq"`
	Sender string `json:"from"`
	Kind   string `json:"kind"` // text | image
	Body   string `json:"body,omitempty"`
	FileID string `json:"file_id,omitempty"` // image 引用（阶段3）
	W      int    `json:"w,omitempty"`
	H      int    `json:"h,omitempty"`
	Size   int64  `json:"size,omitempty"`
	TS     int64  `json:"ts"` // 服务端落库时间，Unix 毫秒
}

// Store SQLite 单文件存储。1:1 场景写入量小，WAL + 单连接足够。
type Store struct {
	db *sql.DB
}

// sqlErrNoRows 便于 files.go 等同包文件引用。
var sqlErrNoRows = sql.ErrNoRows

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// 单连接串行写，规避 SQLite 写锁竞争
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("执行 %s 失败: %w", pragma, err)
		}
	}
	schema := `
CREATE TABLE IF NOT EXISTS messages (
	seq     INTEGER PRIMARY KEY AUTOINCREMENT,
	sender  TEXT    NOT NULL,
	kind    TEXT    NOT NULL,
	body    TEXT    NOT NULL DEFAULT '',
	file_id TEXT    NOT NULL DEFAULT '',
	w       INTEGER NOT NULL DEFAULT 0,
	h       INTEGER NOT NULL DEFAULT 0,
	size    INTEGER NOT NULL DEFAULT 0,
	ts      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_seq ON messages(seq);

-- 房间表：存储密码 hash，房间只有一个
CREATE TABLE IF NOT EXISTS rooms (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	password_hash  TEXT    NOT NULL,
	created_at     INTEGER NOT NULL
);

-- 设备表：device_id 唯一；role 为 'A' 或 'B'，同一 role 允许多设备
CREATE TABLE IF NOT EXISTS devices (
	device_id  TEXT PRIMARY KEY,
	role       TEXT NOT NULL CHECK(role IN ('A','B')),
	token      TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化表结构失败: %w", err)
	}
	s := &Store{db: db}
	if err := s.initFilesSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化 files 表失败: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Save 持久化消息并分配 seq。ts=0 时取服务端当前时间。
func (s *Store) Save(m *Message) error {
	if m.TS == 0 {
		m.TS = time.Now().UnixMilli()
	}
	res, err := s.db.Exec(
		`INSERT INTO messages(sender, kind, body, file_id, w, h, size, ts) VALUES(?,?,?,?,?,?,?,?)`,
		m.Sender, m.Kind, m.Body, m.FileID, m.W, m.H, m.Size, m.TS,
	)
	if err != nil {
		return err
	}
	m.Seq, err = res.LastInsertId()
	return err
}

// Get 按 seq 取单条（用于 msg.ack 回执定位发送方）。
func (s *Store) Get(seq int64) (*Message, error) {
	m := &Message{}
	err := s.db.QueryRow(
		`SELECT seq, sender, kind, body, file_id, w, h, size, ts FROM messages WHERE seq=?`, seq,
	).Scan(&m.Seq, &m.Sender, &m.Kind, &m.Body, &m.FileID, &m.W, &m.H, &m.Size, &m.TS)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// List 返回 seq > after 的消息（升序，至多 limit 条）及是否还有更多。
func (s *Store) List(after int64, limit int) ([]Message, bool, error) {
	rows, err := s.db.Query(
		`SELECT seq, sender, kind, body, file_id, w, h, size, ts FROM messages WHERE seq>? ORDER BY seq ASC LIMIT ?`,
		after, limit+1,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.Seq, &m.Sender, &m.Kind, &m.Body, &m.FileID, &m.W, &m.H, &m.Size, &m.TS); err != nil {
			return nil, false, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	hasMore := false
	if len(out) > limit {
		out = out[:limit]
		hasMore = true
	}
	return out, hasMore, nil
}

// LastSeq 当前最大 seq，无消息返回 0。
func (s *Store) LastSeq() int64 {
	var v sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(seq) FROM messages`).Scan(&v); err != nil || !v.Valid {
		return 0
	}
	return v.Int64
}

// ---------- Room ----------

// Room 房间基本信息。
type Room struct {
	ID           int64
	PasswordHash string
	CreatedAt    int64
}

// GetRoom 返回当前房间（数据库中应只有一条记录），未初始化返回 nil,nil。
func (s *Store) GetRoom() (*Room, error) {
	r := &Room{}
	err := s.db.QueryRow(`SELECT id, password_hash, created_at FROM rooms ORDER BY id LIMIT 1`).Scan(
		&r.ID, &r.PasswordHash, &r.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return r, nil
}

// InitRoom 首次初始化房间：写入密码 hash。成功返回 room id。
// 如果房间已存在，返回 (已存在的 id, nil)。
func (s *Store) InitRoom(passwordHash string) (int64, error) {
	existing, err := s.GetRoom()
	if err != nil {
		return 0, err
	}
	if existing != nil {
		return existing.ID, nil
	}
	res, err := s.db.Exec(
		`INSERT INTO rooms(password_hash, created_at) VALUES(?,?)`,
		passwordHash, time.Now().UnixMilli(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ---------- Device ----------

// Device 设备注册信息。
type Device struct {
	DeviceID  string
	Role      string
	Token     string
	CreatedAt int64
}

// UpsertDevice 注册或更新设备（同一 device_id 覆盖旧记录）。
// role 必须为 'A' 或 'B'。
func (s *Store) UpsertDevice(deviceID, role, token string) error {
	if role != "A" && role != "B" {
		return fmt.Errorf("非法 role: %s", role)
	}
	_, err := s.db.Exec(
		`INSERT INTO devices(device_id, role, token, created_at) VALUES(?,?,?,?)
		 ON CONFLICT(device_id) DO UPDATE SET role=excluded.role, token=excluded.token`,
		deviceID, role, token, time.Now().UnixMilli(),
	)
	return err
}

// GetDevice 按 device_id 查询。
func (s *Store) GetDevice(deviceID string) (*Device, error) {
	d := &Device{}
	err := s.db.QueryRow(
		`SELECT device_id, role, token, created_at FROM devices WHERE device_id=?`, deviceID,
	).Scan(&d.DeviceID, &d.Role, &d.Token, &d.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// GetDeviceByToken 按 token 查设备（JWT 验证后定位）。
func (s *Store) GetDeviceByToken(token string) (*Device, error) {
	d := &Device{}
	err := s.db.QueryRow(
		`SELECT device_id, role, token, created_at FROM devices WHERE token=?`, token,
	).Scan(&d.DeviceID, &d.Role, &d.Token, &d.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return d, nil
}

// DeviceCount 返回指定 role 的设备数，role 为空则返回总数。
func (s *Store) DeviceCount(role string) (int, error) {
	var c int
	if role == "" {
		err := s.db.QueryRow(`SELECT COUNT(*) FROM devices`).Scan(&c)
		return c, err
	}
	err := s.db.QueryRow(`SELECT COUNT(*) FROM devices WHERE role=?`, role).Scan(&c)
	return c, err
}
