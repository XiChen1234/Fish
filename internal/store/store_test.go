package store

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开存储失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSaveAndLastSeq(t *testing.T) {
	s := openTestStore(t)

	if got := s.LastSeq(); got != 0 {
		t.Fatalf("空库 LastSeq 应为 0，got %d", got)
	}

	m1 := &Message{Sender: "a", Kind: "text", Body: "one"}
	if err := s.Save(m1); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}
	if m1.Seq != 1 {
		t.Fatalf("首条消息 seq 应为 1，got %d", m1.Seq)
	}
	if m1.TS == 0 {
		t.Fatal("Save 应自动填充 ts")
	}

	m2 := &Message{Sender: "b", Kind: "text", Body: "two"}
	_ = s.Save(m2)
	if m2.Seq != 2 {
		t.Fatalf("seq 应单调递增，got %d", m2.Seq)
	}
	if got := s.LastSeq(); got != 2 {
		t.Fatalf("LastSeq 应为 2，got %d", got)
	}
}

func TestGet(t *testing.T) {
	s := openTestStore(t)

	m := &Message{Sender: "a", Kind: "text", Body: "hello"}
	_ = s.Save(m)

	got, err := s.Get(m.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Sender != "a" || got.Body != "hello" {
		t.Fatalf("Get 结果不符: %+v", got)
	}

	got, err = s.Get(99999)
	if err != nil || got != nil {
		t.Fatalf("不存在的 seq 应返回 nil，got %+v err=%v", got, err)
	}
}

func TestList(t *testing.T) {
	s := openTestStore(t)

	for _, body := range []string{"m1", "m2", "m3", "m4", "m5"} {
		_ = s.Save(&Message{Sender: "a", Kind: "text", Body: body})
	}

	// 全量
	msgs, hasMore, err := s.List(0, 10)
	if err != nil || len(msgs) != 5 || hasMore {
		t.Fatalf("List(0,10) 应返回 5 条无更多，got %d 条 hasMore=%v err=%v", len(msgs), hasMore, err)
	}
	if msgs[0].Body != "m1" || msgs[4].Body != "m5" {
		t.Fatalf("应按 seq 升序: %+v", msgs)
	}

	// 增量 after=2
	msgs, hasMore, _ = s.List(2, 10)
	if len(msgs) != 3 || msgs[0].Seq != 3 {
		t.Fatalf("List(2,10) 应返回 seq 3..5，got %+v", msgs)
	}

	// 分页 limit=2
	msgs, hasMore, _ = s.List(0, 2)
	if len(msgs) != 2 || !hasMore {
		t.Fatalf("List(0,2) 应返回 2 条且有更多，got %d hasMore=%v", len(msgs), hasMore)
	}

	// 空结果
	msgs, hasMore, _ = s.List(100, 10)
	if len(msgs) != 0 || hasMore {
		t.Fatalf("after 超界应返回空，got %+v", msgs)
	}
}

func TestReopenPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s1.Save(&Message{Sender: "a", Kind: "text", Body: "persist"})
	_ = s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.LastSeq(); got != 1 {
		t.Fatalf("重启后数据应保留，LastSeq=%d", got)
	}
}
