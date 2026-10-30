package customtools

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"agent_rebuild/internal/tool"
	"agent_rebuild/internal/tool/mcp"
	"agent_rebuild/internal/tool/router"
)

// Source 是外部工具目录的来源（生产是 *mcp.Manager；测试可注入假实现）。
type Source interface {
	Catalog() []mcp.CatalogServer
}

// Options 配置 Service。
type Options struct {
	// Builtin 内置（meta）工具：永远原生暴露，Rebuild 时在它的基础上追加 search_router。
	Builtin *tool.Registry
	Source  Source
	// Store 覆盖设置的持久化；nil 表示不支持保存（memory 驱动），面板只读。
	Store Store
	// Apply 接收重建后的 Registry（main.go 里是 api.Server.ReplaceTools）。
	Apply  func(*tool.Registry)
	Router router.Options
	Logger *slog.Logger
}

// Service 把「外部工具目录 + 用户覆盖」组装成 search_router，并响应两类变化：
// 外部工具变化（mcp.Manager 的 OnChange）与用户改设置（SetOverride）。
type Service struct {
	opts Options
	log  *slog.Logger

	// rebuildMu 串行化 Rebuild：两类变化可能并发触发，串行之后最后一次重建
	// 一定基于最新的目录与覆盖，不会出现旧快照覆盖新快照。
	rebuildMu sync.Mutex

	mu        sync.RWMutex
	overrides map[string]Override // 内存缓存，与 Store 同步；Rebuild 不查库
}

// New 创建 Service，并从 Store 载入已有覆盖（失败只告警：工具照常可用，只是用户设置暂不生效）。
func New(opts Options) *Service {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Service{opts: opts, log: opts.Logger, overrides: map[string]Override{}}
	if opts.Store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if m, err := opts.Store.List(ctx); err != nil {
			s.log.Warn("读取自定义工具设置失败，暂按默认值处理", "err", err)
		} else {
			s.overrides = m
		}
	}
	return s
}

// OverridesSupported 报告是否能保存用户设置。
func (s *Service) OverridesSupported() bool { return s.opts.Store != nil }

// Rebuild 取最新目录、叠加覆盖、构建 search_router，交给 Apply。
//
// 没有配置任何外部 server 时不注册 search_router：一个永远「没有可用工具」的
// 代理工具只会白占 token、诱导模型去试。server 配了但暂时都不可用时**照样注册**，
// 目录里会写明不可用及原因，模型能如实告诉用户。
func (s *Service) Rebuild() {
	s.rebuildMu.Lock()
	defer s.rebuildMu.Unlock()

	catalog := s.opts.Source.Catalog()
	if len(catalog) == 0 {
		s.opts.Apply(s.opts.Builtin)
		return
	}
	s.mu.RLock()
	servers := make([]router.Server, 0, len(catalog))
	for _, cs := range catalog {
		rs := router.Server{
			Name:        cs.Name,
			Description: cs.Description,
			Available:   cs.Available,
			Reason:      cs.Reason,
		}
		for _, ct := range cs.Tools {
			risk, enabled := Effective(ct.BaseRisk, ct.BaseEnabled, s.overrides[ct.FullName()])
			rs.Entries = append(rs.Entries, router.Entry{
				Server:      ct.Server,
				Name:        ct.Name,
				Title:       ct.Title,
				Description: ct.Description,
				InputSchema: ct.InputSchema,
				Risk:        risk,
				Enabled:     enabled,
				Call:        ct.Call,
			})
		}
		servers = append(servers, rs)
	}
	s.mu.RUnlock()

	r := router.New(servers, s.opts.Router)
	s.opts.Apply(s.opts.Builtin.With(r))
	s.log.Debug("自定义工具已重建", "servers", len(servers), "has_enabled", r.HasEnabledTools())
}

// SetOverride 保存一条用户设置并立即重建（对之后**新开始**的对话轮次生效）。
func (s *Service) SetOverride(ctx context.Context, in Input) error {
	name, o, err := in.parse()
	if err != nil {
		return err
	}
	if s.opts.Store == nil {
		return ErrUnsupported
	}
	if err := s.opts.Store.Set(ctx, name, o); err != nil {
		return err
	}
	s.mu.Lock()
	if o.IsEmpty() {
		delete(s.overrides, name)
	} else {
		s.overrides[name] = o
	}
	s.mu.Unlock()
	s.Rebuild()
	return nil
}

// ToolView 是面板上的一行工具：基础值、用户覆盖、生效值。
type ToolView struct {
	Name         string `json:"name"` // "server/原始名"
	OriginalName string `json:"original_name"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description"`
	BaseRisk     string `json:"base_risk"`
	BaseEnabled  bool   `json:"base_enabled"`
	// OverrideRisk 为空串、OverrideEnabled 为 null 表示该字段跟随基础值。
	OverrideRisk    string `json:"override_risk"`
	OverrideEnabled *bool  `json:"override_enabled"`
	Risk            string `json:"risk"`
	Enabled         bool   `json:"enabled"`
}

// ToolViews 按 server 名返回当前目录里的工具及其设置（只含可用 server 的工具）。
func (s *Service) ToolViews() map[string][]ToolView {
	out := map[string][]ToolView{}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, cs := range s.opts.Source.Catalog() {
		views := make([]ToolView, 0, len(cs.Tools))
		for _, ct := range cs.Tools {
			o := s.overrides[ct.FullName()]
			risk, enabled := Effective(ct.BaseRisk, ct.BaseEnabled, o)
			v := ToolView{
				Name:         ct.FullName(),
				OriginalName: ct.Name,
				Title:        ct.Title,
				Description:  ct.Description,
				BaseRisk:     ct.BaseRisk.String(),
				BaseEnabled:  ct.BaseEnabled,
				Risk:         risk.String(),
				Enabled:      enabled,
			}
			if o.Risk != nil {
				v.OverrideRisk = o.Risk.String()
			}
			if o.Enabled != nil {
				e := *o.Enabled
				v.OverrideEnabled = &e
			}
			views = append(views, v)
		}
		out[cs.Name] = views
	}
	return out
}
