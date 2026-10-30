package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Status 是一个 server 的连接状态（见 docs/17 第 7 节状态机）。
type Status string

const (
	StatusConnecting Status = "connecting"
	StatusReady      Status = "ready"
	StatusRetrying   Status = "retrying"
	StatusFailed     Status = "failed"
	StatusDisabled   Status = "disabled"
)

// ErrUnknownServer / ErrNotRestartable 供 API 层映射 HTTP 状态码。
var (
	ErrUnknownServer  = errors.New("mcp: 未知的 server")
	ErrNotRestartable = errors.New("mcp: 该 server 已禁用或配置无效，重启无效（请修改配置后重启应用）")
)

// ServerStatus 是一个 server 的连接状态快照（不含工具明细，工具见 Catalog）。
//
// 工具明细不放在这里：面板要展示的是「基础值 + 前端覆盖 + 生效值」，覆盖存在
// customtools 包里，由 API 层把 Status 与 Catalog、覆盖三者拼起来。
type ServerStatus struct {
	Name            string     `json:"name"`
	Source          string     `json:"source"`
	Description     string     `json:"description,omitempty"`
	Command         string     `json:"command,omitempty"`
	Status          Status     `json:"status"`
	LastError       string     `json:"last_error,omitempty"`
	ServerName      string     `json:"server_name,omitempty"`
	ServerVersion   string     `json:"server_version,omitempty"`
	ProtocolVersion string     `json:"protocol_version,omitempty"`
	ToolCount       int        `json:"tool_count"`
	Restarts        int        `json:"restarts"`
	ConnectedAt     *time.Time `json:"connected_at,omitempty"`
	// Restartable 为 false 表示 disabled 或配置无效：点「重启」没用，前端不给按钮。
	Restartable bool `json:"restartable"`
}

// CatalogServer 是 Catalog 快照里的一个 server：说明、可用性、工具。
type CatalogServer struct {
	Name string
	// Description 给模型看的一句话说明：mcp.json 的 description 优先，
	// 其次是 server initialize 返回的 instructions（docs/17 12.2）。
	Description string
	// Available 仅 ready 时为 true；不可用时 Reason 说明原因（进目录，让模型如实告诉用户）。
	Available bool
	Reason    string
	// Tools 仅 Available 时非空。包含 mcp.json 关掉的工具（BaseEnabled=false）。
	Tools []CatalogTool
}

// Options 配置 Manager。
type Options struct {
	Servers []ServerConfig
	// ConfigPaths 读过（或尝试读过）的配置文件，仅用于状态展示。
	ConfigPaths []string
	// Workspace 用于解析 server 的 cwd（空 cwd = Workspace；相对 cwd 相对 Workspace）。
	Workspace string
	// OnChange 在任一 server 的工具集或可用性变化后被调用；回调里用 Catalog() 取最新快照。
	// Manager 保证它被串行调用（见 publish）。
	OnChange func()
	Logger   *slog.Logger

	// 以下为测试可调的重连参数；零值用默认。
	BackoffBase time.Duration // 默认 1s
	BackoffMax  time.Duration // 默认 30s
	MaxFailures int           // 连续失败多少次后放弃，默认 5
	StableAfter time.Duration // 稳定运行多久后清零失败计数，默认 60s
}

// Manager 管理多个 MCP server 的连接生命周期。
//
// 每个启用的 server 一个 goroutine（server.run）：连接 → 就绪 → 监听 →
// 断开后退避重连。任何一个 server 的问题都只影响它自己的工具，
// 不阻塞服务启动，也不影响内置工具与对话主流程。
type Manager struct {
	opts    Options
	log     *slog.Logger
	servers []*server // 顺序 = 配置排序后的顺序，决定工具在 Registry 里的顺序

	notifyMu sync.Mutex // 串行化 OnChange，见 publish

	startOnce sync.Once
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// NewManager 创建 Manager（不启动任何进程，见 Start）。
func NewManager(opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.BackoffBase <= 0 {
		opts.BackoffBase = time.Second
	}
	if opts.BackoffMax <= 0 {
		opts.BackoffMax = 30 * time.Second
	}
	if opts.MaxFailures <= 0 {
		opts.MaxFailures = 5
	}
	if opts.StableAfter <= 0 {
		opts.StableAfter = 60 * time.Second
	}
	m := &Manager{opts: opts, log: opts.Logger}
	for _, sc := range opts.Servers {
		// 不经 LoadFiles 构造的配置（测试、将来的 API）可能没填超时；0 会让 initialize 立刻超时。
		if sc.StartupTimeout <= 0 {
			sc.StartupTimeout = DefaultStartupTimeout
		}
		s := &server{
			m:         m,
			cfg:       sc,
			log:       opts.Logger.With("mcp_server", sc.Name),
			restartCh: make(chan struct{}, 1),
		}
		switch {
		case sc.Err != nil:
			s.status, s.lastError = StatusFailed, sc.Err.Error()
		case sc.Disabled:
			s.status = StatusDisabled
		default:
			s.status = StatusConnecting
		}
		m.servers = append(m.servers, s)
	}
	return m
}

// Start 在后台启动所有启用且配置有效的 server，立即返回（不等任何 server 就绪）。
//
// 不阻塞的理由：桌面端靠 AGENT_READY 判断后端可用，`npx -y` 首次下载可能要
// 几十秒，阻塞会让整个应用的启动时间被最慢的那个 server 拖住。
// 只会生效一次；ctx 取消等价于 Close 的前半段（停止重连）。
func (m *Manager) Start(ctx context.Context) {
	m.startOnce.Do(func() {
		ctx, m.cancel = context.WithCancel(ctx)
		for _, s := range m.servers {
			if s.cfg.Err != nil || s.cfg.Disabled {
				continue
			}
			m.wg.Add(1)
			go func(s *server) {
				defer m.wg.Done()
				s.run(ctx)
			}(s)
		}
		m.log.Info("MCP 管理器已启动", "servers", len(m.servers))
	})
}

// Close 停止所有 server 并等待子进程退出（每个最多约 2×stopGrace）。
// 应在 HTTP 服务关闭、在途请求结束之后调用：先关会让在途工具调用全部失败。
func (m *Manager) Close() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
}

// Restart 让指定 server 立即重连（清零失败计数，failed 状态也能恢复）。
// 只是发信号，不等待连接结果；进度通过 Status 观察。
func (m *Manager) Restart(name string) error {
	for _, s := range m.servers {
		if s.cfg.Name != name {
			continue
		}
		if s.cfg.Err != nil || s.cfg.Disabled {
			return ErrNotRestartable
		}
		select {
		case s.restartCh <- struct{}{}:
		default: // 已有一个待处理的重启请求，合并
		}
		return nil
	}
	return fmt.Errorf("%w: %q", ErrUnknownServer, name)
}

// ConfigPaths 返回读取（或尝试读取）过的配置文件路径。
func (m *Manager) ConfigPaths() []string { return append([]string(nil), m.opts.ConfigPaths...) }

// Status 返回所有 server 的状态快照（顺序稳定）。
func (m *Manager) Status() []ServerStatus {
	out := make([]ServerStatus, 0, len(m.servers))
	for _, s := range m.servers {
		out = append(out, s.snapshot())
	}
	return out
}

// Catalog 返回所有 server 的目录快照（顺序 = 配置排序后的顺序）。
//
// 包括不可用的 server：工具目录要把它们标成「暂不可用」告诉模型（docs/17 12.2）。
// disabled 的 server 不进目录——那是用户明确不想用的，模型没必要知道。
func (m *Manager) Catalog() []CatalogServer {
	out := make([]CatalogServer, 0, len(m.servers))
	for _, s := range m.servers {
		if s.cfg.Disabled {
			continue
		}
		s.mu.Lock()
		cs := CatalogServer{
			Name:        s.cfg.Name,
			Description: s.cfg.Description,
			Available:   s.status == StatusReady,
		}
		if cs.Description == "" {
			cs.Description = s.instructions
		}
		if cs.Available {
			cs.Tools = append([]CatalogTool(nil), s.tools...)
		} else {
			cs.Reason = unavailableReason(s.status, s.lastError)
		}
		s.mu.Unlock()
		out = append(out, cs)
	}
	return out
}

// unavailableReason 给模型看的简短原因：只取错误的第一行，stderr 尾部对模型没有用。
func unavailableReason(st Status, lastError string) string {
	first, _, _ := strings.Cut(lastError, "\n")
	first = strings.TrimSpace(first)
	if r := []rune(first); len(r) > 80 {
		first = string(r[:80]) + "…"
	}
	switch {
	case st == StatusConnecting && first == "":
		return "正在连接"
	case first == "":
		return string(st)
	default:
		return first
	}
}

// publish 通知 OnChange：工具集或可用性变了。
//
// 用 notifyMu 串行化：两个 server 几乎同时变化时，回调不会并发执行；
// 回调在锁内现取 Catalog()，后进入的那次一定看到包含双方变化的全量快照，
// 不会出现「先发生的变化后被应用、把后发生的变化覆盖掉」的乱序。
// 调用方不能持有任何 server.mu。
func (m *Manager) publish() {
	if m.opts.OnChange == nil {
		return
	}
	m.notifyMu.Lock()
	defer m.notifyMu.Unlock()
	m.opts.OnChange()
}

// resolveCwd：空 → workspace；相对路径 → 相对 workspace；workspace 也为空时继承本进程 cwd。
func (m *Manager) resolveCwd(cwd string) string {
	switch {
	case cwd == "":
		return m.opts.Workspace
	case filepath.IsAbs(cwd) || m.opts.Workspace == "":
		return cwd
	default:
		return filepath.Join(m.opts.Workspace, cwd)
	}
}

// server 是一个 MCP server 的运行时状态。mu 保护除 cfg/log/restartCh 以外的字段。
type server struct {
	m         *Manager
	cfg       ServerConfig
	log       *slog.Logger
	restartCh chan struct{}

	mu          sync.Mutex
	status      Status
	lastError   string
	tools       []CatalogTool
	info        Implementation
	protocol    string
	restarts    int
	connectedAt time.Time

	// instructions 来自 initialize（已截成一句），作为 server 说明的后备来源。
	instructions string
}

// errRestartRequested 表示本次会话因用户点「重启」而结束，不计入失败次数。
var errRestartRequested = errors.New("mcp: 用户请求重启")

// run 是一个 server 的生命周期循环，直到 ctx 取消。
func (s *server) run(ctx context.Context) {
	failures := 0
	first := true
	for ctx.Err() == nil {
		s.mu.Lock()
		if !first {
			s.restarts++
		}
		// 重连期间保留上一次的错误：面板上「connecting」旁边仍能看到为什么断的。
		s.status = StatusConnecting
		s.mu.Unlock()
		first = false

		started := time.Now()
		err := s.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errRestartRequested) {
			failures = 0
			continue
		}
		// 稳定运行过一段时间后才断开的，不算「连续失败」：否则一个偶尔崩一次的
		// server 会在几天后累计到上限被永久放弃。
		if time.Since(started) >= s.m.opts.StableAfter {
			failures = 0
		}
		failures++
		msg := errString(err)

		if failures >= s.m.opts.MaxFailures {
			s.setStatus(StatusFailed, fmt.Sprintf("连续失败 %d 次，已停止自动重连（可在 MCP 面板点「重启」）：%s", failures, msg))
			// 可用性变了也要通知：工具目录里要把它标成「暂不可用」并给出原因（docs/17 12.2）。
			s.m.publish()
			s.log.Warn("MCP server 连续失败，已放弃自动重连", "failures", failures, "err", msg)
			select {
			case <-ctx.Done():
				return
			case <-s.restartCh:
				failures = 0
				continue
			}
		}

		delay := s.m.opts.BackoffBase << (failures - 1)
		if delay > s.m.opts.BackoffMax || delay <= 0 {
			delay = s.m.opts.BackoffMax
		}
		s.setStatus(StatusRetrying, msg)
		s.m.publish()
		s.log.Warn("MCP server 连接断开，稍后重连", "attempt", failures, "delay", delay, "err", msg)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.restartCh:
			timer.Stop()
			failures = 0
		case <-timer.C:
		}
	}
}

// session 跑一次完整连接：启动进程 → 握手 → 拉工具 → 监听，直到断开。
// 返回值永远非 nil（除非 ctx 取消），描述这次连接为什么结束。
func (s *server) session(ctx context.Context) (retErr error) {
	// 丢掉连接建立前积压的重启请求：用户在 connecting 期间点的「重启」
	// 已经被这次连接满足了，不该在刚连上时再断开一次。
	select {
	case <-s.restartCh:
	default:
	}

	proc, err := startProcess(s.cfg, s.m.resolveCwd(s.cfg.Cwd), s.log)
	if err != nil {
		return err
	}

	listChanged := make(chan struct{}, 1)
	client := NewClient(proc.stdout, proc.stdin, func(n Notification) {
		switch n.Method {
		case "notifications/tools/list_changed":
			// 合并：处理一次 tools/list 期间到达的多次通知只再触发一次。
			select {
			case listChanged <- struct{}{}:
			default:
			}
		case "notifications/message":
			s.log.Debug("MCP server 日志", "params", truncateForLog(n.Params))
		}
	}, s.log)

	defer func() {
		// 顺序：先撤工具（新一轮对话看不到它们）→ 再关进程。
		// selfExited 必须在 stop 之前取，见 process.exitDetail。
		selfExited := proc.exitedOnItsOwn()
		if !selfExited && ctx.Err() == nil && !errors.Is(retErr, errRestartRequested) {
			// 连接异常结束时，进程往往正在退出（stdout 已关、Wait 还没返回）。
			// 稍等片刻再判断，否则崩溃原因里会缺「exit status N」。
			select {
			case <-proc.exited:
				selfExited = true
			case <-time.After(500 * time.Millisecond):
			}
		}
		s.clearTools()
		proc.stop()
		<-client.Done()
		if retErr != nil && !errors.Is(retErr, errRestartRequested) {
			if detail := proc.exitDetail(selfExited); detail != "" {
				retErr = fmt.Errorf("%w；%s", retErr, detail)
			}
		}
	}()

	initCtx, cancel := context.WithTimeout(ctx, s.cfg.StartupTimeout)
	err = client.Initialize(initCtx)
	if err == nil {
		err = s.refreshTools(initCtx, client)
	}
	cancel()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			return fmt.Errorf("启动超时（%s，可调大 startup_timeout_sec）: %w", s.cfg.StartupTimeout, err)
		}
		return err
	}

	s.mu.Lock()
	s.info = client.ServerInfo
	s.protocol = client.ProtocolVersion
	s.instructions = firstSentence(client.Instructions)
	s.connectedAt = time.Now()
	s.mu.Unlock()
	s.setStatus(StatusReady, "")
	s.m.publish()
	s.log.Info("MCP server 已就绪",
		"server_name", client.ServerInfo.Name, "server_version", client.ServerInfo.Version,
		"protocol", client.ProtocolVersion, "tools", s.snapshot().ToolCount)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.restartCh:
			s.log.Info("收到重启请求")
			return errRestartRequested
		case <-client.Done():
			return fmt.Errorf("连接断开: %v", client.Err())
		case <-proc.exited:
			return errors.New("server 进程退出")
		case <-listChanged:
			listCtx, cancel := context.WithTimeout(ctx, s.cfg.StartupTimeout)
			err := s.refreshTools(listCtx, client)
			cancel()
			if err != nil {
				// 刷新失败不断开：旧工具集仍然可用，下次通知再试。
				s.log.Warn("tools/list_changed 后刷新工具失败，保留旧工具集", "err", err)
				continue
			}
			s.m.publish()
			s.log.Info("MCP 工具集已刷新", "tools", s.snapshot().ToolCount)
		}
	}
}

// refreshTools 拉一次 tools/list 并替换本 server 的工具（不触发 publish）。
func (s *server) refreshTools(ctx context.Context, client *Client) error {
	remote, err := client.ListTools(ctx)
	if err != nil {
		return err
	}
	tools := buildCatalog(s.cfg, client, remote, s.log.Warn)
	s.mu.Lock()
	s.tools = tools
	s.mu.Unlock()
	return nil
}

// clearTools 撤掉本 server 的工具；之前处于 ready 才需要通知（否则全量集合没变）。
func (s *server) clearTools() {
	s.mu.Lock()
	wasReady := s.status == StatusReady
	s.tools = nil
	s.connectedAt = time.Time{}
	if wasReady {
		// 先改状态再 publish，Catalog() 才会把它标成不可用、不带工具。
		s.status = StatusConnecting
	}
	s.mu.Unlock()
	if wasReady {
		s.m.publish()
	}
}

func (s *server) setStatus(st Status, lastError string) {
	s.mu.Lock()
	s.status = st
	s.lastError = lastError
	s.mu.Unlock()
}

func (s *server) snapshot() ServerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := ServerStatus{
		Name:            s.cfg.Name,
		Source:          s.cfg.Source,
		Description:     s.cfg.Description,
		Command:         s.cfg.Command,
		Status:          s.status,
		LastError:       s.lastError,
		ServerName:      s.info.Name,
		ServerVersion:   s.info.Version,
		ProtocolVersion: s.protocol,
		ToolCount:       len(s.tools),
		Restarts:        s.restarts,
		Restartable:     s.cfg.Err == nil && !s.cfg.Disabled,
	}
	if !s.connectedAt.IsZero() {
		t := s.connectedAt
		st.ConnectedAt = &t
	}
	if st.Description == "" {
		st.Description = s.instructions
	}
	return st
}

func errString(err error) string {
	if err == nil {
		return "未知原因"
	}
	return err.Error()
}

// firstSentence 截取说明的第一句（遇到换行或句号为止，最长 120 字符）：
// instructions 可能是一整段用法文档，目录里只需要一句话。
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n。"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120]) + "…"
	}
	return strings.TrimSpace(s)
}
