package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Code 是对外错误码（与 docs/02-api-and-sse.md §0.1 对齐）。
type Code string

const (
	CodeInvalidArgument  Code = "invalid_argument"
	CodeSessionNotFound  Code = "session_not_found"
	CodeMessageNotFound  Code = "message_not_found"
	CodeSessionBusy      Code = "session_busy"
	CodePayloadTooLarge  Code = "payload_too_large"
	CodeInternal         Code = "internal_error"
	CodeProviderUnavail  Code = "provider_unavailable"
	CodeSSEUnsupported   Code = "sse_unsupported"
	CodeMethodNotAllowed Code = "method_not_allowed"
	CodeNotFound         Code = "not_found"
	// CodeConflict 状态冲突（如 resume 时该问题已被回答 / 已无挂起项）。
	CodeConflict Code = "conflict"
)

// ErrorBody 统一错误体：{ "error": { code, message, request_id } }
type ErrorBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id,omitempty"`
	} `json:"error"`
}

// respondJSON 写出 JSON 响应。
func respondJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if payload == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.ErrorContext(r.Context(), "写响应体失败", "err", err, "path", r.URL.Path)
	}
}

// respondError 写出统一错误体。
func respondError(w http.ResponseWriter, r *http.Request, status int, code Code, msg string) {
	var body ErrorBody
	body.Error.Code = string(code)
	body.Error.Message = msg
	body.Error.RequestID = RequestIDFrom(r.Context())
	respondJSON(w, r, status, body)
}

// respondNoContent 无内容响应。
func respondNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}
