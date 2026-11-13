package auth

import "testing"

func TestVerify(t *testing.T) {
	a := New(map[string]string{"a": "tok-a", "b": "tok-b"})

	if id, ok := a.Verify("tok-a"); !ok || id != "a" {
		t.Fatalf("tok-a 应验证为 a，got id=%q ok=%v", id, ok)
	}
	if id, ok := a.Verify("tok-b"); !ok || id != "b" {
		t.Fatalf("tok-b 应验证为 b，got id=%q ok=%v", id, ok)
	}
	if _, ok := a.Verify("bad"); ok {
		t.Fatal("无效 token 不应通过验证")
	}
	if _, ok := a.Verify(""); ok {
		t.Fatal("空 token 不应通过验证")
	}
}

func TestPeer(t *testing.T) {
	a := New(map[string]string{"a": "tok-a", "b": "tok-b"})

	if p, ok := a.Peer("a"); !ok || p != "b" {
		t.Fatalf("a 的对端应为 b，got %q ok=%v", p, ok)
	}
	if p, ok := a.Peer("b"); !ok || p != "a" {
		t.Fatalf("b 的对端应为 a，got %q ok=%v", p, ok)
	}
	if _, ok := a.Peer("c"); ok {
		t.Fatal("未知身份不应有对端")
	}
}
