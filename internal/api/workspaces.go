package api

// workspace 管理接口：会话可以绑定的「本地文件夹集合」。
//
// 设计要点（与 docs/14-workspace-and-session-presets.md 对齐）：
//   - 与模型配置一样，与内存存储并存：STORE_DRIVER=memory 时这些接口返回 501
//     （workspace 目前只有 SQLite 实现，照抄 modelconfig 的降级方式）。
//   - 这里只管「绑定关系」的 CRUD，不涉及任何工具调用的路径权限判断——
//     那是下一阶段的改动，见文档第 6 节已知限制。

import (
	"errors"
	"log/slog"
	"net/http"

	"agent_rebuild/internal/workspace"
)

// SetWorkspaceStore 注入 workspace 存储（nil 表示当前驱动不支持）。
func (s *Server) SetWorkspaceStore(store workspace.Store) {
	s.workspaces = store
}

type workspaceInput struct {
	Name    string   `json:"name"`
	Folders []string `json:"folders"`
}

// requireWorkspaces 检查 workspace 存储是否可用。
func (s *Server) requireWorkspaces(w http.ResponseWriter, r *http.Request) bool {
	if s.workspaces == nil {
		respondError(w, r, http.StatusNotImplemented, CodeInternal,
			"当前存储驱动不支持 workspace（需要 STORE_DRIVER=sqlite）")
		return false
	}
	return true
}

// handleListWorkspaces 列出全部 workspace。
func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	if !s.requireWorkspaces(w, r) {
		return
	}
	list, err := s.workspaces.List(r.Context())
	if err != nil {
		s.respondWorkspaceError(w, r, err)
		return
	}
	respondJSON(w, r, http.StatusOK, map[string]any{"workspaces": list})
}

// handleCreateWorkspace 新建 workspace。
func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	if !s.requireWorkspaces(w, r) {
		return
	}
	var req workspaceInput
	if err := decodeJSON(w, r, &req); err != nil {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}

	ws, err := s.workspaces.Create(r.Context(), workspace.Input{Name: req.Name, Folders: req.Folders})
	if err != nil {
		s.respondWorkspaceError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "workspace 已创建", "id", ws.ID, "name", ws.Name, "folders", len(ws.Folders))
	respondJSON(w, r, http.StatusCreated, ws)
}

// handleUpdateWorkspace 更新 workspace。
func (s *Server) handleUpdateWorkspace(w http.ResponseWriter, r *http.Request) {
	if !s.requireWorkspaces(w, r) {
		return
	}
	id := r.PathValue("id")
	var req workspaceInput
	if err := decodeJSON(w, r, &req); err != nil {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}

	ws, err := s.workspaces.Update(r.Context(), id, workspace.Input{Name: req.Name, Folders: req.Folders})
	if err != nil {
		s.respondWorkspaceError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "workspace 已更新", "id", ws.ID, "name", ws.Name)
	respondJSON(w, r, http.StatusOK, ws)
}

// handleDeleteWorkspace 删除 workspace。
//
// 不会级联清理引用它的会话（见 workspace.SQLStore.Delete 的注释）：
// 那些会话的 workspace_id 会变成悬空引用，等到真正使用时才报错。
func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	if !s.requireWorkspaces(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := s.workspaces.Delete(r.Context(), id); err != nil {
		s.respondWorkspaceError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "workspace 已删除", "id", id)
	respondNoContent(w)
}

// respondWorkspaceError 把 workspace 错误映射为 HTTP 状态。
func (s *Server) respondWorkspaceError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		respondError(w, r, http.StatusNotFound, CodeNotFound, "workspace 不存在")
	case errors.Is(err, workspace.ErrInvalid):
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
	default:
		slog.ErrorContext(r.Context(), "workspace 操作失败", "err", err)
		respondError(w, r, http.StatusInternalServerError, CodeInternal, "workspace 操作失败")
	}
}
