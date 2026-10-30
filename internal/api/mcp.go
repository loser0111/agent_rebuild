package api

import (
	"errors"
	"net/http"

	"agent_rebuild/internal/customtools"
	"agent_rebuild/internal/tool/mcp"
)

// SetMCP 注入 MCP 管理器与自定义工具服务（见 docs/17-mcp-client.md）。不调用表示 MCP 未启用。
func (s *Server) SetMCP(m *mcp.Manager, ct *customtools.Service) {
	s.mcp = m
	s.customTools = ct
}

// mcpServerView = 连接状态 + 该 server 的工具（基础值 / 用户覆盖 / 生效值）。
type mcpServerView struct {
	mcp.ServerStatus
	Tools []customtools.ToolView `json:"tools"`
}

// mcpServersResponse 是 GET /api/mcp/servers 的响应。
type mcpServersResponse struct {
	// Enabled 为 false 表示 MCP_ENABLED=false；为 true 但 servers 为空表示没有配置。
	Enabled bool `json:"enabled"`
	// OverridesSupported 为 false 表示当前存储不能保存工具设置（memory 驱动），面板只读。
	OverridesSupported bool `json:"overrides_supported"`
	// ConfigPaths 读取（或尝试读取）过的配置文件：没有配置时，前端据此告诉用户该把文件放哪。
	ConfigPaths []string        `json:"config_paths"`
	Servers     []mcpServerView `json:"servers"`
}

func (s *Server) mcpServersSnapshot() mcpServersResponse {
	if s.mcp == nil {
		return mcpServersResponse{ConfigPaths: []string{}, Servers: []mcpServerView{}}
	}
	var views map[string][]customtools.ToolView
	supported := false
	if s.customTools != nil {
		views = s.customTools.ToolViews()
		supported = s.customTools.OverridesSupported()
	}
	statuses := s.mcp.Status()
	out := make([]mcpServerView, 0, len(statuses))
	for _, st := range statuses {
		tools := views[st.Name]
		if tools == nil {
			tools = []customtools.ToolView{}
		}
		out = append(out, mcpServerView{ServerStatus: st, Tools: tools})
	}
	return mcpServersResponse{
		Enabled:            true,
		OverridesSupported: supported,
		ConfigPaths:        s.mcp.ConfigPaths(),
		Servers:            out,
	}
}

// handleListMCPServers 返回所有 MCP server 的状态与工具设置（供前端面板轮询）。
func (s *Server) handleListMCPServers(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, r, http.StatusOK, s.mcpServersSnapshot())
}

// handleRestartMCPServer 让指定 server 立即重连。只发信号、不等结果，返回 202；
// 前端继续轮询状态即可看到 connecting → ready / failed。
func (s *Server) handleRestartMCPServer(w http.ResponseWriter, r *http.Request) {
	if s.mcp == nil {
		respondError(w, r, http.StatusConflict, CodeConflict, "MCP 未启用（MCP_ENABLED=false）")
		return
	}
	err := s.mcp.Restart(r.PathValue("name"))
	switch {
	case err == nil:
		respondJSON(w, r, http.StatusAccepted, map[string]any{"ok": true})
	case errors.Is(err, mcp.ErrUnknownServer):
		respondError(w, r, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, mcp.ErrNotRestartable):
		respondError(w, r, http.StatusConflict, CodeConflict, err.Error())
	default:
		respondError(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
	}
}

// handleSetToolOverride 保存一个自定义工具的风险等级 / 开关（PUT /api/tool-overrides）。
//
// 请求体 {tool: "server/原始名", risk: ""|safe|moderate|dangerous, enabled: null|true|false}：
// risk 为空 / enabled 为 null 表示该字段恢复默认（跟随 mcp.json）。整体替换语义，所以是 PUT。
// 成功后立即重建工具集，对之后新开始的对话轮次生效；返回最新的面板数据，省一次轮询。
func (s *Server) handleSetToolOverride(w http.ResponseWriter, r *http.Request) {
	if s.customTools == nil {
		respondError(w, r, http.StatusConflict, CodeConflict, "MCP 未启用（MCP_ENABLED=false）")
		return
	}
	var in customtools.Input
	if err := decodeJSON(w, r, &in); err != nil {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}
	err := s.customTools.SetOverride(r.Context(), in)
	switch {
	case err == nil:
		respondJSON(w, r, http.StatusOK, s.mcpServersSnapshot())
	case errors.Is(err, customtools.ErrInvalid):
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
	case errors.Is(err, customtools.ErrUnsupported):
		// 与 workspace / 模型配置在 memory 驱动下的处理一致：501。
		respondError(w, r, http.StatusNotImplemented, CodeInternal, err.Error())
	default:
		respondError(w, r, http.StatusInternalServerError, CodeInternal, err.Error())
	}
}
