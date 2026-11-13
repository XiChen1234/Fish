package auth_test

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"fish/internal/auth"
	"fish/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开存储: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newAuth(t *testing.T) (*auth.Auth, *store.Store) {
	st := newStore(t)
	return auth.New(st, []byte("test-secret-key-32-xxxxx")), st
}

// ---------- Init ----------

func TestInit_FirstDeviceBecomesA(t *testing.T) {
	a, _ := newAuth(t)
	role, _, err := a.Init("device-1", "secret123")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if role != "A" {
		t.Fatalf("期望 A，got %s", role)
	}
}

func TestInit_SecondInitRejected(t *testing.T) {
	a, _ := newAuth(t)
	if _, _, err := a.Init("device-1", "secret123"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.Init("device-x", "other"); err != auth.ErrRoomExists {
		t.Fatalf("期望 ErrRoomExists, got %v", err)
	}
}

func TestInit_ValidatesInput(t *testing.T) {
	a, _ := newAuth(t)
	if _, _, err := a.Init("", "secret123"); err == nil {
		t.Fatal("空 device_id 应报错")
	}
	if _, _, err := a.Init("d", ""); err == nil {
		t.Fatal("空密码应报错")
	}
}

// TestInit_ConcurrentOnlyOneSucceeds 并发 init 只能有一个成功，避免出现两台 A。
func TestInit_ConcurrentOnlyOneSucceeds(t *testing.T) {
	a, st := newAuth(t)
	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, _, err := a.Init(fmt.Sprintf("dev-%d", i), "secret123")
			results[i] = err
		}(i)
	}
	wg.Wait()

	success := 0
	for i, err := range results {
		switch err {
		case nil:
			success++
		case auth.ErrRoomExists:
		default:
			t.Fatalf("dev-%d 返回意外错误: %v", i, err)
		}
	}
	if success != 1 {
		t.Fatalf("并发 init 应只有 1 个成功，got %d", success)
	}
	if cnt, _ := st.DeviceCount("A"); cnt != 1 {
		t.Fatalf("应只有 1 台 A 设备，got %d", cnt)
	}
}

// ---------- Login ----------

func TestLogin_FirstAfterInitBecomesB(t *testing.T) {
	a, _ := newAuth(t)
	_, _, _ = a.Init("device-a", "secret123")

	// 首次 login，自动分配 B
	role, _, err := a.Login("device-b", "secret123", "")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if role != "B" {
		t.Fatalf("期望 B，got %s", role)
	}
}

func TestLogin_WrongPassword(t *testing.T) {
	a, _ := newAuth(t)
	_, _, _ = a.Init("device-a", "secret123")
	if _, _, err := a.Login("device-b", "wrongpw", ""); err != auth.ErrBadPassword {
		t.Fatalf("期望 ErrBadPassword, got %v", err)
	}
}

func TestLogin_RoomNotInit(t *testing.T) {
	a, _ := newAuth(t)
	if _, _, err := a.Login("device-b", "whatever", ""); err != auth.ErrRoomNotInit {
		t.Fatalf("期望 ErrRoomNotInit, got %v", err)
	}
}

func TestLogin_RegisteredDeviceKeepsRole(t *testing.T) {
	a, _ := newAuth(t)
	_, tokA, _ := a.Init("device-a", "secret123")
	_, _, _ = a.Login("device-b", "secret123", "")

	// device-a 再来 login，应保持 A，desiredRole 被忽略
	role, tok, err := a.Login("device-a", "secret123", "B")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if role != "A" {
		t.Fatalf("已注册设备应保持 A，got %s", role)
	}
	if tok == tokA {
		t.Logf("token 相同（同一设备再次登录会重签新 token，属正常）")
	}
}

func TestLogin_ExplicitRoleWhenBothExist(t *testing.T) {
	a, _ := newAuth(t)
	_, _, _ = a.Init("device-a", "secret123")
	_, _, _ = a.Login("device-b", "secret123", "")

	// A、B 都已有设备，第三台必须显式指定
	role, _, err := a.Login("device-c", "secret123", "A")
	if err != nil {
		t.Fatalf("显式指定 A 应成功: %v", err)
	}
	if role != "A" {
		t.Fatalf("期望 A，got %s", role)
	}

	// 留空应报错
	if _, _, err := a.Login("device-d", "secret123", ""); err == nil {
		t.Fatal("A、B 都存在时 desiredRole 留空应报错")
	}
}

// ---------- Verify + Resume ----------

func TestVerify_TokenWorks(t *testing.T) {
	a, _ := newAuth(t)
	_, tokA, _ := a.Init("device-a", "secret123")

	role, did, ok := a.Verify(tokA)
	if !ok {
		t.Fatal("token 应有效")
	}
	if role != "A" || did != "device-a" {
		t.Fatalf("role=%s did=%s", role, did)
	}
}

func TestVerify_BadToken(t *testing.T) {
	a, _ := newAuth(t)
	if _, _, ok := a.Verify("not-a-token"); ok {
		t.Fatal("坏 token 应返回 ok=false")
	}
}

// TestResume_Validation 覆盖 resume 的设备/token 一致性校验。
func TestResume_Validation(t *testing.T) {
	a, _ := newAuth(t)
	_, tok, _ := a.Init("device-a", "secret123")

	role, did, gotTok, err := a.Resume("", tok)
	if err != nil || role != "A" || did != "device-a" || gotTok != tok {
		t.Fatalf("合法 resume 失败: role=%s did=%s err=%v", role, did, err)
	}
	if _, _, _, err := a.Resume("device-x", tok); err != auth.ErrDeviceMismatch {
		t.Fatalf("device_id 不匹配应报 ErrDeviceMismatch, got %v", err)
	}
	if _, _, _, err := a.Resume("device-a", "bad-token"); err != auth.ErrInvalidToken {
		t.Fatalf("坏 token 应报 ErrInvalidToken, got %v", err)
	}

	// 同设备重新登录换发 token 后，旧 token 立即失效
	_, tok2, err := a.Login("device-a", "secret123", "")
	if err != nil {
		t.Fatal(err)
	}
	if tok2 == tok {
		t.Fatal("重新登录应换发不同的 token")
	}
	if _, _, _, err := a.Resume("device-a", tok); err != auth.ErrInvalidToken {
		t.Fatalf("旧 token 应失效, got %v", err)
	}
	if _, _, _, err := a.Resume("device-a", tok2); err != nil {
		t.Fatalf("新 token 应可用: %v", err)
	}
}

// ---------- PeerOf ----------

func TestPeerOf(t *testing.T) {
	r1, ok := auth.PeerOf("A")
	if !ok || r1 != "B" {
		t.Fatalf("A 的对端应为 B，got %q ok=%v", r1, ok)
	}
	r2, ok := auth.PeerOf("B")
	if !ok || r2 != "A" {
		t.Fatalf("B 的对端应为 A，got %q ok=%v", r2, ok)
	}
	if _, ok := auth.PeerOf("X"); ok {
		t.Fatal("X 对端应不存在")
	}
}
