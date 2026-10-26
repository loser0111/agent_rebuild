package loop_test

// ask_user 的挂起行为测试。
//
// 关注点（按重要性排序）：
//  1. checkpoint 里必须写下 session_id —— 这是本功能的核心要求，
//     缺了它进程重启后就找不到「哪个会话在等回答」。
//  2. 挂起时**不能**产出最终回答（否则用户会以为已经答完了）。
//  3. session_id 缺失 / 无存储时必须安全降级，
//     而不是落一条空主键的 checkpoint 或 panic。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/agent/loop"
	"agent_rebuild/internal/session"
	"agent_rebuild/internal/tool"
)

// fakeCheckpointer 是内存版 CheckpointStore，供测试断言写入内容。
type fakeCheckpointer struct {
	mu    sync.Mutex
	saved []*session.Checkpoint
	err   error
}

func (f *fakeCheckpointer) SaveCheckpoint(_ context.Context, cp *session.Checkpoint) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.saved = append(f.saved, cp.Clone())
	return nil
}

func (f *fakeCheckpointer) LoadCheckpoint(_ context.Context, _ string) (*session.Checkpoint, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.saved) - 1; i >= 0; i-- {
		if f.saved[i].State == session.CheckpointSuspended {
			return f.saved[i].Clone(), nil
		}
	}
	return nil, session.ErrCheckpointNotFound
}

func (f *fakeCheckpointer) DeleteCheckpoint(_ context.Context, _ string) error { return nil }

// last 返回最后一次保存的 checkpoint。
func (f *fakeCheckpointer) last() *session.Checkpoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.saved) == 0 {
		return nil
	}
	return f.saved[len(f.saved)-1].Clone()
}

// suspendedOf 返回保存过的 suspended checkpoint（nil 表示从未挂起）。
func (f *fakeCheckpointer) suspendedOf() *session.Checkpoint {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, cp := range f.saved {
		if cp.State == session.CheckpointSuspended {
			return cp.Clone()
		}
	}
	return nil
}

// drain 把事件通道读干。
func drain(t *testing.T, out <-chan agent.Event) []agent.Event {
	t.Helper()
	var evs []agent.Event
	timeout := time.After(3 * time.Second)
	for {
		select {
		case ev, ok := <-out:
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		case <-timeout:
			t.Fatal("等待事件超时")
			return evs
		}
	}
}

// askUserCall 构造一次 ask_user 调用。
func askUserCall(id, args string) agent.ToolCall {
	return agent.ToolCall{ID: id, Name: "ask_user", Args: args}
}

func newAskLoop(runner agent.Runner, cp session.CheckpointStore, sessionID string) *loop.Loop {
	return loop.New(runner, loop.Config{
		Registry:     tool.DefaultRegistry(tool.Options{Workspace: "/tmp"}),
		SessionID:    sessionID,
		Checkpointer: cp,
		MaxSteps:     4,
	})
}

// ---- 核心：checkpoint 必须保存 sessionId ----

func TestAskUserSuspendsAndSavesSessionID(t *testing.T) {
	cp := &fakeCheckpointer{}
	runner := &scriptedRunner{steps: []scriptedStep{{
		text:  "我需要确认一下。",
		tools: []agent.ToolCall{askUserCall("call_1", `{"question":"用哪种数据库？","options":["SQLite","Postgres"]}`)},
	}}}

	lp := newAskLoop(runner, cp, "sess_abc123")
	out := make(chan agent.Event, 64)
	res, err := lp.Run(context.Background(), nil, out)
	close(out)
	if err != nil {
		t.Fatalf("Run 不应报错（挂起是正常业务结果）: %v", err)
	}

	// 1. checkpoint 中存在 suspended 记录且带 session_id。
	got := cp.suspendedOf()
	if got == nil {
		t.Fatal("应写入一条 suspended checkpoint")
	}
	if got.SessionID != "sess_abc123" {
		t.Fatalf("checkpoint.SessionID = %q，期望 sess_abc123", got.SessionID)
	}
	// 2. 挂起详情可用于 resume。
	if len(got.Pendings) == 0 {
		t.Fatal("suspended checkpoint 必须带 Pending")
	}
	if got.Pendings[0].CallID != "call_1" {
		t.Fatalf("Pending.CallID = %q，期望 call_1", got.Pendings[0].CallID)
	}
	if got.Pendings[0].Kind != session.PendingInput {
		t.Fatalf("Pending.Kind = %q，期望 input", got.Pendings[0].Kind)
	}
	if got.Pendings[0].Question != "用哪种数据库？" {
		t.Fatalf("Pending.Question = %q", got.Pendings[0].Question)
	}
	if len(got.Pendings[0].Options) != 2 {
		t.Fatalf("Pending.Options = %v，期望 2 项", got.Pendings[0].Options)
	}

	// 3. Loop 应暴露挂起信息。
	if lp.Suspended() == nil {
		t.Fatal("Loop.Suspended() 应返回挂起信息")
	}

	// 4. 挂起时**不能**产出最终回答，否则前端会把空内容当完成。
	if res.Content != "" {
		t.Fatalf("挂起不应有最终回答，实际 %q", res.Content)
	}
	if len(res.ToolResults) != 0 {
		t.Fatalf("挂起的调用不应有工具结果，实际 %d 条", len(res.ToolResults))
	}
}

// 挂起后必须立刻停止，不能继续执行同一个 assistant 消息里的后续调用。
func TestAskUserStopsFollowingCalls(t *testing.T) {
	cp := &fakeCheckpointer{}
	runner := &scriptedRunner{steps: []scriptedStep{{
		tools: []agent.ToolCall{
			askUserCall("call_ask", `{"question":"继续吗？"}`),
			// 这一条在挂起后不应被执行。
			{ID: "call_ls", Name: "list_dir", Args: `{"path":"."}`},
		},
	}}}

	lp := newAskLoop(runner, cp, "sess_stop")
	out := make(chan agent.Event, 64)
	if _, err := lp.Run(context.Background(), nil, out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	close(out)

	if len(lp.Suspended().Pendings[0].Options) != 0 {
		t.Log("options 应为空（自由输入）")
	}
	cp.mu.Lock()
	savedCount := len(cp.saved)
	cp.mu.Unlock()
	// 只有两次写入：running（开始时）+ suspended（挂起时）。
	// 若后续调用被执行，会多出执行痕迹。
	if savedCount != 2 {
		t.Fatalf("checkpoint 写入次数 = %d，期望 2（running + suspended）", savedCount)
	}
}

// ---- 安全分支 ----

// sessionId 缺失时必须降级，不能落一条空主键的 checkpoint。
func TestAskUserWithoutSessionIDDegrades(t *testing.T) {
	cp := &fakeCheckpointer{}
	runner := &scriptedRunner{steps: []scriptedStep{
		{
			tools: []agent.ToolCall{askUserCall("call_1", `{"question":"问题？"}`)},
		},
		{
			// 降级后模型拿到「无法提问」的结果，继续给出回答。
			text: "好的，我基于现有信息继续。",
		},
	}}

	lp := newAskLoop(runner, cp, "") // ← 故意不传 sessionId
	out := make(chan agent.Event, 64)
	res, err := lp.Run(context.Background(), nil, out)
	close(out)
	if err != nil {
		t.Fatalf("缺少 sessionId 时不应报错，应降级: %v", err)
	}

	// 关键断言：一条 checkpoint 都不该写。
	if cp.last() != nil {
		t.Fatalf("无 sessionId 时不应写 checkpoint，实际写入 %+v", cp.last())
	}
	// 也不应挂起。
	if lp.Suspended() != nil {
		t.Fatal("无 sessionId 时不应挂起")
	}
	// 应把「无法提问」作为工具结果回喂，并继续到有回答。
	if len(res.ToolResults) != 1 {
		t.Fatalf("应产生 1 条降级工具结果，实际 %d", len(res.ToolResults))
	}
	if !res.ToolResults[0].IsError {
		t.Fatal("降级结果应标记 IsError")
	}
	if !strings.Contains(res.ToolResults[0].Output, "未配置 ID") {
		t.Fatalf("降级信息应说明原因，实际: %s", res.ToolResults[0].Output)
	}
	if res.Content == "" {
		t.Fatal("降级后应能产出最终回答")
	}
}

// 没有 Checkpointer 时同样降级（内存模式下就是这个路径）。
func TestAskUserWithoutCheckpointerDegrades(t *testing.T) {
	runner := &scriptedRunner{steps: []scriptedStep{
		{tools: []agent.ToolCall{askUserCall("call_1", `{"question":"问题？"}`)}},
		{text: "继续回答。"},
	}}
	lp := newAskLoop(runner, nil, "sess_x") // ← Checkpointer 为 nil
	out := make(chan agent.Event, 64)
	res, err := lp.Run(context.Background(), nil, out)
	close(out)
	if err != nil {
		t.Fatalf("无 Checkpointer 时不应报错: %v", err)
	}
	if lp.Suspended() != nil {
		t.Fatal("无 Checkpointer 时不应挂起")
	}
	if len(res.ToolResults) != 1 || !res.ToolResults[0].IsError {
		t.Fatalf("应有一条错误结果，实际: %+v", res.ToolResults)
	}
}

// 问题为空必须在挂起前拦下：否则会存一个空白 checkpoint。
func TestAskUserEmptyQuestionRejected(t *testing.T) {
	cp := &fakeCheckpointer{}
	runner := &scriptedRunner{steps: []scriptedStep{
		{tools: []agent.ToolCall{askUserCall("call_1", `{"question":"   "}`)}},
		{text: "继续。"},
	}}
	lp := newAskLoop(runner, cp, "sess_q")
	out := make(chan agent.Event, 64)
	res, err := lp.Run(context.Background(), nil, out)
	close(out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if cp.suspendedOf() != nil {
		t.Fatal("question 为空时不应挂起")
	}
	if len(res.ToolResults) != 1 || !res.ToolResults[0].IsError {
		t.Fatalf("应返回参数错误，实际: %+v", res.ToolResults)
	}
	if !strings.Contains(res.ToolResults[0].Output, "不合法") {
		t.Fatalf("错误信息应说明参数不合法，实际: %s", res.ToolResults[0].Output)
	}
}

// 存储写入失败时不能假装挂起成功。
func TestAskUserSaveFailureReturnsError(t *testing.T) {
	cp := &fakeCheckpointer{err: errSaveFailed}
	runner := &scriptedRunner{steps: []scriptedStep{
		{tools: []agent.ToolCall{askUserCall("call_1", `{"question":"问题？"}`)}},
		{text: "继续。"},
	}}
	lp := newAskLoop(runner, cp, "sess_e")
	out := make(chan agent.Event, 64)
	res, err := lp.Run(context.Background(), nil, out)
	close(out)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 写失败时必须回喂错误结果，而不是静默「挂起」但库里什么都没有。
	if len(res.ToolResults) != 1 || !res.ToolResults[0].IsError {
		t.Fatalf("写入失败应回喂错误结果，实际: %+v", res.ToolResults)
	}
	if !strings.Contains(res.ToolResults[0].Output, "保存") {
		t.Fatalf("错误信息应提到保存失败，实际: %s", res.ToolResults[0].Output)
	}
}

var errSaveFailed = &saveError{}

type saveError struct{}

func (e *saveError) Error() string { return "模拟存储故障" }

// ---- 工具本体 ----

// ask_user 必须标为 Interactive，否则 loop 不会拦截它。
func TestAskUserSpecIsInteractive(t *testing.T) {
	reg := tool.DefaultRegistry(tool.Options{Workspace: "/tmp"})
	th, ok := reg.Get("ask_user")
	if !ok {
		t.Fatal("默认工具集应包含 ask_user")
	}
	spec := th.Spec()
	if !spec.Interactive {
		t.Fatal("ask_user 必须标记 Interactive")
	}
	// 无副作用：不应因风险等级被审批拦住。
	if spec.Risk != tool.RiskSafe {
		t.Fatalf("ask_user 风险等级 = %v，期望 safe", spec.Risk)
	}
	if spec.Risk.RequiresApproval() {
		t.Fatal("ask_user 不应要求审批")
	}
	// 参数 schema 必须是合法 JSON（否则模型侧报错）。
	var probe map[string]any
	if err := json.Unmarshal(spec.Parameters, &probe); err != nil {
		t.Fatalf("parameters 不是合法 JSON: %v", err)
	}
}

// 选项需要去重、去空、限 5 个（模型经常给过多）。
//
// 上限 5 而不是 4：多问题时每个问题的选项都要能排开，
// 4 个会让「方案 A/B/C/自定义补充」这类常见组合放不下。
func TestParseAskUserArgsNormalizesOptions(t *testing.T) {
	q, opts, err := tool.ParseAskUserArgs(json.RawMessage(`{
		"question":" 选一个 ",
		"options":["A","","A","B","C","D","E","F"]
	}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if q != "选一个" {
		t.Fatalf("question 应被 trim，实际 %q", q)
	}
	if len(opts) != 5 {
		t.Fatalf("选项应被限制为 5 个，实际 %v", opts)
	}
	if opts[0] != "A" || opts[1] != "B" {
		t.Fatalf("选项顺序或去重有误: %v", opts)
	}
}

// 一次调用可含多个问题（questions 数组）——这是本次优化的核心能力。
func TestParseAskUserMultipleQuestions(t *testing.T) {
	qs, err := tool.ParseAskUserQuestions(json.RawMessage(`{
		"questions":[
			{"id":"db","question":"用哪种库？","options":["SQLite","Postgres"]},
			{"id":"scope","question":"改哪些文件？"},
			{"question":"要不要写测试？","options":["要","不要"]}
		]
	}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(qs) != 3 {
		t.Fatalf("应解析出 3 个问题，实际 %d：%+v", len(qs), qs)
	}
	if qs[0].ID != "db" || qs[1].ID != "scope" {
		t.Fatalf("显式 id 应保留，实际 %q / %q", qs[0].ID, qs[1].ID)
	}
	// 没给 id 的自动补序号，否则前端无法把答案对应回问题。
	if qs[2].ID == "" {
		t.Fatal("缺省 id 应自动生成")
	}
	if len(qs[0].Options) != 2 {
		t.Fatalf("选项丢失: %v", qs[0].Options)
	}
}

// 旧格式（单个 question + options）必须继续可用。
func TestParseAskUserLegacySingleQuestion(t *testing.T) {
	qs, err := tool.ParseAskUserQuestions(json.RawMessage(`{
		"question":"用哪种库？",
		"options":["SQLite","Postgres"]
	}`))
	if err != nil {
		t.Fatalf("旧格式解析失败: %v", err)
	}
	if len(qs) != 1 {
		t.Fatalf("旧格式应解析为 1 个问题，实际 %d", len(qs))
	}
	if qs[0].Question != "用哪种库？" {
		t.Fatalf("question = %q", qs[0].Question)
	}
}

// 超过上限应明确报错，而不是静默截断（用户会以为问题丢了）。
func TestParseAskUserTooManyQuestions(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{
		"questions": []map[string]any{
			{"question": "1"}, {"question": "2"}, {"question": "3"},
			{"question": "4"}, {"question": "5"}, {"question": "6"}, {"question": "7"},
		},
	})
	if _, err := tool.ParseAskUserQuestions(raw); err == nil {
		t.Fatal("超过 6 个问题应报错")
	}
}

// 空 question 必须被拒。
func TestParseAskUserRejectsBlankQuestion(t *testing.T) {
	if _, err := tool.ParseAskUserQuestions(json.RawMessage(`{"questions":[{"question":"   "}]}`)); err == nil {
		t.Fatal("空 question 应报错")
	}
}
