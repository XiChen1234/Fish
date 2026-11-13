// Package auth 负责：房间初始化（init）、设备登录（login）、JWT 签发与验证。
// 核心角色：role 为 "A" 或 "B"，同一 role 允许多设备同时在线。
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"fish/internal/store"
)

// 错误码（供上层使用）。
var (
	ErrRoomExists    = errors.New("房间已初始化，请使用 login")
	ErrRoomNotInit   = errors.New("房间未初始化，请使用 init 设置密码")
	ErrBadPassword   = errors.New("密码错误")
	ErrBadRole       = errors.New("role 必须为 A 或 B")
	ErrPasswordEmpty = errors.New("密码不能为空")
	ErrDeviceEmpty   = errors.New("device_id 不能为空")
	ErrInvalidToken  = errors.New("token 无效")
)

const (
	passwordMinLen = 4
	passwordMaxLen = 64
)

// Auth 动态鉴权管理器（基于 store，无预设 token）。
type Auth struct {
	store   *store.Store
	jwtKey  []byte // 32+ 字节随机密钥，首次启动时生成并持久化（简单起见这里内联生成）
	jwtExp  time.Duration
}

// New 创建 Auth。jwtKey 为 HMAC 签名密钥；传 nil 则自动生成（进程重启后失效，
// 适合单机/测试环境；生产应持久化到本地文件）。
func New(st *store.Store, jwtKey []byte) *Auth {
	if jwtKey == nil {
		jwtKey = make([]byte, 32)
		_, _ = rand.Read(jwtKey)
	}
	return &Auth{store: st, jwtKey: jwtKey, jwtExp: 365 * 24 * time.Hour}
}

// ---------- Action: Init ----------

// Init 首次连接时：用密码初始化房间并以 role A 登录。
// 房间已存在返回 ErrRoomExists。
func (a *Auth) Init(deviceID, password string) (role, token string, err error) {
	if err := validateDevice(deviceID); err != nil {
		return "", "", err
	}
	if err := validatePassword(password); err != nil {
		return "", "", err
	}
	if room, _ := a.store.GetRoom(); room != nil {
		return "", "", ErrRoomExists
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", "", err
	}
	if _, err := a.store.InitRoom(string(hash)); err != nil {
		return "", "", err
	}
	return a.registerDevice(deviceID, "A")
}

// ---------- Action: Login ----------

// Login 密码登录。房间未初始化返回 ErrRoomNotInit。
// 若设备已注册：密码校验必须通过，role 保持不变（desiredRole 忽略）。
// 若新设备：密码校验通过后 role 按规则分配——如果房间只有 A 已存在（设备数>=1），
// 新设备默认给 B；若 A、B 都已存在则按 desiredRole 分配（desiredRole 留空报错）。
func (a *Auth) Login(deviceID, password, desiredRole string) (role, token string, err error) {
	if err := validateDevice(deviceID); err != nil {
		return "", "", err
	}
	if err := validatePassword(password); err != nil {
		return "", "", err
	}
	room, err := a.store.GetRoom()
	if err != nil {
		return "", "", err
	}
	if room == nil {
		return "", "", ErrRoomNotInit
	}
	if err := bcrypt.CompareHashAndPassword([]byte(room.PasswordHash), []byte(password)); err != nil {
		return "", "", ErrBadPassword
	}

	existing, err := a.store.GetDevice(deviceID)
	if err != nil {
		return "", "", err
	}
	if existing != nil {
		// 已注册设备：复用原 role，只签新 token
		tok, err := a.sign(existing.Role, deviceID)
		if err != nil {
			return "", "", err
		}
		_ = a.store.UpsertDevice(deviceID, existing.Role, tok)
		return existing.Role, tok, nil
	}

	// 新设备：分配 role
	role = strings.ToUpper(strings.TrimSpace(desiredRole))
	if role == "" {
		// 自动分配：看已注册设备情况
		aCnt, _ := a.store.DeviceCount("A")
		bCnt, _ := a.store.DeviceCount("B")
		switch {
		case aCnt == 0:
			role = "A"
		case bCnt == 0:
			role = "B"
		default:
			return "", "", errors.New("房间内 A、B 都已有设备，请显式指定 desired_role=A 或 B")
		}
	} else if role != "A" && role != "B" {
		return "", "", ErrBadRole
	}

	return a.registerDevice(deviceID, role)
}

func (a *Auth) registerDevice(deviceID, role string) (string, string, error) {
	tok, err := a.sign(role, deviceID)
	if err != nil {
		return "", "", err
	}
	if err := a.store.UpsertDevice(deviceID, role, tok); err != nil {
		return "", "", err
	}
	return role, tok, nil
}

// ---------- Token 验证 ----------

// Verify 验证 JWT，返回 (role, deviceID, ok)。
// token 无效/过期返回 ("","",false)。
func (a *Auth) Verify(token string) (role, deviceID string, ok bool) {
	claims := &jwtClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("非 HMAC 签名方法: %v", t.Header["alg"])
		}
		return a.jwtKey, nil
	})
	if err != nil || !parsed.Valid {
		return "", "", false
	}
	if claims.Role != "A" && claims.Role != "B" {
		return "", "", false
	}
	return claims.Role, claims.DeviceID, true
}

// VerifyFromBearer 从 Authorization: Bearer <token> 头或 ?token= query 取值验证。
func (a *Auth) VerifyFromBearer(bearerHeader, queryToken string) (role, deviceID string, ok bool) {
	tok := ""
	const prefix = "Bearer "
	if len(bearerHeader) > len(prefix) && bearerHeader[:len(prefix)] == prefix {
		tok = bearerHeader[len(prefix):]
	}
	if tok == "" {
		tok = queryToken
	}
	if tok == "" {
		return "", "", false
	}
	return a.Verify(strings.TrimSpace(tok))
}

// ---------- PeerOf ----------

// PeerOf 是给 Hub/Handler 用的：给定 A 返回 B，给定 B 返回 A。
func PeerOf(role string) (string, bool) {
	switch strings.ToUpper(role) {
	case "A":
		return "B", true
	case "B":
		return "A", true
	}
	return "", false
}

// ---------- helpers ----------

type jwtClaims struct {
	jwt.RegisteredClaims
	Role     string `json:"role"`
	DeviceID string `json:"did"`
}

func (a *Auth) sign(role, deviceID string) (string, error) {
	now := time.Now()
	claims := jwtClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(a.jwtExp)),
			Subject:   deviceID,
			Issuer:    "fish",
		},
		Role:     role,
		DeviceID: deviceID,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(a.jwtKey)
}

func validateDevice(id string) error {
	if strings.TrimSpace(id) == "" {
		return ErrDeviceEmpty
	}
	if len(id) > 128 {
		return fmt.Errorf("device_id 过长（%d>128）", len(id))
	}
	return nil
}

func validatePassword(p string) error {
	if p == "" {
		return ErrPasswordEmpty
	}
	if len(p) < passwordMinLen || len(p) > passwordMaxLen {
		return fmt.Errorf("密码长度需在 %d-%d 之间", passwordMinLen, passwordMaxLen)
	}
	return nil
}

// RandDeviceID 生成一个短 device_id，供浏览器端首次生成用。
func RandDeviceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
