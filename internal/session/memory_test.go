package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// newTestStore 返回注入了可控时钟的内存存储，便于断言时间字段。
func newTestStore(maxMsgs int) (*MemoryStore, *fakeClock) {
	clock := &fakeClock{t: time.Date(2025, 1, 1, 8, 0, 0, 0, time.UTC)}
	s := NewMemoryStore(maxMsgs)
	s.SetClock(clock.Now)
	return s, clock
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func mustCreate(t *testing.T, s *MemoryStore, title string) *Session {
	t.Helper()
	sess := NewSessionAt(title, "test-model", s.clock())
	if err := s.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create(%q): %v", title, err)
	}
	return sess
}

// clock 返回当前注入时钟的读数（测试辅助）。
func (s *MemoryStore) clock() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.now()
}

// ---- 创建 ----

func TestCreateSession(t *testing.T) {
	ctx := context.Background()
	s, clock := newTestStore(0)

	sess := NewSessionAt("  关于 SSE 的  设计讨论  ", "gpt-4o-mini", clock.Now())
	if err := s.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := s.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "关于 SSE 的 设计讨论" {
		t.Fatalf("Title = %q，期望折叠多余空白", got.Title)
	}
	if got.ID == "" || got.ID[:5] != "sess_" {
		t.Fatalf("ID = %q，期望 sess_ 前缀", got.ID)
	}
	if got.Status != SessionIdle {
		t.Fatalf("Status = %q，期望 idle", got.Status)
	}
	if !got.CreatedAt.Equal(clock.Now()) || !got.UpdatedAt.Equal(clock.Now()) {
		t.Fatalf("时间戳未使用注入时钟: created=%v updated=%v", got.CreatedAt, got.UpdatedAt)
	}
	if got.MessageCount != 0 {
		t.Fatalf("MessageCount = %d，期望 0", got.MessageCount)
	}
}

func TestCreateSessionDefaultTitle(t *testing.T) {
	s, clock := newTestStore(0)
	sess := NewSessionAt("", "", clock.Now())
	if err := s.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := "新会话 · " + clock.Now().Format("01-02 15:04")
	if sess.Title != want {
		t.Fatalf("Title = %q，期望 %q", sess.Title, want)
	}
	if !IsDefaultTitle(sess.Title) {
		t.Fatal("默认标题应被 IsDefaultTitle 识别")
	}
	if sess.Model != "mock-echo" {
		t.Fatalf("Model = %q，期望回退 mock-echo", sess.Model)
	}
}

func TestCreateDuplicateID(t *testing.T) {
	s, _ := newTestStore(0)
	sess := mustCreate(t, s, "a")
	if err := s.Create(context.Background(), sess); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复创建 err = %v，期望 ErrConflict", err)
	}
}

func TestCreateValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(0)

	if err := s.Create(ctx, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Create(nil) err = %v，期望 ErrInvalidArgument", err)
	}

	bad := &Session{ID: "", Status: SessionIdle}
	if err := s.Create(ctx, bad); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空 ID err = %v，期望 ErrInvalidArgument", err)
	}

	badStatus := &Session{ID: NewSessionID(), Status: "bogus"}
	if err := s.Create(ctx, badStatus); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("非法状态 err = %v，期望 ErrInvalidArgument", err)
	}
}

// ---- 读取 ----

func TestGetNotFound(t *testing.T) {
	s, _ := newTestStore(0)
	if _, err := s.Get(context.Background(), "sess_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get err = %v，期望 ErrNotFound", err)
	}
}

func TestGetReturnsDeepCopy(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(0)
	sess := NewSession("原标题", "m")
	sess.Meta = map[string]string{"client": "web"}
	if err := s.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	first, _ := s.Get(ctx, sess.ID)
	first.Title = "被外部改掉了"
	first.Meta["client"] = "hacked"
	first.Meta["injected"] = "yes"

	second, err := s.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if second.Title != "原标题" {
		t.Fatalf("Title 被外部修改污染: %q", second.Title)
	}
	if second.Meta["client"] != "web" {
		t.Fatalf("Meta 被外部修改污染: %v", second.Meta)
	}
	// Create 也应拷贝入参，外部改动原对象不影响存储。
	sess.Title = "外部又改了"
	third, _ := s.Get(ctx, sess.ID)
	if third.Title != "原标题" {
		t.Fatalf("Create 未拷贝入参: %q", third.Title)
	}
}

// ---- 更新 ----

func TestUpdateSession(t *testing.T) {
	ctx := context.Background()
	s, clock := newTestStore(0)
	sess := mustCreate(t, s, "旧标题")

	clock.Advance(2 * time.Minute)
	updated, err := s.Update(ctx, sess.ID, func(sc *Session) error {
		sc.Title = "新标题"
		sc.Status = SessionRunning
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Title != "新标题" || updated.Status != SessionRunning {
		t.Fatalf("Update 结果错误: %+v", updated)
	}
	if !updated.UpdatedAt.Equal(clock.Now()) {
		t.Fatalf("UpdatedAt 未刷新: %v", updated.UpdatedAt)
	}
	if !updated.CreatedAt.Equal(sess.CreatedAt) {
		t.Fatal("Update 不应改变 CreatedAt")
	}
}

func TestUpdateRollbackOnError(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(0)
	sess := mustCreate(t, s, "原标题")

	sentinel := errors.New("业务校验失败")
	_, err := s.Update(ctx, sess.ID, func(sc *Session) error {
		sc.Title = "不该生效的标题"
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v，期望原样返回 fn 的错误", err)
	}

	got, _ := s.Get(ctx, sess.ID)
	if got.Title != "原标题" {
		t.Fatalf("fn 报错后状态被污染: %q", got.Title)
	}
}

func TestUpdateCannotChangeID(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(0)
	sess := mustCreate(t, s, "t")

	if _, err := s.Update(ctx, sess.ID, func(sc *Session) error {
		sc.ID = "sess_hijacked"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != sess.ID {
		t.Fatalf("ID 被篡改: %q", got.ID)
	}
}

func TestUpdateNotFound(t *testing.T) {
	s, _ := newTestStore(0)
	_, err := s.Update(context.Background(), "sess_x", func(*Session) error { return nil })
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v，期望 ErrNotFound", err)
	}
}

// ---- 列表与分页 ----

func TestListOrderAndPaging(t *testing.T) {
	ctx := context.Background()
	s, clock := newTestStore(0)

	ids := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		sess := mustCreate(t, s, fmt.Sprintf("会话%d", i))
		ids = append(ids, sess.ID)
		clock.Advance(time.Minute) // 保证 updated_at 严格递增
	}

	page1, cursor, err := s.List(ctx, ListOptions{Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 长度 = %d，期望 2", len(page1))
	}
	if cursor == "" {
		t.Fatal("还有更多数据时应返回 next_cursor")
	}
	// 倒序：最新创建的在最前。
	if page1[0].ID != ids[4] || page1[1].ID != ids[3] {
		t.Fatalf("倒序错误: %q, %q", page1[0].Title, page1[1].Title)
	}

	page2, cursor2, err := s.List(ctx, ListOptions{Limit: 2, Cursor: cursor})
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if len(page2) != 2 || page2[0].ID != ids[2] {
		t.Fatalf("page2 错误: %+v", page2)
	}

	page3, cursor3, err := s.List(ctx, ListOptions{Limit: 2, Cursor: cursor2})
	if err != nil {
		t.Fatalf("List page3: %v", err)
	}
	if len(page3) != 1 || page3[0].ID != ids[0] {
		t.Fatalf("page3 错误: %+v", page3)
	}
	if cursor3 != "" {
		t.Fatalf("末页 next_cursor 应为空，得到 %q", cursor3)
	}
}

func TestListLimitClamping(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(0)
	for i := 0; i < 3; i++ {
		mustCreate(t, s, fmt.Sprintf("c%d", i))
	}

	all, _, err := s.List(ctx, ListOptions{Limit: 0})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("默认 limit 应返回全部 3 条，得到 %d", len(all))
	}

	oversize, _, err := s.List(ctx, ListOptions{Limit: 10000})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(oversize) != 3 {
		t.Fatalf("上限 clamp 后应返回 3 条，得到 %d", len(oversize))
	}
}

func TestListEmpty(t *testing.T) {
	s, _ := newTestStore(0)
	items, cursor, err := s.List(context.Background(), ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 0 || cursor != "" {
		t.Fatalf("空库 List = %v, cursor=%q", items, cursor)
	}
}

// ---- 删除 ----

func TestDeleteSession(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(0)
	sess := mustCreate(t, s, "待删除")
	if err := s.AppendMessage(ctx, NewUserMessage(sess.ID, "hi")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	if err := s.Delete(ctx, sess.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后 Get err = %v，期望 ErrNotFound", err)
	}
	if _, err := s.Messages(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("删除后 Messages err = %v，期望 ErrNotFound", err)
	}
	if err := s.Delete(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("重复删除 err = %v，期望 ErrNotFound", err)
	}
	if s.Len() != 0 {
		t.Fatalf("Len = %d，期望 0", s.Len())
	}
}

// ---- 消息 ----

func TestAppendMessageUpdatesCountAndTime(t *testing.T) {
	ctx := context.Background()
	s, clock := newTestStore(0)
	sess := mustCreate(t, s, "会话")

	clock.Advance(time.Minute)
	msg := NewUserMessage(sess.ID, "你好")
	if err := s.AppendMessage(ctx, msg); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	got, _ := s.Get(ctx, sess.ID)
	if got.MessageCount != 1 {
		t.Fatalf("MessageCount = %d，期望 1", got.MessageCount)
	}
	if !got.UpdatedAt.Equal(clock.Now()) {
		t.Fatalf("会话 UpdatedAt 未随消息刷新: %v", got.UpdatedAt)
	}

	msgs, err := s.Messages(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "你好" || msgs[0].Role != RoleUser {
		t.Fatalf("消息落库错误: %+v", msgs)
	}
	if msgs[0].Status != StatusComplete {
		t.Fatalf("用户消息状态 = %q，期望 complete", msgs[0].Status)
	}
}

func TestAppendMessageUnknownSession(t *testing.T) {
	s, _ := newTestStore(0)
	err := s.AppendMessage(context.Background(), NewUserMessage("sess_missing", "hi"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v，期望 ErrNotFound", err)
	}
}

func TestAppendMessageValidation(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(0)
	sess := mustCreate(t, s, "会话")

	cases := []struct {
		name string
		msg  *Message
	}{
		{"nil", nil},
		{"空 ID", &Message{SessionID: sess.ID, Role: RoleUser, Status: StatusComplete}},
		{"空 session", &Message{ID: NewMessageID(), Role: RoleUser, Status: StatusComplete}},
		{"非法角色", &Message{ID: NewMessageID(), SessionID: sess.ID, Role: "robot", Status: StatusComplete}},
		{"非法状态", &Message{ID: NewMessageID(), SessionID: sess.ID, Role: RoleUser, Status: "bogus"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := s.AppendMessage(ctx, c.msg); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err = %v，期望 ErrInvalidArgument", err)
			}
		})
	}
}

func TestUpdateMessage(t *testing.T) {
	ctx := context.Background()
	s, clock := newTestStore(0)
	sess := mustCreate(t, s, "会话")
	placeholder := NewAssistantPlaceholderAt(sess.ID, clock.Now())
	if err := s.AppendMessage(ctx, placeholder); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	clock.Advance(time.Minute)
	err := s.UpdateMessage(ctx, sess.ID, placeholder.ID, func(m *Message) error {
		m.Content = "完整回答"
		m.Status = StatusComplete
		m.Usage = &Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30}
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateMessage: %v", err)
	}

	msgs, _ := s.Messages(ctx, sess.ID)
	if msgs[0].Content != "完整回答" || msgs[0].Status != StatusComplete {
		t.Fatalf("消息未更新: %+v", msgs[0])
	}
	if msgs[0].Usage == nil || msgs[0].Usage.TotalTokens != 30 {
		t.Fatalf("Usage 未写入: %+v", msgs[0].Usage)
	}
	if !msgs[0].CreatedAt.Before(msgs[0].UpdatedAt) {
		t.Fatalf("UpdatedAt 应晚于 CreatedAt: %+v", msgs[0])
	}
}

func TestUpdateMessageRollbackOnError(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(0)
	sess := mustCreate(t, s, "会话")
	msg := NewAssistantPlaceholder(sess.ID)
	if err := s.AppendMessage(ctx, msg); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	sentinel := errors.New("boom")
	err := s.UpdateMessage(ctx, sess.ID, msg.ID, func(m *Message) error {
		m.Content = "不该生效"
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v，期望原样返回 fn 错误", err)
	}
	msgs, _ := s.Messages(ctx, sess.ID)
	if msgs[0].Content != "" {
		t.Fatalf("fn 报错后内容被污染: %q", msgs[0].Content)
	}
}

func TestUpdateMessageNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(0)
	sess := mustCreate(t, s, "会话")

	if err := s.UpdateMessage(ctx, sess.ID, "msg_missing", func(*Message) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在消息 err = %v，期望 ErrNotFound", err)
	}
	if err := s.UpdateMessage(ctx, "sess_missing", "msg_x", func(*Message) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在会话 err = %v，期望 ErrNotFound", err)
	}
}

func TestMessagesReturnsDeepCopy(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(0)
	sess := mustCreate(t, s, "会话")
	msg := NewAssistantPlaceholder(sess.ID)
	msg.Usage = &Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	if err := s.AppendMessage(ctx, msg); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	first, _ := s.Messages(ctx, sess.ID)
	first[0].Content = "外部篡改"
	first[0].Usage.TotalTokens = 999

	second, _ := s.Messages(ctx, sess.ID)
	if second[0].Content != "" {
		t.Fatalf("Content 被外部修改污染: %q", second[0].Content)
	}
	if second[0].Usage.TotalTokens != 3 {
		t.Fatalf("Usage 指针被共享: %d", second[0].Usage.TotalTokens)
	}
}

// ---- 消息裁剪 ----

func TestTrimKeepsSystemAndNewest(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(4)
	sess := mustCreate(t, s, "会话")

	system := &Message{ID: NewMessageID(), SessionID: sess.ID, Role: RoleSystem, Content: "系统提示", Status: StatusComplete}
	if err := s.AppendMessage(ctx, system); err != nil {
		t.Fatalf("AppendMessage system: %v", err)
	}
	for i := 0; i < 6; i++ {
		m := NewUserMessage(sess.ID, fmt.Sprintf("msg-%d", i))
		if err := s.AppendMessage(ctx, m); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}

	msgs, _ := s.Messages(ctx, sess.ID)
	if len(msgs) != 4 {
		t.Fatalf("裁剪后长度 = %d，期望 4", len(msgs))
	}
	if msgs[0].Role != RoleSystem || msgs[0].Content != "系统提示" {
		t.Fatalf("system 消息应被保留在首位: %+v", msgs[0])
	}
	if msgs[len(msgs)-1].Content != "msg-5" {
		t.Fatalf("最新消息应保留，末条 = %q", msgs[len(msgs)-1].Content)
	}
	// 计数应反映裁剪后的真实条数。
	got, _ := s.Get(ctx, sess.ID)
	if got.MessageCount != 4 {
		t.Fatalf("MessageCount = %d，期望 4", got.MessageCount)
	}
}

// 回归测试：system 消息数超过上限时旧实现会切片越界 panic。
func TestTrimMoreSystemThanLimitDoesNotPanic(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(2)
	sess := mustCreate(t, s, "会话")

	for i := 0; i < 5; i++ {
		m := &Message{
			ID: NewMessageID(), SessionID: sess.ID, Role: RoleSystem,
			Content: fmt.Sprintf("sys-%d", i), Status: StatusComplete,
		}
		if err := s.AppendMessage(ctx, m); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}

	msgs, err := s.Messages(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("system 消息应全部保留，实际 %d 条", len(msgs))
	}
}

// ---- 快照（持久化扩展点）----

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	src, _ := newTestStore(0)
	sess := mustCreate(t, src, "可迁移会话")
	if err := src.AppendMessage(ctx, NewUserMessage(sess.ID, "你好")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	snap, err := src.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.Version != SnapshotVersion || len(snap.Sessions) != 1 || len(snap.Messages[sess.ID]) != 1 {
		t.Fatalf("快照内容不完整: %+v", snap)
	}

	dst := NewMemoryStore(0)
	if err := dst.Restore(ctx, snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got, err := dst.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("恢复后 Get: %v", err)
	}
	if got.Title != "可迁移会话" || got.MessageCount != 1 {
		t.Fatalf("恢复结果错误: %+v", got)
	}
	msgs, _ := dst.Messages(ctx, sess.ID)
	if len(msgs) != 1 || msgs[0].Content != "你好" {
		t.Fatalf("消息恢复错误: %+v", msgs)
	}
}

func TestRestoreRejectsBadVersion(t *testing.T) {
	s := NewMemoryStore(0)
	err := s.Restore(context.Background(), &Snapshot{Version: 999})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err = %v，期望 ErrInvalidArgument", err)
	}
}

// ---- 并发 ----

func TestConcurrentAppendAndRead(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(0)
	sess := mustCreate(t, s, "并发会话")

	const writers, perWriter = 16, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := s.AppendMessage(ctx, NewUserMessage(sess.ID, fmt.Sprintf("w%d-%d", id, i))); err != nil {
					t.Errorf("AppendMessage: %v", err)
					return
				}
			}
		}(w)
	}
	// 同时进行读与列表查询，检验读写锁。
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if _, err := s.Messages(ctx, sess.ID); err != nil {
					t.Errorf("Messages: %v", err)
					return
				}
				if _, _, err := s.List(ctx, ListOptions{Limit: 10}); err != nil {
					t.Errorf("List: %v", err)
					return
				}
				if _, err := s.Get(ctx, sess.ID); err != nil {
					t.Errorf("Get: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	want := writers * perWriter
	msgs, err := s.Messages(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != want {
		t.Fatalf("并发写入后消息数 = %d，期望 %d", len(msgs), want)
	}
	got, _ := s.Get(ctx, sess.ID)
	if got.MessageCount != want {
		t.Fatalf("MessageCount = %d，期望 %d（计数与消息数必须一致）", got.MessageCount, want)
	}
}

func TestConcurrentSessionCRUD(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestStore(0)

	const n = 30
	var wg sync.WaitGroup
	errCh := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sess := NewSession(fmt.Sprintf("并发-%d", i), "m")
			if err := s.Create(ctx, sess); err != nil {
				errCh <- err
				return
			}
			if _, err := s.Update(ctx, sess.ID, func(sc *Session) error {
				sc.Status = SessionRunning
				return nil
			}); err != nil {
				errCh <- err
				return
			}
			if _, err := s.Get(ctx, sess.ID); err != nil {
				errCh <- err
				return
			}
			if i%3 == 0 {
				if err := s.Delete(ctx, sess.ID); err != nil {
					errCh <- err
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("并发 CRUD 出错: %v", err)
	}

	items, _, err := s.List(ctx, ListOptions{Limit: 200})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	wantRemaining := n - (n+2)/3 // i%3==0 的个数
	if len(items) != wantRemaining {
		t.Fatalf("剩余会话 = %d，期望 %d", len(items), wantRemaining)
	}
}

// ---- ctx 取消 ----

func TestContextCancellation(t *testing.T) {
	s, _ := newTestStore(0)
	sess := mustCreate(t, s, "ctx")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.Create(ctx, NewSession("x", "m")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create err = %v，期望 context.Canceled", err)
	}
	if _, err := s.Get(ctx, sess.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get err = %v，期望 context.Canceled", err)
	}
	if _, _, err := s.List(ctx, ListOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("List err = %v，期望 context.Canceled", err)
	}
	if _, err := s.Messages(ctx, sess.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Messages err = %v，期望 context.Canceled", err)
	}
	if err := s.Delete(ctx, sess.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete err = %v，期望 context.Canceled", err)
	}
}

// ---- 领域辅助函数 ----

func TestDeriveTitle(t *testing.T) {
	now := time.Date(2025, 3, 4, 9, 30, 0, 0, time.UTC)
	cases := []struct {
		name, in, want string
	}{
		{"普通文本", "帮我设计一个 SSE 协议", "帮我设计一个 SSE 协议"},
		{"取首行", "第一行\n第二行", "第一行"},
		{"去 Markdown 前缀", "### 标题党", "标题党"},
		{"空内容回退", "   \n\n ", "新会话 · 03-04 09:30"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeriveTitle(c.in, now); got != c.want {
				t.Fatalf("DeriveTitle(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}

	long := ""
	for i := 0; i < 40; i++ {
		long += "字"
	}
	got := DeriveTitle(long, now)
	if len([]rune(got)) != titleMaxRunes+1 { // +1 为省略号
		t.Fatalf("超长标题截断后长度 = %d，期望 %d", len([]rune(got)), titleMaxRunes+1)
	}
}

func TestIDUniqueness(t *testing.T) {
	seen := make(map[string]struct{}, 2000)
	for i := 0; i < 2000; i++ {
		id := NewSessionID()
		if _, dup := seen[id]; dup {
			t.Fatalf("生成重复 ID: %q", id)
		}
		seen[id] = struct{}{}
	}
	if got := NewMessageID(); len(got) < 10 || got[:4] != "msg_" {
		t.Fatalf("NewMessageID = %q，前缀不正确", got)
	}
}
