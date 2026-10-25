package api

// 模型配置管理接口（多模型支持）。
//
// 设计要点：
//   - API Key 只写不读：GET 接口返回 has_api_key 布尔值，永不返回明文
//   - 切换默认配置会实时重建 Runner，无需重启服务
//   - 与内存存储并存：STORE_DRIVER=memory 时这些接口返回 501

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"agent_rebuild/internal/agent"
	"agent_rebuild/internal/config"
	"agent_rebuild/internal/modelconfig"
	"agent_rebuild/internal/session"
)

// sessionMessage 是 session.Message 的别名，避免与包名冲突的冗长写法。
type sessionMessage = session.Message

// contextWithTimeout 从请求派生带超时的 context。
func contextWithTimeout(r *http.Request, seconds int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), time.Duration(seconds)*time.Second)
}

// newProbeMessage 构造连通性测试用的消息。
func newProbeMessage(content string) session.Message {
	return session.Message{
		ID:      "probe",
		Role:    session.RoleUser,
		Content: content,
		Status:  session.StatusComplete,
	}
}

// errorsIs 是 errors.Is 的简写，便于本文件内使用。
func errorsIs(err, target error) bool { return errors.Is(err, target) }

// SetModelConfigStore 注入模型配置存储与 Runner 重建器。
//
// rebuild 用于在默认配置变化后重建 Runner（协议/地址/密钥都可能变）。
func (s *Server) SetModelConfigStore(store modelconfig.Store, rebuild func(*modelconfig.Config) (agent.Runner, error)) {
	s.models = store
	s.rebuildRunner = rebuild

	// 注入「按模型配置 ID 解析 Runner」的能力，使每个请求可以选用不同模型。
	//
	// 这里刻意做成每次执行都重新查库 + 重新构造 Runner：
	//   - 用户改了密钥/地址后立即生效，无需重启
	//   - 配置被删除时能明确报错，而不是静默用默认模型
	//   - 缓存留给下层（SQLite 很快），不在这里引入失效问题
	if store != nil && rebuild != nil {
		s.exec.SetResolver(func(modelConfigID string) (agent.Runner, error) {
			mc, err := store.Get(context.Background(), modelConfigID)
			if err != nil {
				if errors.Is(err, modelconfig.ErrNotFound) {
					return nil, fmt.Errorf("模型配置已不存在（可能已被删除）")
				}
				return nil, err
			}
			return rebuild(mc)
		})
	}
}

type modelConfigInput struct {
	Name         string   `json:"name"`
	Protocol     string   `json:"protocol"`
	Provider     string   `json:"provider"`
	BaseURL      string   `json:"base_url"`
	Model        string   `json:"model"`
	SystemPrompt string   `json:"system_prompt"`
	Temperature  *float64 `json:"temperature"`
	MaxTokens    int      `json:"max_tokens"`
	// APIKey 只写。更新时留空表示不改动。
	APIKey    string `json:"api_key"`
	IsDefault bool   `json:"is_default"`
}

type modelConfigListResponse struct {
	Configs []*modelconfig.Config `json:"configs"`
	// CurrentProtocol 当前生效的协议（便于前端展示"正在使用哪个"）。
	CurrentProtocol string `json:"current_protocol"`
	CurrentModel    string `json:"current_model"`
}

// requireModels 检查模型配置存储是否可用。
func (s *Server) requireModels(w http.ResponseWriter, r *http.Request) bool {
	if s.models == nil {
		respondError(w, r, http.StatusNotImplemented, CodeInternal,
			"当前存储驱动不支持模型配置（需要 STORE_DRIVER=sqlite）")
		return false
	}
	return true
}

// handleListModelConfigs 列出全部模型配置。
func (s *Server) handleListModelConfigs(w http.ResponseWriter, r *http.Request) {
	if !s.requireModels(w, r) {
		return
	}
	list, err := s.models.List(r.Context())
	if err != nil {
		s.respondModelError(w, r, err)
		return
	}
	respondJSON(w, r, http.StatusOK, modelConfigListResponse{
		Configs:         list,
		CurrentProtocol: s.cfg.LLMProtocol,
		CurrentModel:    s.cfg.LLMModel,
	})
}

// handleCreateModelConfig 新建配置。
func (s *Server) handleCreateModelConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireModels(w, r) {
		return
	}
	var req modelConfigInput
	if err := decodeJSON(w, r, &req); err != nil {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}

	cfg, err := s.models.Create(r.Context(), toModelInput(req))
	if err != nil {
		s.respondModelError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "模型配置已创建",
		"id", cfg.ID, "name", cfg.Name, "protocol", cfg.Protocol, "model", cfg.Model)

	// 若新建的是默认配置，立即生效。
	if cfg.IsDefault {
		if applyErr := s.applyModelConfig(cfg); applyErr != nil {
			slog.WarnContext(r.Context(), "配置已保存但未生效", "err", applyErr)
		}
	}
	respondJSON(w, r, http.StatusCreated, cfg)
}

// handleUpdateModelConfig 更新配置。
func (s *Server) handleUpdateModelConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireModels(w, r) {
		return
	}
	id := r.PathValue("id")
	var req modelConfigInput
	if err := decodeJSON(w, r, &req); err != nil {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
		return
	}

	cfg, err := s.models.Update(r.Context(), id, toModelInput(req))
	if err != nil {
		s.respondModelError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "模型配置已更新", "id", cfg.ID, "name", cfg.Name)
	if cfg.IsDefault {
		if applyErr := s.applyModelConfig(cfg); applyErr != nil {
			slog.WarnContext(r.Context(), "配置已保存但未生效", "err", applyErr)
		}
	}
	respondJSON(w, r, http.StatusOK, cfg)
}

// handleDeleteModelConfig 删除配置。
func (s *Server) handleDeleteModelConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireModels(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := s.models.Delete(r.Context(), id); err != nil {
		s.respondModelError(w, r, err)
		return
	}
	slog.InfoContext(r.Context(), "模型配置已删除", "id", id)

	// 删除后若存在新的默认配置，立即生效。
	if def, err := s.models.GetDefault(r.Context()); err == nil {
		if applyErr := s.applyModelConfig(def); applyErr != nil {
			slog.WarnContext(r.Context(), "新默认配置未能生效", "err", applyErr)
		}
	}
	respondNoContent(w)
}

// handleSetDefaultModelConfig 设为默认（并立即生效）。
func (s *Server) handleSetDefaultModelConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireModels(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := s.models.SetDefault(r.Context(), id); err != nil {
		s.respondModelError(w, r, err)
		return
	}

	cfg, err := s.models.Get(r.Context(), id)
	if err != nil {
		s.respondModelError(w, r, err)
		return
	}
	if applyErr := s.applyModelConfig(cfg); applyErr != nil {
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, applyErr.Error())
		return
	}
	respondJSON(w, r, http.StatusOK, cfg)
}

// handleTestModelConfig 用指定配置发一次最小请求，验证连通性。
//
// 这是配置环节最容易出错的地方（密钥错、地址错、模型名错），
// 提供在线测试能显著降低排查成本。
func (s *Server) handleTestModelConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireModels(w, r) {
		return
	}
	id := r.PathValue("id")
	cfg, err := s.models.Get(r.Context(), id)
	if err != nil {
		s.respondModelError(w, r, err)
		return
	}
	// 每次测试都重建 Runner，确保用的是最新密钥。
	runner, buildErr := s.rebuildRunner(cfg)
	if buildErr != nil {
		respondJSON(w, r, http.StatusOK, map[string]any{
			"ok": false, "error": buildErr.Error(),
		})
		return
	}

	ctx, cancel := contextWithTimeout(r, 30)
	defer cancel()

	// 用最小历史触发一次生成，只看是否拿到首个增量或正常结束。
	history := []sessionMessage{
		newProbeMessage("ping"),
	}
	var gotOutput bool
	var runErr error
	for ev := range runner.Run(ctx, agent.Request{History: history}) {
		switch ev.Type {
		case agent.TypeDelta:
			if ev.Delta != "" {
				gotOutput = true
			}
		case agent.TypeDone:
			gotOutput = true
		case agent.TypeError:
			runErr = ev.Err
		}
		if gotOutput || runErr != nil {
			break
		}
	}

	if runErr != nil {
		respondJSON(w, r, http.StatusOK, map[string]any{"ok": false, "error": runErr.Error()})
		return
	}
	if !gotOutput {
		respondJSON(w, r, http.StatusOK, map[string]any{
			"ok": false, "error": "模型未返回任何内容",
		})
		return
	}
	respondJSON(w, r, http.StatusOK, map[string]any{
		"ok": true, "protocol": cfg.Protocol, "model": cfg.Model,
	})
}

// applyModelConfig 让配置立即生效（重建 Runner）。
//
// 返回的错误交由调用方决定如何响应（有的场景配置已保存成功，
// 只是生效失败，语义与"保存失败"不同）。
func (s *Server) applyModelConfig(cfg *modelconfig.Config) error {
	if s.rebuildRunner == nil {
		return nil
	}
	runner, err := s.rebuildRunner(cfg)
	if err != nil {
		slog.Error("重建 Runner 失败", "err", err, "config", cfg.Name)
		return fmt.Errorf("配置已保存，但无法生效: %w", err)
	}
	// 运行时切换：executor 通过 runnerFn 延迟读取，立即生效。
	s.runner = runner
	s.cfg.LLMProtocol = cfg.Protocol
	s.cfg.LLMProvider = cfg.Provider
	s.cfg.LLMModel = cfg.Model
	s.cfg.LLMBaseURL = resolveBaseURL(cfg)
	slog.Info("已切换模型配置",
		"name", cfg.Name, "protocol", cfg.Protocol, "model", cfg.Model)
	return nil
}

// resolveBaseURL 计算「实际会请求的」BaseURL，仅用于状态展示。
//
// 与 buildRunnerFromModelConfig 保持一致：填了地址就用填的，
// 留空则套用 provider 预设。版本段补全发生在 agent 层，这里不重复处理。
func resolveBaseURL(cfg *modelconfig.Config) string {
	if cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	if cfg.Provider != "" {
		if spec, err := config.NormalizeProvider(cfg.Provider); err == nil {
			return spec.BaseURL
		}
	}
	return ""
}

// respondModelError 把模型配置错误映射为 HTTP 状态。
func (s *Server) respondModelError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errorsIs(err, modelconfig.ErrNotFound):
		respondError(w, r, http.StatusNotFound, CodeNotFound, "模型配置不存在")
	case errorsIs(err, modelconfig.ErrConflict):
		respondError(w, r, http.StatusConflict, CodeInvalidArgument, err.Error())
	case errorsIs(err, modelconfig.ErrInvalid):
		respondError(w, r, http.StatusBadRequest, CodeInvalidArgument, err.Error())
	default:
		slog.ErrorContext(r.Context(), "模型配置操作失败", "err", err)
		respondError(w, r, http.StatusInternalServerError, CodeInternal, "模型配置操作失败")
	}
}

func toModelInput(req modelConfigInput) modelconfig.Input {
	return modelconfig.Input{
		Name:         strings.TrimSpace(req.Name),
		Protocol:     req.Protocol,
		Provider:     req.Provider,
		BaseURL:      req.BaseURL,
		Model:        req.Model,
		SystemPrompt: req.SystemPrompt,
		Temperature:  req.Temperature,
		MaxTokens:    req.MaxTokens,
		APIKey:       req.APIKey,
		IsDefault:    req.IsDefault,
	}
}
