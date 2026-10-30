package router

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"agent_rebuild/internal/tool"
)

// descriptionHead 是 search_router 描述的固定部分。目录（动态部分）接在后面。
//
// 措辞的目标是让模型在「内置工具做不到」时想起这个工具（docs/17 12.2）：
// 模型不知道某类能力存在，就不会去搜。
const descriptionHead = `发现并执行「自定义工具」（由外部 MCP 服务提供，是内置工具之外的能力）。
内置工具做不到的事（访问外部系统、数据库、公司内部服务等），先用它查有没有现成的工具，不要直接回答做不到。
用法：action=search 按关键词查找工具，结果里有参数说明 → action=execute 执行（tool 填全名「服务名/工具名」，arguments 填参数）。也可以 action=list 按服务浏览。
标注「可能需要用户确认」的工具，执行前可能会请用户批准，请提前告知用户。`

// description 生成完整描述：固定部分 + 按预算降级的目录。
func (r *Router) description(budget int) string {
	catalog := r.catalog(budget)
	if catalog == "" {
		return descriptionHead + "\n\n当前没有可用的自定义工具。"
	}
	return descriptionHead + "\n\n当前可用的自定义工具：\n" + catalog
}

// catalog 按预算逐级降级（docs/17 12.2）：
//  1. 每个工具一行「名字：描述首句」；
//  2. 每个 server 一句说明 + 前 8 个工具名；
//  3. 每个 server 一句说明 + 工具数量。
//
// 第 3 级即使超预算也照样返回：目录是模型知道这些工具存在的唯一途径，宁可长一点。
func (r *Router) catalog(budget int) string {
	for level := 1; level <= 3; level++ {
		c := r.catalogAt(level)
		if level == 3 || utf8.RuneCountInString(c) <= budget {
			return c
		}
	}
	return ""
}

func (r *Router) catalogAt(level int) string {
	var b strings.Builder
	for i := range r.servers {
		srv := &r.servers[i]
		if !srv.Available {
			fmt.Fprintf(&b, "- %s：暂不可用（%s）", srv.Name, orDefault(srv.Reason, "未连接"))
			if srv.Description != "" {
				fmt.Fprintf(&b, "。%s", srv.Description)
			}
			b.WriteString("\n")
			continue
		}
		n := r.enabledCount(srv)
		if n == 0 {
			continue // 全部被关掉：用户不想让模型用，目录里也不提
		}
		fmt.Fprintf(&b, "- %s（%d 个）：%s\n", srv.Name, n, r.serverSummary(srv))
		switch level {
		case 1:
			for _, ix := range r.entries {
				if ix.server == srv && ix.Enabled {
					fmt.Fprintf(&b, "  %s：%s\n", ix.Name, clip(firstSentence(ix.Description), 60))
				}
			}
		case 2:
			names := r.enabledNames(srv, 8)
			line := strings.Join(names, "、")
			if n > len(names) {
				line += " …"
			}
			fmt.Fprintf(&b, "  %s\n", line)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// serverSummary：配置/initialize 里的说明优先；都没有时用工具名自动生成。
func (r *Router) serverSummary(srv *Server) string {
	if d := strings.TrimSpace(srv.Description); d != "" {
		return clip(d, 80)
	}
	names := r.enabledNames(srv, 3)
	return fmt.Sprintf("提供 %s 等工具", strings.Join(names, "、"))
}

// ---- list ----

func (r *Router) list(server string, page int) tool.Result {
	var pool []*indexed
	if server = strings.TrimSpace(server); server != "" {
		srv := r.findServer(server)
		if srv == nil {
			return tool.Result{Output: fmt.Sprintf("没有名为 %q 的服务。可用服务：%s", server, r.serverNames()), IsError: true}
		}
		if !srv.Available {
			return tool.Result{Output: fmt.Sprintf("服务 %s 暂不可用：%s", srv.Name, orDefault(srv.Reason, "未连接")), IsError: true}
		}
		for _, ix := range r.entries {
			if ix.server == srv && ix.Enabled {
				pool = append(pool, ix)
			}
		}
	} else {
		for _, ix := range r.entries {
			if ix.Enabled && ix.server.Available {
				pool = append(pool, ix)
			}
		}
	}
	if len(pool) == 0 {
		return tool.Result{Output: "当前没有可用的自定义工具。\n" + r.unavailableNote()}
	}

	pages := (len(pool) + listPageSize - 1) / listPageSize
	if page < 1 {
		page = 1
	}
	if page > pages {
		page = pages
	}
	start := (page - 1) * listPageSize
	end := min(start+listPageSize, len(pool))

	var b strings.Builder
	fmt.Fprintf(&b, "共 %d 个自定义工具（第 %d/%d 页）：\n", len(pool), page, pages)
	var last *Server
	for _, ix := range pool[start:end] {
		if ix.server != last {
			last = ix.server
			fmt.Fprintf(&b, "\n[%s] %s\n", last.Name, r.serverSummary(last))
		}
		fmt.Fprintf(&b, "- %s（%s）：%s\n", ix.FullName(), riskLabel(ix.Risk), clip(firstSentence(ix.Description), 80))
	}
	if page < pages {
		fmt.Fprintf(&b, "\n还有更多，用 page=%d 查看下一页。", page+1)
	}
	if note := r.unavailableNote(); note != "" {
		b.WriteString("\n" + note)
	}
	b.WriteString("\n用 search 查看某个工具的参数说明后再 execute。")
	return tool.Result{Output: strings.TrimSpace(b.String())}
}

func (r *Router) serverNames() string {
	names := make([]string, 0, len(r.servers))
	for _, s := range r.servers {
		names = append(names, s.Name)
	}
	if len(names) == 0 {
		return "（无）"
	}
	return strings.Join(names, "、")
}

func (r *Router) unavailableNote() string {
	var parts []string
	for _, s := range r.servers {
		if !s.Available {
			parts = append(parts, fmt.Sprintf("%s（%s）", s.Name, orDefault(s.Reason, "未连接")))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "暂不可用的服务：" + strings.Join(parts, "、")
}

// ---- search ----

func (r *Router) search(query string) tool.Result {
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return tool.Result{Output: "search 需要 query：给出一个或多个关键词（空格分隔），例如 \"issue 创建\"", IsError: true}
	}
	type hit struct {
		ix    *indexed
		score float64
		all   bool
	}
	var hits []hit
	for _, ix := range r.entries {
		if !ix.Enabled || !ix.server.Available {
			continue
		}
		score, all := scoreEntry(ix, tokens)
		if score > 0 {
			hits = append(hits, hit{ix, score, all})
		}
	}
	if len(hits) == 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "没有找到与 %q 相关的自定义工具。", query)
		if cat := r.catalogAt(3); cat != "" {
			b.WriteString("\n现有服务：\n" + cat)
		}
		b.WriteString("\n可以换个关键词（中英文都可以），或用 list 浏览全部工具。")
		return tool.Result{Output: b.String()}
	}
	// 同分时保持目录顺序（stable），结果稳定可预期。
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > searchLimit {
		hits = hits[:searchLimit]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "找到 %d 个相关工具（按相关度排序）：\n", len(hits))
	for i, h := range hits {
		fmt.Fprintf(&b, "\n%d. %s（%s）", i+1, h.ix.FullName(), riskLabel(h.ix.Risk))
		if !h.all && len(tokens) > 1 {
			b.WriteString("［部分关键词匹配］")
		}
		b.WriteString("\n")
		if d := strings.TrimSpace(h.ix.Description); d != "" {
			fmt.Fprintf(&b, "   %s\n", clip(d, 400))
		}
		b.WriteString(indent(h.ix.sig.render(h.ix.FullName()), "   "))
	}
	b.WriteString("\n执行：action=execute，tool 填上面的全名，arguments 按签名填写。")
	return tool.Result{Output: strings.TrimSpace(b.String())}
}

// ---- execute ----

func (r *Router) execute(ctx context.Context, name string, args json.RawMessage) (tool.Result, error) {
	ix, msg := r.resolve(name)
	if ix == nil {
		return tool.Result{Output: msg, IsError: true}, nil
	}
	if !ix.Enabled {
		return tool.Result{Output: fmt.Sprintf("工具 %s 已被用户禁用，不能使用。请换用其他方式，或告诉用户需要在 MCP 面板里启用它。", ix.FullName()), IsError: true}, nil
	}
	if !ix.server.Available {
		return tool.Result{Output: fmt.Sprintf("工具 %s 所在的服务暂不可用：%s。请如实告诉用户。", ix.FullName(), orDefault(ix.server.Reason, "未连接")), IsError: true}, nil
	}
	if problems := ix.sig.validate(args); len(problems) > 0 {
		return tool.Result{
			Output: fmt.Sprintf("参数不符合 %s 的要求，未执行：\n- %s\n\n%s",
				ix.FullName(), strings.Join(problems, "\n- "), ix.sig.render(ix.FullName())),
			IsError: true,
		}, nil
	}
	if len(strings.TrimSpace(string(args))) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	if ix.Call == nil {
		return tool.Result{Output: fmt.Sprintf("工具 %s 没有可用的执行入口", ix.FullName()), IsError: true}, nil
	}
	return ix.Call(ctx, args)
}

// ---- 小工具 ----

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// firstSentence 取描述的第一句（换行、句号为止）。
func firstSentence(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n。"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// clip 按字符数截断（不切断多字节字符）。
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}
