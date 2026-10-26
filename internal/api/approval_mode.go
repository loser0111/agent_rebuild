package api

import (
	"net/http"

	"agent_rebuild/internal/config"
)

// approvalModeOptions 固定三档，顺序即前端下拉展示顺序（manual 最保守，排最前）。
var approvalModeOptions = []string{"manual", "model", "auto"}

// approvalModeResponse GET/PUT /api/approval-mode 共用的响应体。
type approvalModeResponse struct {
	Mode    string   `json:"mode"`
	Options []string `json:"options"`
	// ToolsEnabled 为 false 时 mode 恒为空字符串——没有工具系统就没有
	// 审批这件事，前端应据此隐藏切换控件而不是展示一个空值当作错误。
	ToolsEnabled bool `json:"tools_enabled"`
}

// handleGetApprovalMode 查询当前生效的审批模式（供前端渲染切换控件初始值）。
func (s *Server) handleGetApprovalMode(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, r, http.StatusOK, approvalModeResponse{
		Mode:         string(s.ApprovalMode()),
		Options:      approvalModeOptions,
		ToolsEnabled: s.Tools() != nil,
	})
}

// approvalModeRequest PUT /api/approval-mode 的请求体。
type approvalModeRequest struct {
	Mode string `json:"mode"`
}

// handleSetApprovalMode 运行时切换审批模式（manual/model/auto），立即生效。
//
// 为什么是 PUT 不是 POST：这是对「当前审批模式」这一资源的整体替换，
// 语义上与 PUT /api/models/{id} 一致，而不是"新建一次切换记录"。
func (s *Server) handleSetApprovalMode(w http.ResponseWriter, r *http.Request) {
	var req approvalModeRequest
	if err := decodeJSON(w, r, &req); err != nil {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}
	if err := s.SetApprovalMode(config.ApprovalMode(req.Mode)); err != nil {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}
	respondJSON(w, r, http.StatusOK, approvalModeResponse{
		Mode:         string(s.ApprovalMode()),
		Options:      approvalModeOptions,
		ToolsEnabled: s.Tools() != nil,
	})
}
