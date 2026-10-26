package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agent_rebuild/internal/modelconfig"
	"agent_rebuild/internal/session"
)

// maxRequestBody 单个 JSON 请求体上限。
const maxRequestBody = 1 << 20 // 1MB

// ---- 请求 / 响应结构（与 docs/02-api-and-sse.md 对齐）----

type createSessionRequest struct {
	Title        string `json:"title"`
	Model        string `json:"model"`
	SystemPrompt string `json:"system_prompt"`
}

type createSessionResponse struct {
	Session *session.Session `json:"session"`
}

type listSessionsResponse struct {
	Sessions   []*session.Session `json:"sessions"`
	NextCursor *string            `json:"next_cursor"`
}

type sessionDetailResponse struct {
	Session     *session.Session  `json:"session"`
	Messages    []session.Message `json:"messages"`
	LastEventID uint64            `json:"last_event_id"`
}

type patchSessionRequest struct {
	Title *string `json:"title"`
	Model *string `json:"model"`
	// ModelConfigID 切换会话使用的模型配置；传空串表示回退到全局默认。
	ModelConfigID *string `json:"model_config_id"`
}

// ---- handlers ----

// handleCreateSession 创建会话。
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req createSessionRequest
	// 允许空 body（前端「新建会话」按钮通常不带内容）。
	if err := decodeJSONOptional(w, r, &req); err != nil {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}

	model := req.Model
	if model == "" {
		model = s.cfg.LLMModel
	}
	sess := session.NewSessionAt(req.Title, model, time.Now().UTC())

	if err := s.store.Create(r.Context(), sess); err != nil {
		s.respondStoreError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "会话已创建", "session_id", sess.ID, "model", sess.Model)
	respondJSON(w, r, http.StatusCreated, createSessionResponse{Session: sess})
}

// handleListSessions 列出会话（updated_at 倒序，游标分页）。
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, "limit 必须是正整数")
			return
		}
		limit = n
	}

	items, cursor, err := s.store.List(r.Context(), session.ListOptions{
		Limit:  limit,
		Cursor: r.URL.Query().Get("cursor"),
	})
	if err != nil {
		s.respondStoreError(w, r, err)
		return
	}

	resp := listSessionsResponse{Sessions: items}
	if cursor != "" {
		resp.NextCursor = &cursor
	}
	respondJSON(w, r, http.StatusOK, resp)
}

// handleGetSession 会话详情（含历史消息与当前事件流位置）。
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	sess, err := s.store.Get(ctx, id)
	if err != nil {
		s.respondStoreError(w, r, err)
		return
	}
	msgs, err := s.store.Messages(ctx, id)
	if err != nil {
		s.respondStoreError(w, r, err)
		return
	}
	// 展示给用户前同样修一下滞留 streaming 的消息状态，避免刷新后
	// 某条回复内容明明完整却还显示"生成中"。纯内存操作，不写数据库。
	// 悬空 tool_calls（含 ask_user 挂起）不在这里处理，交给前端
	// rebuildToolCalls 结合 Checkpoint.Pending 区分"等待回答"与"真丢失"。
	msgs = session.RepairHistory(msgs)
	respondJSON(w, r, http.StatusOK, sessionDetailResponse{
		Session:     sess,
		Messages:    msgs,
		LastEventID: s.hub.LastID(id),
	})
}

// handlePatchSession 局部更新（当前支持标题与模型）。
func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req patchSessionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}
	if req.Title == nil && req.Model == nil && req.ModelConfigID == nil {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument,
			"至少需要提供 title、model 或 model_config_id")
		return
	}

	// 切换模型配置前先确认它存在，否则会话会指向一个已删除的配置，
	// 直到下次发消息才报错，用户很难定位。
	if req.ModelConfigID != nil && *req.ModelConfigID != "" {
		if s.models == nil {
			respondError(w, r, http.StatusNotImplemented, CodeInternal,
				"当前存储驱动不支持模型配置（需要 STORE_DRIVER=sqlite）")
			return
		}
		if _, err := s.models.Get(r.Context(), *req.ModelConfigID); err != nil {
			if errors.Is(err, modelconfig.ErrNotFound) {
				respondError(w, r, http.StatusNotFound, CodeNotFound, "模型配置不存在")
				return
			}
			s.respondModelError(w, r, err)
			return
		}
	}

	updated, err := s.store.Update(r.Context(), id, func(sc *session.Session) error {
		if req.Title != nil {
			if len([]rune(*req.Title)) > 200 {
				return errors.New("标题过长（最多 200 字）")
			}
			sc.Title = session.NormalizeTitle(*req.Title, time.Now().UTC())
		}
		if req.Model != nil && *req.Model != "" {
			sc.Model = *req.Model
		}
		if req.ModelConfigID != nil {
			sc.ModelConfigID = strings.TrimSpace(*req.ModelConfigID)
		}
		return nil
	})
	if err != nil {
		s.respondStoreError(w, r, err)
		return
	}
	respondJSON(w, r, http.StatusOK, createSessionResponse{Session: updated})
}

// handleDeleteSession 删除会话：先中断运行中的任务，再删数据与事件缓冲。
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// 任务可能在后台运行（连接已断也继续），必须先取消。
	s.exec.Stop(id)
	if err := s.store.Delete(r.Context(), id); err != nil {
		s.respondStoreError(w, r, err)
		return
	}
	s.hub.Drop(id)
	s.locks.Forget(id)
	slog.InfoContext(r.Context(), "会话已删除", "session_id", id)
	respondNoContent(w)
}

// ---- 辅助 ----

// decodeJSON 严格解析 JSON 请求体（限制大小、拒绝未知字段之外的多余内容）。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	return decodeBody(w, r, dst, false)
}

// decodeJSONOptional 允许空 body。
func decodeJSONOptional(w http.ResponseWriter, r *http.Request, dst any) error {
	return decodeBody(w, r, dst, true)
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any, optional bool) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) && optional {
			return nil
		}
		if errors.Is(err, io.EOF) {
			return errors.New("请求体不能为空")
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return errors.New("请求体超过 1MB 限制")
		}
		return errors.New("请求体不是合法 JSON: " + err.Error())
	}
	return nil
}

// respondStoreError 把存储层错误映射为 HTTP 状态码。
func (s *Server) respondStoreError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, session.ErrNotFound):
		respondError(w, r, http.StatusNotFound, CodeSessionNotFound, "会话不存在")
	case errors.Is(err, session.ErrConflict):
		respondError(w, r, http.StatusConflict, CodeInvalidArgument, "资源冲突")
	case errors.Is(err, session.ErrInvalidArgument):
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
	case errors.Is(err, session.ErrInvalidCursor):
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, "游标无效")
	case errors.Is(err, context.Canceled):
		// 客户端主动断开：记录日志即可，此时连接通常已不可写。
		slog.DebugContext(r.Context(), "请求被客户端取消", "path", r.URL.Path)
	case errors.Is(err, context.DeadlineExceeded):
		respondError(w, r, http.StatusGatewayTimeout, CodeInternal, "请求超时")
	default:
		slog.ErrorContext(r.Context(), "存储层错误", "err", err, "path", r.URL.Path)
		respondError(w, r, http.StatusInternalServerError, CodeInternal, "服务内部错误")
	}
}
