package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

// storeFactory 用于对任意 Store 实现跑同一套契约测试。
// 新增 SQLite / Redis 实现时，只需在 store_impls_test.go 里追加一个工厂即可复用。
type storeFactory struct {
	name string
	new  func(t *testing.T) Store
}

// RunStoreContract 校验 Store 接口的全部不变条件。
//
// 这些断言与具体存储无关，因此新实现接入时「契约测试先跑通」即可认为行为对齐。
func RunStoreContract(t *testing.T, f storeFactory) {
	t.Helper()

	t.Run("Create_Get", func(t *testing.T) {
		s := f.new(t)
		ctx := context.Background()
		sess := NewSession("契约会话", "m")
		if err := s.Create(ctx, sess); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := s.Get(ctx, sess.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.ID != sess.ID || got.Title != "契约会话" {
			t.Fatalf("Get 结果不一致: %+v", got)
		}
		if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
			t.Fatalf("时间戳不应为零值: %+v", got)
		}
	})

	t.Run("Create_Duplicate", func(t *testing.T) {
		s := f.new(t)
		ctx := context.Background()
		sess := NewSession("dup", "m")
		if err := s.Create(ctx, sess); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.Create(ctx, sess.Clone()); !errors.Is(err, ErrConflict) {
			t.Fatalf("重复创建 err = %v，期望 ErrConflict", err)
		}
	})

	t.Run("NotFound", func(t *testing.T) {
		s := f.new(t)
		ctx := context.Background()
		if _, err := s.Get(ctx, "sess_none"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get err = %v，期望 ErrNotFound", err)
		}
		if _, err := s.Messages(ctx, "sess_none"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Messages err = %v，期望 ErrNotFound", err)
		}
		if err := s.Delete(ctx, "sess_none"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Delete err = %v，期望 ErrNotFound", err)
		}
		if err := s.AppendMessage(ctx, NewUserMessage("sess_none", "x")); !errors.Is(err, ErrNotFound) {
			t.Fatalf("AppendMessage err = %v，期望 ErrNotFound", err)
		}
	})

	t.Run("AppendMessage_MaintainsCount", func(t *testing.T) {
		s := f.new(t)
		ctx := context.Background()
		sess := NewSession("msg", "m")
		if err := s.Create(ctx, sess); err != nil {
			t.Fatalf("Create: %v", err)
		}
		for i := 0; i < 3; i++ {
			if err := s.AppendMessage(ctx, NewUserMessage(sess.ID, "hi")); err != nil {
				t.Fatalf("AppendMessage: %v", err)
			}
		}
		got, err := s.Get(ctx, sess.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.MessageCount != 3 {
			t.Fatalf("MessageCount = %d，期望 3", got.MessageCount)
		}
		msgs, err := s.Messages(ctx, sess.ID)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		if len(msgs) != 3 {
			t.Fatalf("消息数 = %d，期望 3", len(msgs))
		}
	})

	t.Run("Update_AtomicRollback", func(t *testing.T) {
		s := f.new(t)
		ctx := context.Background()
		sess := NewSession("rollback", "m")
		if err := s.Create(ctx, sess); err != nil {
			t.Fatalf("Create: %v", err)
		}
		boom := errors.New("boom")
		if _, err := s.Update(ctx, sess.ID, func(sc *Session) error {
			sc.Title = "不该生效"
			return boom
		}); !errors.Is(err, boom) {
			t.Fatalf("err = %v，期望原样返回 fn 错误", err)
		}
		got, _ := s.Get(ctx, sess.ID)
		if got.Title != "rollback" {
			t.Fatalf("fn 报错后状态被污染: %q", got.Title)
		}
	})

	t.Run("Update_RefreshesUpdatedAt", func(t *testing.T) {
		s := f.new(t)
		ctx := context.Background()
		sess := NewSession("ts", "m")
		if err := s.Create(ctx, sess); err != nil {
			t.Fatalf("Create: %v", err)
		}
		before, _ := s.Get(ctx, sess.ID)
		time.Sleep(2 * time.Millisecond)
		after, err := s.Update(ctx, sess.ID, func(sc *Session) error {
			sc.Title = "改名"
			return nil
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if !after.UpdatedAt.After(before.UpdatedAt) {
			t.Fatalf("UpdatedAt 未推进: %v -> %v", before.UpdatedAt, after.UpdatedAt)
		}
		if !after.CreatedAt.Equal(before.CreatedAt) {
			t.Fatal("CreatedAt 不应改变")
		}
	})

	t.Run("UpdateMessage_Status", func(t *testing.T) {
		s := f.new(t)
		ctx := context.Background()
		sess := NewSession("um", "m")
		if err := s.Create(ctx, sess); err != nil {
			t.Fatalf("Create: %v", err)
		}
		msg := NewAssistantPlaceholder(sess.ID)
		if err := s.AppendMessage(ctx, msg); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
		if err := s.UpdateMessage(ctx, sess.ID, msg.ID, func(m *Message) error {
			m.Content = "done"
			m.Status = StatusComplete
			return nil
		}); err != nil {
			t.Fatalf("UpdateMessage: %v", err)
		}
		msgs, _ := s.Messages(ctx, sess.ID)
		if msgs[0].Content != "done" || msgs[0].Status != StatusComplete {
			t.Fatalf("消息未更新: %+v", msgs[0])
		}
	})

	t.Run("Delete_CascadesMessages", func(t *testing.T) {
		s := f.new(t)
		ctx := context.Background()
		sess := NewSession("del", "m")
		if err := s.Create(ctx, sess); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.AppendMessage(ctx, NewUserMessage(sess.ID, "x")); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
		if err := s.Delete(ctx, sess.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := s.Get(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("删除后 Get err = %v", err)
		}
		if _, err := s.Messages(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("删除后 Messages err = %v", err)
		}
	})

	t.Run("List_DescAndPaging", func(t *testing.T) {
		s := f.new(t)
		ctx := context.Background()
		for i := 0; i < 4; i++ {
			sess := NewSessionAt("c", "m", time.Date(2025, 1, 1, 8, i, 0, 0, time.UTC))
			if err := s.Create(ctx, sess); err != nil {
				t.Fatalf("Create: %v", err)
			}
			time.Sleep(2 * time.Millisecond) // 保证 updated_at 严格递增
		}
		page1, cursor, err := s.List(ctx, ListOptions{Limit: 2})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(page1) != 2 {
			t.Fatalf("page1 = %d 条，期望 2", len(page1))
		}
		if cursor == "" {
			t.Fatal("还有更多数据时应返回游标")
		}
		page2, _, err := s.List(ctx, ListOptions{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatalf("List page2: %v", err)
		}
		if len(page2) != 2 {
			t.Fatalf("page2 = %d 条，期望 2", len(page2))
		}
		// 分页不应重复返回同一条会话。
		seen := map[string]bool{}
		for _, x := range append(page1, page2...) {
			if seen[x.ID] {
				t.Fatalf("分页出现重复会话 %s", x.ID)
			}
			seen[x.ID] = true
		}
	})

	t.Run("DeepCopyIsolation", func(t *testing.T) {
		s := f.new(t)
		ctx := context.Background()
		sess := NewSession("copy", "m")
		sess.Meta = map[string]string{"k": "v"}
		if err := s.Create(ctx, sess); err != nil {
			t.Fatalf("Create: %v", err)
		}
		msg := NewAssistantPlaceholder(sess.ID)
		msg.Usage = &Usage{TotalTokens: 7}
		if err := s.AppendMessage(ctx, msg); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}

		// 分别篡改读到的会话与消息，再读一次确认内部状态未被污染。
		got, _ := s.Get(ctx, sess.ID)
		got.Meta["k"] = "tampered"
		msgs, _ := s.Messages(ctx, sess.ID)
		msgs[0].Usage.TotalTokens = 999
		msgs[0].Content = "tampered"

		again, _ := s.Get(ctx, sess.ID)
		if again.Meta["k"] != "v" {
			t.Fatalf("会话 Meta 未隔离: %v", again.Meta)
		}
		againMsgs, _ := s.Messages(ctx, sess.ID)
		if againMsgs[0].Usage.TotalTokens != 7 || againMsgs[0].Content != "" {
			t.Fatalf("消息深拷贝未隔离: %+v", againMsgs[0])
		}
	})

	t.Run("ContextCanceled", func(t *testing.T) {
		s := f.new(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := s.Create(ctx, NewSession("x", "m")); !errors.Is(err, context.Canceled) {
			t.Fatalf("Create err = %v，期望 context.Canceled", err)
		}
	})
}

// RunStoreContractHelper 是给外部包（其他存储实现）使用的导出入口。
//
// 用法：在实现方所在包的测试里调用，传入构造函数即可复用全部契约断言。
// 这样新实现接入时无需复制测试代码，也能保证行为与内存实现一致。
func RunStoreContractHelper(t *testing.T, newStore func(t *testing.T) Store) {
	t.Helper()
	RunStoreContract(t, storeFactory{
		name: "custom",
		new:  newStore,
	})
}

func TestMemoryStoreContract(t *testing.T) {
	RunStoreContract(t, storeFactory{
		name: "memory",
		new: func(t *testing.T) Store {
			t.Helper()
			return NewMemoryStore(0)
		},
	})
}

func TestMemoryStoreContractWithTrim(t *testing.T) {
	// 开启裁剪后同样必须满足全部契约（计数、分页、深拷贝等）。
	RunStoreContract(t, storeFactory{
		name: "memory-trim",
		new: func(t *testing.T) Store {
			t.Helper()
			return NewMemoryStore(5)
		},
	})
}
