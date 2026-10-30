// Package router 实现代理工具 search_router（见 docs/17-mcp-client.md 第 12 节）。
//
// 工具分三类：meta 工具（内置，原生暴露）、代理工具（本包，原生暴露）、
// 自定义工具（MCP 等外部来源，**不原生暴露**，只能经本工具 list / search / execute）。
// 这样发给模型的 tools 列表固定，不随外部工具数量膨胀。
//
// 本包只依赖 internal/tool，不认识 MCP：外部工具由调用方（customtools）翻译成 Entry 传进来。
// Router 是不可变快照：工具集变化时由调用方整体新建一个 Router，再原子替换进 Registry。
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"agent_rebuild/internal/tool"
)

// Name 是代理工具暴露给模型的名字。
const Name = "search_router"

const (
	// DefaultCatalogBudget 是工具描述里目录部分的字符预算（docs/17 12.2）。
	DefaultCatalogBudget = 3000
	listPageSize         = 50
	searchLimit          = 8
)

// Entry 是一个自定义工具（已叠加前端覆盖后的**生效值**）。
type Entry struct {
	Server      string
	Name        string // server 内的原始工具名
	Title       string
	Description string
	InputSchema json.RawMessage
	Risk        tool.Risk
	// Enabled=false 的工具不进目录、list、search；execute 时明确告诉模型「已被用户禁用」。
	Enabled bool
	Call    func(ctx context.Context, args json.RawMessage) (tool.Result, error)
}

// FullName 返回 "server/原始名"。
func (e Entry) FullName() string { return e.Server + "/" + e.Name }

// Server 是一个外部工具来源的目录信息。
type Server struct {
	Name        string
	Description string
	Available   bool
	// Reason 不可用的原因（Available=false 时给模型看）。
	Reason  string
	Entries []Entry
}

// Options 可调参数。
type Options struct {
	// CatalogBudget 目录字符预算，<=0 用 DefaultCatalogBudget。
	CatalogBudget int
}

// Router 是 search_router 工具：实现 tool.Tool 与 tool.CallResolver。
type Router struct {
	servers []Server
	entries []*indexed          // 全部工具（含禁用的），顺序 = server 顺序 + server 内顺序
	byFull  map[string]*indexed // "server/name" → 工具
	byShort map[string][]*indexed
	spec    tool.Spec
}

// indexed 是带搜索索引的 Entry。
type indexed struct {
	Entry
	server     *Server
	sig        signature
	nameLower  string
	nameJoined string   // 去掉分隔符的小写名字，匹配 "pullrequest" 这类连写
	segments   []string // 名字按 _ - / . 和驼峰切开
	descLower  string
	paramNames []string
	serverDesc string
}

var _ tool.Tool = (*Router)(nil)
var _ tool.CallResolver = (*Router)(nil)

// New 用一份目录快照构建 Router。servers 的顺序决定目录与 list 的顺序。
func New(servers []Server, opts Options) *Router {
	if opts.CatalogBudget <= 0 {
		opts.CatalogBudget = DefaultCatalogBudget
	}
	r := &Router{
		servers: make([]Server, len(servers)),
		byFull:  map[string]*indexed{},
		byShort: map[string][]*indexed{},
	}
	copy(r.servers, servers)
	for i := range r.servers {
		srv := &r.servers[i]
		for _, e := range srv.Entries {
			ix := newIndexed(e, srv)
			if _, dup := r.byFull[e.FullName()]; dup {
				continue
			}
			r.entries = append(r.entries, ix)
			r.byFull[e.FullName()] = ix
			r.byShort[e.Name] = append(r.byShort[e.Name], ix)
		}
	}
	r.spec = tool.Spec{
		Name:        Name,
		Description: r.description(opts.CatalogBudget),
		Parameters:  json.RawMessage(parametersSchema),
		// 自身风险没有意义：真正的定级在 ResolveCall（按被代理的目标）。
		Risk: tool.RiskSafe,
	}
	return r
}

func newIndexed(e Entry, srv *Server) *indexed {
	ix := &indexed{
		Entry:      e,
		server:     srv,
		sig:        parseSignature(e.InputSchema),
		nameLower:  strings.ToLower(e.Name),
		descLower:  strings.ToLower(e.Title + " " + e.Description),
		serverDesc: strings.ToLower(srv.Name + " " + srv.Description),
	}
	ix.segments = splitSegments(e.Name)
	ix.nameJoined = strings.Join(ix.segments, "")
	for _, p := range ix.sig.params {
		ix.paramNames = append(ix.paramNames, strings.ToLower(p.name))
	}
	return ix
}

// Spec 实现 tool.Tool。
func (r *Router) Spec() tool.Spec { return r.spec }

// HasEnabledTools 报告是否至少有一个可用的自定义工具（供调用方决定要不要注册本工具）。
func (r *Router) HasEnabledTools() bool {
	for _, ix := range r.entries {
		if ix.Enabled && ix.server.Available {
			return true
		}
	}
	return false
}

const parametersSchema = `{
  "type": "object",
  "properties": {
    "action": {"type": "string", "enum": ["list", "search", "execute"],
      "description": "list：按服务浏览工具；search：按关键词查找工具及其参数说明；execute：执行一个自定义工具"},
    "query": {"type": "string", "description": "search 用：关键词，空格分隔，可多个；支持中英文与拼写容错"},
    "server": {"type": "string", "description": "list 用（可选）：只看某个服务"},
    "page": {"type": "integer", "description": "list 用（可选）：页码，从 1 开始"},
    "tool": {"type": "string", "description": "execute 用：工具全名，格式 服务名/工具名"},
    "arguments": {"type": "object", "description": "execute 用：传给该工具的参数，按 search 返回的参数说明填写"}
  },
  "required": ["action"]
}`

type routerArgs struct {
	Action    string          `json:"action"`
	Query     string          `json:"query"`
	Server    string          `json:"server"`
	Page      int             `json:"page"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

func decodeArgs(raw json.RawMessage) (routerArgs, error) {
	var a routerArgs
	if len(raw) == 0 {
		return a, fmt.Errorf("缺少参数：至少需要 action（list / search / execute）")
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("参数格式错误：%v", err)
	}
	a.Action = strings.ToLower(strings.TrimSpace(a.Action))
	a.Tool = strings.TrimSpace(a.Tool)
	a.Arguments = unwrapStringArgs(a.Arguments)
	return a, nil
}

// unwrapStringArgs 兼容模型把 arguments 写成 JSON 字符串（"{\"q\":1}"）的常见错误。
func unwrapStringArgs(raw json.RawMessage) json.RawMessage {
	t := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(t, `"`) {
		return raw
	}
	var inner string
	if json.Unmarshal(raw, &inner) != nil {
		return raw
	}
	inner = strings.TrimSpace(inner)
	if strings.HasPrefix(inner, "{") && json.Valid([]byte(inner)) {
		return json.RawMessage(inner)
	}
	return raw
}

// Execute 实现 tool.Tool。
func (r *Router) Execute(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	a, err := decodeArgs(raw)
	if err != nil {
		return tool.Result{Output: err.Error(), IsError: true}, nil
	}
	switch a.Action {
	case "list":
		return r.list(a.Server, a.Page), nil
	case "search":
		return r.search(a.Query), nil
	case "execute":
		return r.execute(ctx, a.Tool, a.Arguments)
	default:
		return tool.Result{
			Output:  fmt.Sprintf("未知 action %q，可选：list / search / execute", a.Action),
			IsError: true,
		}, nil
	}
}

// ResolveCall 实现 tool.CallResolver：list / search 只读；execute 按目标工具的生效风险定级。
//
// 目标不存在、已禁用、所在服务不可用 → safe：这些情况 execute 只会返回错误、不会执行任何东西，
// 没必要让用户为一次注定失败的调用点批准。必须和 execute 用同一套解析逻辑（resolve），
// 否则会出现「定级时认为 A、执行时执行了 B」。
func (r *Router) ResolveCall(raw json.RawMessage) (string, tool.Risk) {
	a, err := decodeArgs(raw)
	if err != nil || a.Action != "execute" {
		return "", tool.RiskSafe
	}
	ix, _ := r.resolve(a.Tool)
	if ix == nil {
		return a.Tool, tool.RiskSafe
	}
	if !ix.Enabled || !ix.server.Available {
		return ix.FullName(), tool.RiskSafe
	}
	return ix.FullName(), ix.Risk
}

// resolve 按全名查工具；只给了原始名且全局唯一时也接受。失败时返回给模型看的说明。
func (r *Router) resolve(name string) (*indexed, string) {
	if name == "" {
		return nil, "缺少 tool：请给出工具全名（服务名/工具名），可先用 search 查找"
	}
	if ix, ok := r.byFull[name]; ok {
		return ix, ""
	}
	if !strings.Contains(name, "/") {
		switch cands := r.byShort[name]; len(cands) {
		case 1:
			return cands[0], ""
		case 0:
		default:
			names := make([]string, 0, len(cands))
			for _, c := range cands {
				names = append(names, c.FullName())
			}
			return nil, fmt.Sprintf("工具名 %q 在多个服务中都存在，请用全名：%s", name, strings.Join(names, "、"))
		}
	}
	return nil, r.notFound(name)
}

// notFound 生成「工具不存在」的说明：最相近的候选 + 同服务的工具列表。
func (r *Router) notFound(name string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "工具 %q 不存在。", name)

	type cand struct {
		full string
		dist int
	}
	var cands []cand
	lower := strings.ToLower(name)
	for _, ix := range r.entries {
		if !ix.Enabled || !ix.server.Available {
			continue
		}
		d := levenshtein(lower, strings.ToLower(ix.FullName()), 1<<30)
		if d2 := levenshtein(lower, ix.nameLower, 1<<30); d2 < d {
			d = d2
		}
		cands = append(cands, cand{ix.FullName(), d})
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })
	if len(cands) > 3 {
		cands = cands[:3]
	}
	if len(cands) > 0 {
		names := make([]string, 0, len(cands))
		for _, c := range cands {
			names = append(names, c.full)
		}
		fmt.Fprintf(&b, "是不是要：%s？", strings.Join(names, "、"))
	}
	if server, _, ok := strings.Cut(name, "/"); ok {
		if srv := r.findServer(server); srv != nil {
			fmt.Fprintf(&b, "\n服务 %s 下的工具：%s", srv.Name, strings.Join(r.enabledNames(srv, 0), "、"))
		}
	}
	b.WriteString("\n可以用 search 按关键词查找，或 list 浏览全部工具。")
	return b.String()
}

func (r *Router) findServer(name string) *Server {
	for i := range r.servers {
		if strings.EqualFold(r.servers[i].Name, name) {
			return &r.servers[i]
		}
	}
	return nil
}

// enabledNames 返回某个 server 下可用工具的原始名（limit<=0 表示不限）。
func (r *Router) enabledNames(srv *Server, limit int) []string {
	var out []string
	for _, ix := range r.entries {
		if ix.server != srv || !ix.Enabled {
			continue
		}
		out = append(out, ix.Name)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (r *Router) enabledCount(srv *Server) int {
	n := 0
	for _, ix := range r.entries {
		if ix.server == srv && ix.Enabled {
			n++
		}
	}
	return n
}

// riskLabel 是给模型看的风险说明：让它提前告诉用户「这一步需要你确认」。
func riskLabel(r tool.Risk) string {
	switch r {
	case tool.RiskSafe:
		return "只读"
	case tool.RiskModerate:
		return "写操作，可能需要用户确认"
	default:
		return "高危，可能需要用户确认"
	}
}
