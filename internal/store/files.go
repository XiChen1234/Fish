package store

import "time"

// FileMeta 图片文件元数据。文件本体落盘在 data_dir/files/{id}，此处仅记录索引。
type FileMeta struct {
	ID          string `json:"file_id"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	TS          int64  `json:"ts"`
}

func (s *Store) initFilesSchema() error {
	schema := `
CREATE TABLE IF NOT EXISTS files (
	id           TEXT    PRIMARY KEY,
	content_type TEXT    NOT NULL,
	size         INTEGER NOT NULL,
	ts           INTEGER NOT NULL
);
`
	_, err := s.db.Exec(schema)
	return err
}

// SaveFile 记录文件元数据（文件本体由 api 层落盘，先落盘后记元数据）。
func (s *Store) SaveFile(m *FileMeta) error {
	if m.TS == 0 {
		m.TS = time.Now().UnixMilli()
	}
	_, err := s.db.Exec(
		`INSERT INTO files(id, content_type, size, ts) VALUES(?,?,?,?)`,
		m.ID, m.ContentType, m.Size, m.TS,
	)
	return err
}

// GetFile 取文件元数据；不存在返回 nil。
func (s *Store) GetFile(id string) (*FileMeta, error) {
	m := &FileMeta{}
	err := s.db.QueryRow(
		`SELECT id, content_type, size, ts FROM files WHERE id=?`, id,
	).Scan(&m.ID, &m.ContentType, &m.Size, &m.TS)
	if err == sqlErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}
