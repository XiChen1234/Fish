package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"fish/internal/store"
)

// 允许的图片类型，见协议 §7
var allowedImageTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

func (s *Server) filesDir() string {
	return filepath.Join(s.cfg.DataDir, "files")
}

// handleFileUpload POST /api/files：multipart 字段 file，Bearer 鉴权。
// 文件本体落盘 data_dir/files/{id}，元数据入 SQLite，返回 {file_id, size}。
func (s *Server) handleFileUpload(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authFile(w, r); !ok {
		return
	}

	// 大小限制（含 multipart 开销，额外放宽 1MB）
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxFileBytes+(1<<20))
	if err := r.ParseMultipartForm(s.cfg.MaxFileBytes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "multipart 解析失败或文件超限: " + err.Error()})
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 file 字段"})
		return
	}
	defer file.Close()

	// 嗅探真实类型，杜绝伪装扩展名
	head := make([]byte, 512)
	n, _ := io.ReadFull(file, head)
	contentType := http.DetectContentType(head[:n])
	ext, allowed := allowedImageTypes[contentType]
	if !allowed {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "不支持的图片类型: " + contentType})
		return
	}

	rest, err := io.ReadAll(io.LimitReader(file, s.cfg.MaxFileBytes))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "读取上传内容失败"})
		return
	}
	data := make([]byte, 0, n+len(rest))
	data = append(data, head[:n]...)
	data = append(data, rest...)
	if int64(len(data)) > s.cfg.MaxFileBytes {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "文件超过大小上限"})
		return
	}

	fileID, err := newFileID()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "生成文件 ID 失败"})
		return
	}

	if err := os.MkdirAll(s.filesDir(), 0o700); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "创建文件目录失败"})
		return
	}
	dst := filepath.Join(s.filesDir(), fileID+ext)
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "文件落盘失败"})
		return
	}

	meta := &store.FileMeta{ID: fileID, ContentType: contentType, Size: int64(len(data))}
	if err := s.store.SaveFile(meta); err != nil {
		_ = os.Remove(dst) // 元数据失败则回收文件，保证一致性
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "文件元数据写入失败"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"file_id": fileID, "size": meta.Size})
}

// handleFileDownload GET /api/files/{id}：鉴权后回传文件本体。
func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authFile(w, r); !ok {
		return
	}
	rawID := r.PathValue("id")
	// 防目录穿越：file_id 必须是纯 hex
	if !isHex(rawID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "非法文件 ID"})
		return
	}

	meta, err := s.store.GetFile(rawID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "查询文件失败"})
		return
	}
	if meta == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "文件不存在"})
		return
	}

	// 依据元数据 content type 恢复扩展名定位文件
	ext, ok := allowedImageTypes[meta.ContentType]
	if !ok {
		ext = ".bin"
	}
	path := filepath.Join(s.filesDir(), rawID+ext)
	f, err := os.Open(path)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "文件不存在"})
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", meta.ContentType)
	w.Header().Set("Content-Length", fmt.Sprint(meta.Size))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = io.Copy(w, f)
}

// authFile 文件接口鉴权：Bearer 头或 ?token=（img 标签场景）。
func (s *Server) authFile(w http.ResponseWriter, r *http.Request) (string, bool) {
	const prefix = "Bearer "
	v := r.Header.Get("Authorization")
	tok := ""
	if len(v) > len(prefix) && v[:len(prefix)] == prefix {
		tok = v[len(prefix):]
	}
	if tok == "" {
		tok = r.URL.Query().Get("token")
	}
	role, _, ok := s.auth.Verify(tok)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid token"})
		return "", false
	}
	return role, true
}

func newFileID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}
