package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/agent/loop"
	"agent_rebuild/internal/config"
	"agent_rebuild/internal/executor"
	"agent_rebuild/internal/modelconfig"
	"agent_rebuild/internal/session"
	"agent_rebuild/internal/sse"
	"agent_rebuild/internal/tool"
)

// Server 持有 HTTP 层需要的全部依赖。
// 依赖均为接口或可替换字段，便于测试时注入假实现。
type Server struct {
	cfg       *config.Config
	store     session.Store
	runner    agent.Runner
	locks     *agent.Locks
	hub       *sse.Hub
	exec      *executor.Executor
	startedAt time.Time
	// webFS 非空时用于单端口静态托管前端产物（web/dist）。
	webFS http.Handler

	// models 模型配置存储（nil 表示当前驱动不支持）。
	models modelconfig.Store
	// rebuildRunner 根据配置重建 Runner（运行时切换模型用）。
	rebuildRunner func(*modelconfig.Config) (agent.Runner, error)
	// tools 工具注册表（nil 表示未启用）。
	tools *tool.Registry
	// checkpoints 断点存储（nil 表示不支持挂起等待回答）。
	checkpoints session.CheckpointStore
}

// NewServer 组装 HTTP 层依赖。
func NewServer(cfg *config.Config, store session.Store, runner agent.Runner, locks *agent.Locks) *Server {
	hub := sse.NewHub(cfg.SSEBufferSize)
	s := &Server{
		cfg:       cfg,
		store:     store,
		runner:    runner,
		locks:     locks,
		hub:       hub,
		startedAt: time.Now(),
	}
	// 延迟读取 s.runner（而非捕获构造参数），使测试替换 Server.runner 后
	// executor 也能感知到新值，同时为将来热切换模型留出入口。
	s.exec = executor.New(func() agent.Runner { return s.runner }, hub, store)
	return s
}

// SetTools 启用工具系统。
//
// 默认审批策略：只读工具直接执行，写文件允许（核心能力），
// 命令执行需要显式开启（cfg.EnableCommandTool）。
func (s *Server) SetTools(registry *tool.Registry, systemPrompt string, maxSteps int, timeout time.Duration) {
	if registry == nil {
		return
	}
	approver := loop.AutoApprover{
		AllowModerate:  true,
		AllowDangerous: s.cfg.EnableCommandTool,
	}
	s.exec.SetTools(registry, systemPrompt, maxSteps, timeout, approver)
	s.tools = registry
}

// Tools 返回当前工具注册表（可为 nil）。
func (s *Server) Tools() *tool.Registry { return s.tools }

// SetCheckpointer 注入断点存储，启用「等待用户回答」能力。
//
// 传 nil 表示禁用挂起：交互型工具（ask_user）会退化为返回一条
// 「无法提问」的工具结果，而不是让整轮请求失败。
func (s *Server) SetCheckpointer(cp session.CheckpointStore) {
	s.checkpoints = cp
	s.exec.SetCheckpointer(cp)
}

// handleListTools 列出可用工具（供前端展示与调试）。
func (s *Server) handleListTools(w http.ResponseWriter, r *http.Request) {
	if s.tools == nil {
		respondJSON(w, r, http.StatusOK, map[string]any{"tools": []any{}})
		return
	}
	type toolInfo struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Risk        string `json:"risk"`
		// RequiresApproval 是否需要用户确认。
		RequiresApproval bool `json:"requires_approval"`
	}
	specs := s.tools.Specs()
	out := make([]toolInfo, 0, len(specs))
	for _, sp := range specs {
		out = append(out, toolInfo{
			Name:             sp.Name,
			Description:      sp.Description,
			Risk:             sp.Risk.String(),
			RequiresApproval: sp.Risk.RequiresApproval(),
		})
	}
	respondJSON(w, r, http.StatusOK, map[string]any{
		"tools": out,
		"count": len(out),
	})
}

// ServeWeb 启用单端口静态托管：请求未命中 /api 时返回 dir 下的前端产物
// （支持 SPA 回退到 index.html）。dir 为空或不存在时不启用。
func (s *Server) ServeWeb(dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	s.webFS = newSPAHandler(dir)
	slog.Info("已启用前端静态托管", "dir", dir)
}

// Handler 构建完整路由链：middleware(router)。
// 顺序（外 → 内）：requestID → logger → cors → recover → router，
// requestID 必须最外层，否则日志里的 request_id 会为空。
func (s *Server) Handler() http.Handler {
	var h http.Handler = s.routes()
	h = s.withRecover(h)
	h = s.withCORS(h)
	h = s.withLogger(h)
	h = s.withRequestID(h)
	return h
}

// CancelAllRuns 取消全部运行中的任务（优雅退出第一步）。
//
// 顺序很重要：先 cancel 让执行体有机会把 interrupted 状态落库，
// 再 Wait 等待收尾完成。
func (s *Server) CancelAllRuns() {
	count := s.exec.ActiveCount()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.exec.Shutdown(ctx); err != nil {
		slog.Warn("执行任务收尾超时", "err", err, "count", count)
		return
	}
	slog.Info("已取消运行中的生成", "count", count)
}

// Wait 等待所有任务收尾（落库完成）。
// 收尾已由 CancelAllRuns 内的 Shutdown 完成，此处保留以兼容调用方。
func (s *Server) Wait() {}

// releaseRun 任务结束时清理执行态（会话锁由调用方释放）。
func (s *Server) releaseRun(sessionID string) {
	// 会话锁与 cancel 表已由 executor 生命周期托管；
	// 这里只做日志，便于排查长任务。
	slog.Debug("执行任务已结束", "session_id", sessionID)
}

// ---- 中间件 ----

type ctxKey int

const ctxKeyRequestID ctxKey = iota

// RequestIDFrom 取请求 ID（供日志与错误体使用）。
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// withRequestID 注入 X-Request-Id：优先沿用客户端传入值，否则生成。
func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := strings.TrimSpace(r.Header.Get("X-Request-Id"))
		if rid == "" {
			rid = newRequestID()
		}
		w.Header().Set("X-Request-Id", rid)
		ctx := context.WithValue(r.Context(), ctxKeyRequestID, rid)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// withLogger 记录请求耗时与状态码。SSE 长连接的 duration 会很大，属正常现象。
func (s *Server) withLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.InfoContext(r.Context(), "http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"dur_ms", time.Since(start).Milliseconds(),
			"request_id", RequestIDFrom(r.Context()),
		)
	})
}

// withRecover 兜住 panic，避免单个请求打挂进程。
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				slog.ErrorContext(r.Context(), "panic recovered", "panic", v, "path", r.URL.Path)
				respondError(w, r, http.StatusInternalServerError, CodeInternal, "服务内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withCORS 只放行白名单来源；dev 默认放开 Vite 端口。
func (s *Server) withCORS(next http.Handler) http.Handler {
	allowed := s.cfg.CORSOrigins
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && slices.Contains(allowed, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PATCH,DELETE,OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type,Accept,X-Request-Id,Last-Event-ID")
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-Id")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// statusRecorder 记录状态码与响应字节数，且保留 Flush 能力。
type statusRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.wroteHeader = true
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush 必须转发，否则 sse.Writer 的类型断言会失败（SSE 关键）。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap 让 http.ResponseController 能拿到底层 writer。
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req_fallback"
	}
	return "req_" + hex.EncodeToString(b[:])
}
