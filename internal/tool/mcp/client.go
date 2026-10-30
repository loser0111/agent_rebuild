package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
)

// 协议常量。
const (
	// ProtocolVersion 是 initialize 时请求的协议版本。
	//
	// server 可以回一个不同的版本（规范允许协商）；我们只用到 tools 子集，
	// 这部分在 2024-11-05 之后的各版本里形状兼容，所以对方回什么版本都接受，
	// 只记录下来供状态面板展示，不因版本不同而断开。
	ProtocolVersion = "2025-06-18"

	// maxMessageBytes 单条 JSON-RPC 消息上限。工具结果里可能有 base64 图片，
	// bufio.Scanner 默认 64KB 远远不够；但也不能无上限——一个不守规矩的 server
	// 往 stdout 灌一条永不结束的行，会把内存吃光。
	maxMessageBytes = 16 << 20

	// JSON-RPC 标准错误码。
	codeMethodNotFound = -32601
)

// ErrClosed 表示连接已关闭（进程退出 / 读流断开 / 主动 Close）。
var ErrClosed = errors.New("mcp: 连接已关闭")

// errLineTooLong 是 readLine 内部用的哨兵：该行超限、已被整体丢弃。
var errLineTooLong = errors.New("mcp: 消息超过大小上限")

// RPCError 是 server 返回的 JSON-RPC error 对象。
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("MCP server 返回错误 %d: %s", e.Code, e.Message)
}

// message 是一条入站 JSON-RPC 消息（请求、响应、通知共用一个结构解码）。
//
// 区分方式：有 method 且有 id → server 发来的请求；有 method 无 id → 通知；
// 无 method → 对我们某个请求的响应。id 用 RawMessage 保留原样：
// 规范允许字符串或数字，回复 server 的请求时必须原样带回。
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// outbound 是一条出站消息。Params / Result 用 any，由 json.Marshal 统一编码。
type outbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Notification 是 server 发来的一条通知。
type Notification struct {
	Method string
	Params json.RawMessage
}

// Client 是一条 MCP 会话：在一对 reader/writer 上跑换行分帧的 JSON-RPC 2.0。
//
// 与 transport 解耦：它只认 io.Reader / io.Writer，stdio 子进程由 stdio.go 管，
// 单元测试可以直接用 io.Pipe 驱动。将来换官方 SDK 时，替换的就是这个类型。
//
// 并发模型：一个后台 goroutine（readLoop）独占读端，按 id 把响应分发给等待中的
// 调用方；写端由 writeMu 串行化，保证多条并发请求不会在一行里交错。
type Client struct {
	w       io.Writer
	writeMu sync.Mutex

	nextID atomic.Int64

	mu      sync.Mutex
	pending map[string]chan *message // id → 等待响应的 channel（缓冲 1）
	closed  bool
	err     error // 读循环结束的原因

	done     chan struct{}
	onNotify func(Notification)
	log      *slog.Logger

	// 握手结果（Initialize 之后只读）。
	ServerInfo      Implementation
	ProtocolVersion string
	Capabilities    ServerCapabilities
	// Instructions 是 server 在 initialize 里给出的用法说明（规范 2025-03-26 起的可选字段），
	// 用作工具目录里 server 说明的后备来源（docs/17 12.2）。
	Instructions string
}

// Implementation 是 initialize 双方互报的实现信息。
type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ServerCapabilities 只解析我们关心的部分。
type ServerCapabilities struct {
	Tools *struct {
		ListChanged bool `json:"listChanged"`
	} `json:"tools,omitempty"`
}

// NewClient 创建会话并启动读循环。onNotify 在读循环 goroutine 里被调用，
// **不能阻塞**（阻塞会让所有响应都收不到）；nil 表示忽略通知。
func NewClient(r io.Reader, w io.Writer, onNotify func(Notification), log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	c := &Client{
		w:        w,
		pending:  make(map[string]chan *message),
		done:     make(chan struct{}),
		onNotify: onNotify,
		log:      log,
	}
	go c.readLoop(bufio.NewReaderSize(r, 64<<10))
	return c
}

// Done 在读循环结束（对端关闭 / 读出错）后关闭。
func (c *Client) Done() <-chan struct{} { return c.done }

// Err 返回读循环结束的原因（Done 关闭前为 nil）。
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// call 发送一个请求并等待响应。
//
// ctx 取消时会向 server 发 notifications/cancelled（规范的取消方式），
// 让对方有机会停掉手里的活；之后即使响应到达也会被丢弃。
func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	id := strconv.FormatInt(c.nextID.Add(1), 10)
	ch := make(chan *message, 1)

	c.mu.Lock()
	if c.closed {
		err := c.err
		c.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrClosed, err)
	}
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.send(outbound{JSONRPC: "2.0", ID: json.RawMessage(id), Method: method, Params: params}); err != nil {
		return err
	}

	select {
	case msg := <-ch:
		return decodeResult(msg, result)
	case <-c.done:
		// 读循环已结束；但响应可能恰好在结束前送达，先非阻塞取一次。
		select {
		case msg := <-ch:
			return decodeResult(msg, result)
		default:
		}
		return fmt.Errorf("%w: %v", ErrClosed, c.Err())
	case <-ctx.Done():
		// 尽力通知取消，失败无所谓（连接可能已经坏了）。
		// 规范规定 initialize 请求不得被取消：它超时意味着整条连接都要重建，不必通知。
		if method != "initialize" {
			_ = c.notify("notifications/cancelled", map[string]any{
				"requestId": json.RawMessage(id),
				"reason":    ctx.Err().Error(),
			})
		}
		return ctx.Err()
	}
}

func decodeResult(msg *message, result any) error {
	if msg.Error != nil {
		return msg.Error
	}
	if result == nil || len(msg.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(msg.Result, result); err != nil {
		return fmt.Errorf("mcp: 解析响应失败: %w", err)
	}
	return nil
}

// notify 发送一条通知（没有 id，不等响应）。
func (c *Client) notify(method string, params any) error {
	return c.send(outbound{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *Client) send(m outbound) error {
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("mcp: 编码请求失败: %w", err)
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.w.Write(b); err != nil {
		return fmt.Errorf("%w: 写入失败: %v", ErrClosed, err)
	}
	return nil
}

// readLoop 独占读端，直到 EOF 或读错误。
func (c *Client) readLoop(br *bufio.Reader) {
	var loopErr error
	for {
		line, err := readLine(br, maxMessageBytes)
		if errors.Is(err, errLineTooLong) {
			c.log.Warn("MCP server 输出了超过上限的消息，已丢弃", "limit_bytes", maxMessageBytes)
			continue
		}
		if len(line) > 0 {
			c.dispatch(line)
		}
		if err != nil {
			loopErr = err // io.EOF = 对端正常关闭
			break
		}
	}
	c.shutdown(loopErr)
}

// readLine 读一行（不含换行符）。超过 limit 的行会被读完并丢弃，返回 errLineTooLong，
// 读端仍停在下一行开头，连接可以继续使用。
func readLine(br *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > limit {
				tooLong = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if tooLong {
			if err != nil {
				return nil, err
			}
			return nil, errLineTooLong
		}
		return bytes.TrimRight(buf, "\r\n"), err
	}
}

// dispatch 处理一行输入：可能是单条消息，也可能是旧版本规范允许的批量数组。
func (c *Client) dispatch(line []byte) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return
	}
	if line[0] == '[' {
		// 2025-03-26 规范允许 JSON-RPC batch，2025-06-18 移除了。兼容一下成本很低。
		var batch []json.RawMessage
		if err := json.Unmarshal(line, &batch); err != nil {
			c.log.Warn("MCP server 输出了无法解析的批量消息，已跳过", "err", err)
			return
		}
		for _, item := range batch {
			c.dispatchOne(item)
		}
		return
	}
	c.dispatchOne(line)
}

func (c *Client) dispatchOne(raw []byte) {
	var msg message
	if err := json.Unmarshal(raw, &msg); err != nil {
		// 不守规矩的 server 会把日志打到 stdout。跳过而不是断开：
		// 断开会让整个 server 不可用，而这行日志本身无害。
		c.log.Warn("MCP server 在 stdout 输出了非 JSON-RPC 内容，已跳过",
			"line", truncateForLog(raw), "err", err)
		return
	}

	hasID := len(msg.ID) > 0 && string(msg.ID) != "null"
	switch {
	case msg.Method != "" && hasID:
		c.handleRequest(&msg)
	case msg.Method != "":
		if c.onNotify != nil {
			c.onNotify(Notification{Method: msg.Method, Params: msg.Params})
		}
	case hasID:
		key := normalizeID(msg.ID)
		c.mu.Lock()
		ch, ok := c.pending[key]
		c.mu.Unlock()
		if ok {
			// 非阻塞：缓冲 1 已足够正常情况；server 对同一 id 重复回复时，
			// 阻塞发送会把整个读循环卡死，宁可丢掉重复的那条。
			select {
			case ch <- &msg:
			default:
			}
		} else {
			c.log.Debug("收到未知或已超时请求的响应，已丢弃", "id", key)
		}
	default:
		c.log.Warn("MCP server 输出了既无 id 也无 method 的消息，已跳过", "line", truncateForLog(raw))
	}
}

// normalizeID 把响应里的 id 规整成与 pending 键相同的形式。
// 我们发出去的是数字 1、2、3……；个别 server 会回成字符串 "1"，两种都认。
func normalizeID(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(bytes.TrimSpace(raw))
}

// handleRequest 应答 server 发来的反向请求。
//
// 我们在 initialize 里没有声明 sampling / roots / elicitation 能力，规范上
// server 不该发这些；但真发了也要回一个错误，否则对方会一直等。
//
// 回复放到独立 goroutine 里写，不能在读循环里同步写：若某个并发的大请求
// 正卡在写 stdin（server 忙着写 stdout、没读 stdin，管道满了），读循环
// 去抢 writeMu 就会停止读 stdout，双方互相等待，连接永久卡死。
func (c *Client) handleRequest(msg *message) {
	go c.reply(msg)
}

func (c *Client) reply(msg *message) {
	out := outbound{JSONRPC: "2.0", ID: msg.ID}
	switch msg.Method {
	case "ping":
		out.Result = struct{}{}
	default:
		out.Error = &RPCError{Code: codeMethodNotFound, Message: "client 不支持方法 " + msg.Method}
	}
	if err := c.send(out); err != nil {
		c.log.Debug("回复 MCP server 请求失败", "method", msg.Method, "err", err)
	}
}

// shutdown 标记连接关闭并唤醒所有等待者。只会执行一次（readLoop 只退出一次）。
func (c *Client) shutdown(err error) {
	c.mu.Lock()
	c.closed = true
	c.err = err
	c.mu.Unlock()
	close(c.done)
}

func truncateForLog(b []byte) string {
	const limit = 200
	if len(b) > limit {
		return string(b[:limit]) + "…"
	}
	return string(b)
}

// ---- MCP 方法 ----

// ClientName / ClientVersion 是 initialize 时自报的实现信息。
const (
	ClientName    = "agent_rebuild"
	ClientVersion = "0.1.0"
)

// Initialize 完成握手：initialize 请求 + notifications/initialized 通知。
func (c *Client) Initialize(ctx context.Context) error {
	var res struct {
		ProtocolVersion string             `json:"protocolVersion"`
		Capabilities    ServerCapabilities `json:"capabilities"`
		ServerInfo      Implementation     `json:"serverInfo"`
		Instructions    string             `json:"instructions"`
	}
	err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		// 只实现 tools 的消费侧：不声明 roots / sampling / elicitation，
		// server 就不会（也不该）向我们发这些请求。
		"capabilities": map[string]any{},
		"clientInfo":   Implementation{Name: ClientName, Version: ClientVersion},
	}, &res)
	if err != nil {
		return fmt.Errorf("initialize 失败: %w", err)
	}
	c.ServerInfo = res.ServerInfo
	c.ProtocolVersion = res.ProtocolVersion
	c.Capabilities = res.Capabilities
	c.Instructions = res.Instructions
	if res.ProtocolVersion != "" && res.ProtocolVersion != ProtocolVersion {
		c.log.Info("MCP server 协商了不同的协议版本，按兼容方式继续",
			"requested", ProtocolVersion, "server", res.ProtocolVersion)
	}
	if err := c.notify("notifications/initialized", nil); err != nil {
		return fmt.Errorf("发送 initialized 通知失败: %w", err)
	}
	return nil
}

// RemoteTool 是 tools/list 返回的一个工具。
type RemoteTool struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	InputSchema json.RawMessage  `json:"inputSchema,omitempty"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations 是 server 自报的工具行为提示。
//
// 规范明确：这些只是提示，客户端不得据此做安全决策，除非信任该 server。
// 用指针区分「没说」和「说了 false」：destructiveHint 规范默认值是 true，
// 没说 ≠ 说了 false。
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// maxListPages 防止 server 返回的 nextCursor 永远不为空导致死循环。
const maxListPages = 100

// ListTools 拉取完整工具列表（自动翻页）。
func (c *Client) ListTools(ctx context.Context) ([]RemoteTool, error) {
	var all []RemoteTool
	cursor := ""
	for page := 0; page < maxListPages; page++ {
		var params any
		if cursor != "" {
			params = map[string]any{"cursor": cursor}
		}
		var res struct {
			Tools      []RemoteTool `json:"tools"`
			NextCursor string       `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &res); err != nil {
			return nil, fmt.Errorf("tools/list 失败: %w", err)
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			return all, nil
		}
		cursor = res.NextCursor
	}
	c.log.Warn("tools/list 翻页超过上限，使用已拿到的部分", "pages", maxListPages, "tools", len(all))
	return all, nil
}

// ContentBlock 是 CallToolResult.content 的一个元素。只解析渲染需要的字段。
type ContentBlock struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	Data     string            `json:"data,omitempty"`
	MimeType string            `json:"mimeType,omitempty"`
	URI      string            `json:"uri,omitempty"`
	Name     string            `json:"name,omitempty"`
	Resource *EmbeddedResource `json:"resource,omitempty"`
}

// EmbeddedResource 是 type=resource 的内嵌资源。
type EmbeddedResource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// CallResult 是 tools/call 的结果。
type CallResult struct {
	Content           []ContentBlock  `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// CallTool 调用一个远端工具。name 是远端**原始**工具名。
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (*CallResult, error) {
	if len(bytes.TrimSpace(args)) == 0 || string(bytes.TrimSpace(args)) == "null" {
		// 规范要求 arguments 是对象；模型对无参工具常给空串，补成 {}。
		args = json.RawMessage(`{}`)
	}
	var res CallResult
	if err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}
