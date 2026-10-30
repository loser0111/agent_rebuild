package router

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"agent_rebuild/internal/tool"
)

// echoCall 返回一个记录调用参数的执行入口。
func echoCall(name string, calls *[]string) func(context.Context, json.RawMessage) (tool.Result, error) {
	return func(_ context.Context, args json.RawMessage) (tool.Result, error) {
		*calls = append(*calls, name+" "+string(args))
		return tool.Result{Output: "ran " + name}, nil
	}
}

func testRouter(t *testing.T, calls *[]string) *Router {
	t.Helper()
	issueSchema := json.RawMessage(`{"type":"object","properties":{
		"state":{"type":"string","enum":["open","closed"],"description":"状态"},
		"query":{"type":"string","description":"搜索语句"},
		"limit":{"type":"integer"}
	},"required":["query"]}`)
	createSchema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{
		"title":{"type":"string"},
		"labels":{"type":"array","items":{"type":"string"}},
		"assignee":{"type":"object","properties":{"login":{"type":"string"}}}
	},"required":["title"]}`)
	return New([]Server{
		{Name: "github", Description: "GitHub 仓库、Issue、PR 的查询与操作", Available: true, Entries: []Entry{
			{Server: "github", Name: "search_issues", Description: "按条件搜索 Issue。支持分页", InputSchema: issueSchema,
				Risk: tool.RiskSafe, Enabled: true, Call: echoCall("search_issues", calls)},
			{Server: "github", Name: "create_issue", Description: "创建一个新的 Issue", InputSchema: createSchema,
				Risk: tool.RiskModerate, Enabled: true, Call: echoCall("create_issue", calls)},
			{Server: "github", Name: "listPullRequests", Description: "列出仓库的 PR", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
				Risk: tool.RiskSafe, Enabled: true, Call: echoCall("listPullRequests", calls)},
			{Server: "github", Name: "delete_repo", Description: "删除仓库", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
				Risk: tool.RiskDangerous, Enabled: false, Call: echoCall("delete_repo", calls)},
		}},
		{Name: "jira", Description: "公司 Jira：创建 Jira 工单、查询工单", Available: true, Entries: []Entry{
			{Server: "jira", Name: "create_ticket", Description: "在 Jira 里创建工单", InputSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
				Risk: tool.RiskDangerous, Enabled: true, Call: echoCall("create_ticket", calls)},
		}},
		{Name: "postgres", Available: false, Reason: "启动超时"},
	}, Options{})
}

func run(t *testing.T, r *Router, args string) tool.Result {
	t.Helper()
	res, err := r.Execute(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("Execute(%s): %v", args, err)
	}
	return res
}

func TestDescriptionCatalog(t *testing.T) {
	r := testRouter(t, &[]string{})
	d := r.Spec().Description
	// 禁用的 delete_repo 不计数；第 1 级目录每个工具一行，描述只取首句。
	for _, want := range []string{
		"github（3 个）：GitHub 仓库",
		"search_issues：按条件搜索 Issue",
		"jira（1 个）",
		"postgres：暂不可用（启动超时）",
	} {
		if !strings.Contains(d, want) {
			t.Fatalf("描述缺少 %q：\n%s", want, d)
		}
	}
	if strings.Contains(d, "delete_repo") || strings.Contains(d, "支持分页") {
		t.Fatalf("描述不应包含禁用工具或描述的第二句：\n%s", d)
	}
	if r.Spec().Name != Name || r.Spec().Risk != tool.RiskSafe {
		t.Fatalf("spec = %+v", r.Spec())
	}
}

func TestDescriptionDegradesWithBudget(t *testing.T) {
	var entries []Entry
	for i := 0; i < 40; i++ {
		entries = append(entries, Entry{Server: "big", Name: fmt.Sprintf("tool_%02d", i),
			Description: strings.Repeat("很长的描述", 10), Enabled: true, Risk: tool.RiskSafe})
	}
	servers := []Server{{Name: "big", Description: "大服务", Available: true, Entries: entries}}

	lvl2 := New(servers, Options{CatalogBudget: 400}).Spec().Description
	if !strings.Contains(lvl2, "tool_00、tool_01") || !strings.Contains(lvl2, " …") || strings.Contains(lvl2, "tool_10") {
		t.Fatalf("预算不足时应降到第 2 级（前 8 个名字 + …）：\n%s", lvl2)
	}
	lvl3 := New(servers, Options{CatalogBudget: 10}).Spec().Description
	if !strings.Contains(lvl3, "big（40 个）：大服务") || strings.Contains(lvl3, "tool_00") {
		t.Fatalf("预算极小时应降到第 3 级（只有说明和数量）：\n%s", lvl3)
	}
}

func TestSearchMultiKeywordFuzzyAndChinese(t *testing.T) {
	r := testRouter(t, &[]string{})
	// 依次覆盖：多关键词、拼写容错、驼峰连写、驼峰分段、中文二字组、逗号分隔且多词全中加分。
	cases := []struct{ query, wantFirst string }{
		{"issue search", "github/search_issues"},
		{"isue", "github/search_issues"},
		{"pullrequest", "github/listPullRequests"},
		{"pull request", "github/listPullRequests"},
		{"创建工单", "jira/create_ticket"},
		{"create, issue", "github/create_issue"},
	}
	for _, c := range cases {
		res := run(t, r, fmt.Sprintf(`{"action":"search","query":%q}`, c.query))
		first := strings.Index(res.Output, "1. ")
		if first < 0 || !strings.HasPrefix(res.Output[first+3:], c.wantFirst) {
			t.Fatalf("search %q 第一个结果应为 %s：\n%s", c.query, c.wantFirst, res.Output)
		}
	}
	// 结果里带签名与风险说明，禁用的工具不出现。
	res := run(t, r, `{"action":"search","query":"issue repo"}`)
	if !strings.Contains(res.Output, `签名：github/search_issues(query: string, state?: "open"|"closed", limit?: integer)`) {
		t.Fatalf("缺少签名（必填参数在前、enum 展开）：\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "只读") || strings.Contains(res.Output, "delete_repo") {
		t.Fatalf("风险说明 / 禁用过滤不对：\n%s", res.Output)
	}
}

func TestSearchNoResultAndEmptyQuery(t *testing.T) {
	r := testRouter(t, &[]string{})
	res := run(t, r, `{"action":"search","query":"天气预报"}`)
	if res.IsError || !strings.Contains(res.Output, "没有找到") || !strings.Contains(res.Output, "github（3 个）") {
		t.Fatalf("无结果时应给出服务概况：%+v", res)
	}
	if res := run(t, r, `{"action":"search"}`); !res.IsError {
		t.Fatal("空 query 应报错")
	}
}

func TestListGroupsAndFilters(t *testing.T) {
	r := testRouter(t, &[]string{})
	res := run(t, r, `{"action":"list"}`)
	for _, want := range []string{"共 4 个自定义工具", "[github]", "[jira]", "github/create_issue（写操作，可能需要用户确认）", "暂不可用的服务：postgres（启动超时）"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("list 缺少 %q：\n%s", want, res.Output)
		}
	}
	res = run(t, r, `{"action":"list","server":"jira"}`)
	if !strings.Contains(res.Output, "共 1 个") || strings.Contains(res.Output, "github/") {
		t.Fatalf("按 server 过滤失败：\n%s", res.Output)
	}
	if res := run(t, r, `{"action":"list","server":"postgres"}`); !res.IsError || !strings.Contains(res.Output, "启动超时") {
		t.Fatalf("不可用的 server 应说明原因：%+v", res)
	}
	if res := run(t, r, `{"action":"list","server":"nope"}`); !res.IsError {
		t.Fatal("未知 server 应报错")
	}
}

func TestListPaginates(t *testing.T) {
	var entries []Entry
	for i := 0; i < 120; i++ {
		entries = append(entries, Entry{Server: "big", Name: fmt.Sprintf("t%03d", i), Enabled: true, Risk: tool.RiskSafe})
	}
	r := New([]Server{{Name: "big", Available: true, Entries: entries}}, Options{})
	p1 := run(t, r, `{"action":"list"}`).Output
	if !strings.Contains(p1, "第 1/3 页") || !strings.Contains(p1, "big/t049") || strings.Contains(p1, "big/t050") || !strings.Contains(p1, "page=2") {
		t.Fatalf("第 1 页不对：\n%s", p1)
	}
	p3 := run(t, r, `{"action":"list","page":9}`).Output
	if !strings.Contains(p3, "第 3/3 页") || !strings.Contains(p3, "big/t119") {
		t.Fatalf("超出页码应落到最后一页：\n%s", p3)
	}
}

func TestExecuteValidatesAndForwards(t *testing.T) {
	var calls []string
	r := testRouter(t, &calls)

	// 正常执行；arguments 写成 JSON 字符串也兼容。
	res := run(t, r, `{"action":"execute","tool":"github/search_issues","arguments":"{\"query\":\"bug\",\"limit\":5}"}`)
	if res.IsError || res.Output != "ran search_issues" || len(calls) != 1 || calls[0] != `search_issues {"query":"bug","limit":5}` {
		t.Fatalf("res=%+v calls=%v", res, calls)
	}
	// 只给原始名且全局唯一：直接执行。
	if res := run(t, r, `{"action":"execute","tool":"create_ticket","arguments":{"summary":"x"}}`); res.IsError {
		t.Fatalf("原始名唯一时应执行：%+v", res)
	}

	calls = calls[:0]
	bad := []struct{ args, want string }{
		{`{"action":"execute","tool":"github/search_issues","arguments":{}}`, "缺少必填参数 query"},
		{`{"action":"execute","tool":"github/search_issues","arguments":{"query":1}}`, "参数 query 应为 string，实际是 number"},
		{`{"action":"execute","tool":"github/search_issues","arguments":{"query":"a","limit":1.5}}`, "参数 limit 应为 integer"},
		{`{"action":"execute","tool":"github/search_issues","arguments":{"query":"a","state":"all"}}`, `取值必须是 "open"|"closed" 之一`},
		{`{"action":"execute","tool":"github/create_issue","arguments":{"title":"a","foo":1}}`, "未知参数 foo"},
		{`{"action":"execute","tool":"github/delete_repo","arguments":{}}`, "已被用户禁用"},
		{`{"action":"execute","tool":"github/serch_issues"}`, "是不是要：github/search_issues"},
		{`{"action":"execute","tool":"github/nope"}`, "服务 github 下的工具：search_issues、create_issue、listPullRequests"},
	}
	for _, c := range bad {
		res := run(t, r, c.args)
		if !res.IsError || !strings.Contains(res.Output, c.want) {
			t.Fatalf("%s：期望错误包含 %q，实际：\n%s", c.args, c.want, res.Output)
		}
	}
	// 参数报错要附签名，模型才能一次改对。
	if res := run(t, r, bad[0].args); !strings.Contains(res.Output, "签名：github/search_issues(") {
		t.Fatalf("参数报错应附签名：\n%s", res.Output)
	}
	if len(calls) != 0 {
		t.Fatalf("校验失败 / 禁用 / 不存在时不应转发：%v", calls)
	}
	// 1.0 是合法的 integer。
	if res := run(t, r, `{"action":"execute","tool":"github/search_issues","arguments":{"query":"a","limit":1.0}}`); res.IsError {
		t.Fatalf("1.0 应视为 integer：%s", res.Output)
	}
}

func TestExecuteAmbiguousShortName(t *testing.T) {
	r := New([]Server{
		{Name: "a", Available: true, Entries: []Entry{{Server: "a", Name: "ping", Enabled: true}}},
		{Name: "b", Available: true, Entries: []Entry{{Server: "b", Name: "ping", Enabled: true}}},
	}, Options{})
	res := run(t, r, `{"action":"execute","tool":"ping"}`)
	if !res.IsError || !strings.Contains(res.Output, "a/ping、b/ping") {
		t.Fatalf("重名短名应要求用全名：%s", res.Output)
	}
}

// 按被代理的目标定级（docs/17 12.5）。
func TestResolveCall(t *testing.T) {
	r := testRouter(t, &[]string{})
	cases := []struct {
		args       string
		wantTarget string
		wantRisk   tool.Risk
	}{
		{`{"action":"list"}`, "", tool.RiskSafe},
		{`{"action":"search","query":"x"}`, "", tool.RiskSafe},
		{`{"action":"execute","tool":"jira/create_ticket"}`, "jira/create_ticket", tool.RiskDangerous},
		// 短名也按目标定级。
		{`{"action":"execute","tool":"create_issue"}`, "github/create_issue", tool.RiskModerate},
		// 禁用 / 不存在：不会执行，不需要审批。
		{`{"action":"execute","tool":"github/delete_repo"}`, "github/delete_repo", tool.RiskSafe},
		{`{"action":"execute","tool":"nope/x"}`, "nope/x", tool.RiskSafe},
		{`not json`, "", tool.RiskSafe},
	}
	for _, c := range cases {
		target, risk := r.ResolveCall(json.RawMessage(c.args))
		if target != c.wantTarget || risk != c.wantRisk {
			t.Fatalf("ResolveCall(%s) = %q/%v，期望 %q/%v", c.args, target, risk, c.wantTarget, c.wantRisk)
		}
	}
}

func TestSignatureComplexParams(t *testing.T) {
	sig := parseSignature(json.RawMessage(`{"type":"object","properties":{
		"tags":{"type":"array","items":{"type":"string"}},
		"owner":{"type":"object","properties":{"id":{"type":"integer"}}},
		"maybe":{"type":["string","null"]}
	}}`))
	out := sig.render("s/t")
	if !strings.Contains(out, "签名：s/t(tags?: string[], owner?: object, maybe?: string|null)") {
		t.Fatalf("签名不对：\n%s", out)
	}
	if !strings.Contains(out, `结构：{"type":"object","properties":{"id":{"type":"integer"}}}`) {
		t.Fatalf("嵌套对象应附原始结构：\n%s", out)
	}
	if p := sig.validate(json.RawMessage(`{"maybe":null}`)); len(p) != 0 {
		t.Fatalf("类型数组里的 null 应通过：%v", p)
	}
	// 无法解析的 schema：放弃预校验，交给 server。
	if p := parseSignature(json.RawMessage(`{"properties":[]}`)).validate(json.RawMessage(`{"x":1}`)); len(p) != 0 {
		t.Fatalf("schema 无法解析时不应拦截：%v", p)
	}
}
