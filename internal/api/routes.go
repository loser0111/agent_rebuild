package api

import (
	"net/http"
	"strings"
	"time"
)

// routes 注册全部路由。使用 Go 1.22+ ServeMux 的 method+pattern 语法，
// 路径参数通过 r.PathValue("id") 获取。
//
// 注意：每个路径额外注册一个「不带 method」的模式，用于在方法不匹配时返回 405。
// 否则兜底的 "/" 会先命中，把 405 变成 404（Go 的 ServeMux 只在无更宽模式时才回 405）。
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// 健康检查
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("/api/health", methodNotAllowed("GET"))

	// 会话 CRUD
	mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("/api/sessions", methodNotAllowed("GET, POST"))

	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("PATCH /api/sessions/{id}", s.handlePatchSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("/api/sessions/{id}", methodNotAllowed("GET, PATCH, DELETE"))

	// 消息与流（SSE）
	mux.HandleFunc("POST /api/sessions/{id}/messages", s.handleSendMessage)
	mux.HandleFunc("/api/sessions/{id}/messages", methodNotAllowed("POST"))

	mux.HandleFunc("GET /api/sessions/{id}/stream", s.handleStream)
	mux.HandleFunc("/api/sessions/{id}/stream", methodNotAllowed("GET"))

	mux.HandleFunc("POST /api/sessions/{id}/cancel", s.handleCancel)
	mux.HandleFunc("/api/sessions/{id}/cancel", methodNotAllowed("POST"))

	// 会话恢复：回答挂起中的提问并继续生成（ask_user）。
	mux.HandleFunc("POST /api/sessions/{id}/resume", s.handleResume)
	mux.HandleFunc("/api/sessions/{id}/resume", methodNotAllowed("POST"))

	// 查询是否有挂起中的提问（前端刷新后据此恢复问答卡片）。
	mux.HandleFunc("GET /api/sessions/{id}/checkpoint", s.handleGetCheckpoint)
	mux.HandleFunc("/api/sessions/{id}/checkpoint", methodNotAllowed("GET"))

	// 模型配置（多模型支持；需要 STORE_DRIVER=sqlite）
	mux.HandleFunc("GET /api/models", s.handleListModelConfigs)
	mux.HandleFunc("POST /api/models", s.handleCreateModelConfig)
	mux.HandleFunc("/api/models", methodNotAllowed("GET, POST"))

	// 工具列表（供前端展示与调试）
	mux.HandleFunc("GET /api/tools", s.handleListTools)
	mux.HandleFunc("/api/tools", methodNotAllowed("GET"))

	mux.HandleFunc("PUT /api/models/{id}", s.handleUpdateModelConfig)
	mux.HandleFunc("DELETE /api/models/{id}", s.handleDeleteModelConfig)
	mux.HandleFunc("/api/models/{id}", methodNotAllowed("PUT, DELETE"))

	mux.HandleFunc("POST /api/models/{id}/default", s.handleSetDefaultModelConfig)
	mux.HandleFunc("/api/models/{id}/default", methodNotAllowed("POST"))

	mux.HandleFunc("POST /api/models/{id}/test", s.handleTestModelConfig)
	mux.HandleFunc("/api/models/{id}/test", methodNotAllowed("POST"))

	// 兜底：/api 下未匹配路径返回 JSON 404，
	// 其余路径交给前端静态资源（单端口部署）或返回 JSON 404。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			respondError(w, r, http.StatusNotFound, CodeNotFound, "接口不存在: "+r.URL.Path)
			return
		}
		if s.webFS != nil {
			s.webFS.ServeHTTP(w, r)
			return
		}
		respondError(w, r, http.StatusNotFound, CodeNotFound, "接口不存在: "+r.URL.Path)
	})

	return mux
}

// methodNotAllowed 返回一个始终响应 405 的 handler，并带上 Allow 头。
func methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		respondError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			"该方法不被支持: "+r.Method)
	}
}

// handleHealth 健康检查，暴露版本、协议与当前 provider。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, r, http.StatusOK, map[string]any{
		"status": "ok",
		// protocol 与 provider 是两个维度：前者决定用哪个 Runner，
		// 后者决定端点与默认模型。分开暴露便于排查「配了却没生效」。
		"protocol":       s.cfg.LLMProtocol,
		"provider":       s.cfg.LLMProvider,
		"model":          s.cfg.LLMModel,
		"version":        Version,
		"env":            s.cfg.Env,
		"uptime_seconds": int(time.Since(s.startedAt).Seconds()),
	})
}

// Version 服务版本号，构建时可用 -ldflags 覆盖。
var Version = "0.1.0"
