package session

import "time"

// CheckpointState 是 checkpoint 的生命周期状态。
//
// 与 SessionStatus 的区别：SessionStatus 描述「会话现在能不能发消息」，
// 而 CheckpointState 描述「这一轮 agent 执行停在哪一步」。两者不是一回事：
// 一个被 ask_user 挂起的会话，会话状态仍是 running（有任务在等），
// 但 checkpoint 是 suspended（卡在某个 pending 调用上）。
type CheckpointState string

const (
	// CheckpointRunning 执行中（正常推进，未挂起）。
	CheckpointRunning CheckpointState = "running"
	// CheckpointSuspended 挂起：等待人工答复（ask_user）或审批。
	CheckpointSuspended CheckpointState = "suspended"
	// CheckpointDone 正常完成，可以清理。
	CheckpointDone CheckpointState = "done"
	// CheckpointFailed 执行失败。
	CheckpointFailed CheckpointState = "failed"
)

// PendingKind 挂起原因，决定 resume 时如何把答复转成工具结果。
type PendingKind string

const (
	// PendingInput 等待用户回答（ask_user）。
	PendingInput PendingKind = "input"
	// PendingApproval 等待用户授权（危险工具）。
	//
	// 目前审批系统尚未开放，该值仅作为协议占位，不会被写入。
	PendingApproval PendingKind = "approval"
)

// Pending 是一次挂起中、尚未产出结果的工具调用。
//
// 核心约定：**一个挂起点 = 一个还没填好的 tool result**。
// ask_user 与工具审批共用这个结构，区别只在 Kind 与 resume 后的处理。
//
// 用值而非指针：Checkpoint.Pendings 是切片，值语义让 Normalize/Clone
// 不必到处处理 nil。
type Pending struct {
	// Kind 挂起类型。
	Kind PendingKind `json:"kind"`
	// CallID 模型给出的 tool_call id，是 resume 的幂等键。
	CallID string `json:"call_id"`
	// Name 工具名。
	Name string `json:"name"`
	// Args 模型给出的原始参数（JSON 字符串）。
	Args string `json:"args"`
	// Question 要展示给用户的问题（ask_user 的 question 参数）。
	Question string `json:"question,omitempty"`
	// Options 可选项（为空表示自由输入）。
	Options []string `json:"options,omitempty"`
	// Multi 该问题是否允许多选。
	Multi bool `json:"multi,omitempty"`
	// QID 问题在本次调用内的短标识（q1/db/...）。
	//
	// 与 CallID 的区别：CallID 是「一次工具调用」，而一个调用里可能
	// 有多个问题。前端要用 QID 把答案对应回具体问题，
	// 否则多问题时答案会串。
	QID string `json:"qid,omitempty"`
}

// Clone 深拷贝 Options，避免共享底层数组。
func (p Pending) Clone() Pending {
	out := p
	if p.Options != nil {
		out.Options = append([]string(nil), p.Options...)
	}
	return out
}

// Checkpoint 是一轮 agent 执行的断点，用于「挂起后跨进程恢复」。
//
// 设计取舍：
//   - **不存消息历史**。历史已经在 messages 表里（带严格递增 seq），
//     checkpoint 再存一份就是双写，必然出现两个真相源。
//     resume 时从 messages 表重建 history。
//   - 以 SessionID 为主键，一个会话同时只有一条 checkpoint。
//     新一轮执行覆盖旧的（旧的要么已 done，要么已被用户放弃）。
//   - Completed 记录「本轮已产出结果的 call_id」，resume 时跳过，
//     让重放幂等。
type Checkpoint struct {
	// SessionID 所属会话 ID（唯一键）。为空表示无效，不允许落库。
	SessionID string `json:"session_id"`
	// Version 格式版本，便于后续迁移。
	Version int `json:"version"`
	// RunID 标识「哪一次执行」，避免旧 checkpoint 被误用于新任务。
	RunID string `json:"run_id"`
	// StepIndex 停在循环的第几步（0 基）。
	StepIndex int `json:"step_index"`
	// State 当前状态。
	State CheckpointState `json:"state"`
	// Pendings 同一轮里所有待回答的调用。
	//
	// 为什么是切片而非单个：模型经常一次抛出多个问题
	// （「你到哪一步了？」「要不要我改代码？」）。早期实现只存第一个，
	// 后面的问题落库了却没人能回答，界面上表现为「问题静默消失」。
	Pendings []Pending `json:"pendings,omitempty"`
	// Completed 已产出结果的 call_id 集合（幂等重放用）。
	Completed []string `json:"completed,omitempty"`
	// UpdatedAt 最后写入时间。
	UpdatedAt time.Time `json:"updated_at"`
}

// Normalize 把旧格式（单 pending）升级为 pendings 列表，并清理无效项。
//
// 兼容性：早期版本只写 `"pending": {...}`。老数据库里那些记录在新代码
// 下会被读成「有挂起但列表为空」，用户看到的是无法回答的卡片。这里
// 在读写两端都做一次归一化，旧数据无痛升级。
func (c *Checkpoint) Normalize() {
	if c == nil {
		return
	}
	// 保持 CallID 非空的项：没有 call_id 就无法补 tool 结果，留着只会
	// 让 resume 永远匹配不上。
	cleaned := make([]Pending, 0, len(c.Pendings))
	for _, p := range c.Pendings {
		if p.CallID == "" {
			continue
		}
		cleaned = append(cleaned, p)
	}
	c.Pendings = cleaned
}

// AddPending 追加一个挂起点（已在列表里则更新，避免重复）。
func (c *Checkpoint) AddPending(p Pending) {
	if c == nil || p.CallID == "" {
		return
	}
	for i := range c.Pendings {
		if c.Pendings[i].CallID == p.CallID {
			c.Pendings[i] = p
			return
		}
	}
	c.Pendings = append(c.Pendings, p)
}

// FindPending 按 call_id 查找挂起点。
func (c *Checkpoint) FindPending(callID string) (Pending, bool) {
	if c == nil {
		return Pending{}, false
	}
	for _, p := range c.Pendings {
		if p.CallID == callID {
			return p, true
		}
	}
	return Pending{}, false
}

// RemovePending 移除已回答的挂起点。
func (c *Checkpoint) RemovePending(callID string) {
	if c == nil {
		return
	}
	kept := make([]Pending, 0, len(c.Pendings))
	for _, p := range c.Pendings {
		if p.CallID != callID {
			kept = append(kept, p)
		}
	}
	c.Pendings = kept
}

// Validate 校验 checkpoint 的基本不变量。
//
// 关键：**SessionID 为空必须报错**。
// checkpoint 以 session_id 为主键，缺了它写入会失败或落到错误位置；
// 与其让调用方在 SQL 层看到费解的外键错误，不如在领域层直接拒绝。
func (c *Checkpoint) Validate() error {
	if c == nil {
		return ErrCheckpointInvalid
	}
	if c.SessionID == "" {
		return ErrCheckpointNoSession
	}
	if c.State == "" {
		return ErrCheckpointInvalid
	}
	// 挂起状态必须有待回答项，否则 resume 时不知道该补哪条 tool 结果，
	// 用户会看到一张「在等什么？」的空白卡片。
	if c.State == CheckpointSuspended && len(c.Pendings) == 0 {
		return ErrCheckpointInvalid
	}
	return nil
}

// IsSuspended 是否为挂起状态（且确实有待回答项）。
func (c *Checkpoint) IsSuspended() bool {
	return c != nil && c.State == CheckpointSuspended && len(c.Pendings) > 0
}

// Clone 返回深拷贝，避免调用方改动内部状态。
func (c *Checkpoint) Clone() *Checkpoint {
	if c == nil {
		return nil
	}
	out := *c
	if c.Pendings != nil {
		out.Pendings = make([]Pending, len(c.Pendings))
		for i, p := range c.Pendings {
			out.Pendings[i] = p.Clone()
		}
	}
	if c.Completed != nil {
		out.Completed = append([]string(nil), c.Completed...)
	}
	return &out
}
