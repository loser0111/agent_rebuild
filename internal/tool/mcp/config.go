// Package mcp 实现 Model Context Protocol 的客户端侧：连接外部 MCP server，
// 把它们暴露的工具包装成 tool.Tool，接入现有的 Registry / ReAct loop / 审批体系。
//
// 本轮只实现 stdio transport 与 tools 能力子集（见 docs/17-mcp-client.md）。
// MCP 工具不原生暴露给模型：本包产出目录快照（Manager.Catalog），由 customtools
// 叠加用户设置后组装成代理工具 search_router（internal/tool/router）。
// 包内分层：
//
//   - config.go   读取 mcp.json（mcpServers 格式）、展开 ${VAR}、校验
//   - client.go   JSON-RPC 2.0 会话：请求/响应配对、通知、server 反向请求
//   - stdio.go    子进程生命周期：进程组、stderr 进日志、优雅关闭
//   - naming.go   server 名校验、"server/tool" 全名
//   - adapter.go  远端工具 → CatalogTool（基础风险/开关、执行入口）；结果渲染
//   - manager.go  多 server 管理：连接、断线退避重连、目录快照与变化通知
//
// 协议层被 Client 类型封装：将来换官方 go-sdk 只需替换 client.go / stdio.go，
// Manager / Adapter / API 层不受影响。
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"agent_rebuild/internal/tool"
)

// 默认值。
const (
	// DefaultStartupTimeout 是 initialize + 首次 tools/list 的超时。
	//
	// 为什么是 30s 而不是 docs/13 写的 10-15s：社区 server 绝大多数用
	// `npx -y <pkg>` 启动，首次运行要现场下载 npm 包，10s 在国内网络下经常不够；
	// 超时设短了用户看到的是「启动失败」，而真实原因只是「慢」。
	DefaultStartupTimeout = 30 * time.Second
)

// ServerConfig 是一个 MCP server 的配置（已展开变量、已校验）。
type ServerConfig struct {
	// Name 配置里的键名，也是自定义工具全名 "server/tool" 的前半部分。
	Name string
	// Description 用户写的一句话说明，进 search_router 的工具目录（docs/17 12.2）。
	Description string
	// Source 来自哪个配置文件（用于状态面板与排查「到底读的是哪份」）。
	Source string

	// Transport 目前只有 "stdio" 可用；"http" 能被解析但会被标为不支持。
	Transport string
	Command   string
	Args      []string
	// Env 是**最终**传给子进程的完整环境（白名单 + 配置里的 env），
	// 形如 "K=V"。在 Load 阶段算好，Manager 不再碰 os.Environ。
	Env []string
	// Cwd 子进程工作目录；空表示由 Manager 用 workspace 根目录填充。
	Cwd string
	URL string

	Disabled bool
	// EnabledTools / DisabledTools 按远端原始工具名决定**基础开关**
	// （前端设置可以再覆盖，见 docs/17 第 5 节）。
	EnabledTools  []string
	DisabledTools []string
	// TrustAnnotations 是否信任 server 自报的 readOnlyHint / destructiveHint。
	TrustAnnotations bool
	// ToolRisk 按原始工具名覆盖风险；键 "*" 表示该 server 的默认风险。
	ToolRisk map[string]tool.Risk

	StartupTimeout time.Duration

	// Err 非空表示这条配置本身无效（缺 command、变量未设置、不支持的 transport……）。
	// 保留在列表里而不是直接丢弃：状态面板需要告诉用户「它为什么没起来」。
	Err error
}

// fileFormat 是磁盘上的配置格式，与 Claude Desktop / Claude Code / Cursor 的
// mcpServers 对齐，额外字段（trust_annotations 等）是本项目扩展。
//
// 不开 DisallowUnknownFields：别的客户端的配置里常带私有字段，严格拒绝会让
// 「直接把现成配置拷过来」失效。代价是拼错字段名不报错——但可能拼错的都是
// 「放宽类」字段，拼错的结果是**更严格**，方向安全。
type fileFormat struct {
	MCPServers map[string]rawServer `json:"mcpServers"`
}

type rawServer struct {
	Type              string            `json:"type"`
	Description       string            `json:"description"`
	Command           string            `json:"command"`
	Args              []string          `json:"args"`
	Env               map[string]string `json:"env"`
	Cwd               string            `json:"cwd"`
	URL               string            `json:"url"`
	Headers           map[string]string `json:"headers"`
	Disabled          bool              `json:"disabled"`
	EnabledTools      []string          `json:"enabled_tools"`
	DisabledTools     []string          `json:"disabled_tools"`
	TrustAnnotations  bool              `json:"trust_annotations"`
	ToolRisk          map[string]string `json:"tool_risk"`
	StartupTimeoutSec float64           `json:"startup_timeout_sec"`
}

// DefaultConfigPaths 计算要读取的配置文件（不检查是否存在，见 docs/17 3.2）。
//
//   - explicit 非空（MCP_CONFIG）：**只**读这些，便于测试与多环境切换；
//   - 否则读用户级 <dataDir>/mcp.json；
//   - loadProject 为真时再追加项目级 <workspace>/.mcp.json（后读覆盖先读）。
//
// 项目级默认不读：.mcp.json 里的 command 会被直接执行，把一个陌生仓库设为
// workspace 就等于运行了仓库作者指定的任意命令。
func DefaultConfigPaths(explicit []string, dataDir, workspace string, loadProject bool) []string {
	if len(explicit) > 0 {
		out := make([]string, 0, len(explicit))
		for _, p := range explicit {
			if abs, err := filepath.Abs(p); err == nil {
				p = abs
			}
			out = append(out, p)
		}
		return out
	}
	var out []string
	if dataDir != "" {
		out = append(out, filepath.Join(dataDir, "mcp.json"))
	}
	if loadProject && workspace != "" {
		out = append(out, filepath.Join(workspace, ".mcp.json"))
	}
	return out
}

// LoadFiles 按顺序读取配置文件，同名 server 后者覆盖前者。
//
// lookupEnv 用于 ${VAR} 展开与环境白名单取值（生产传 os.LookupEnv，测试可注入）。
//
// 返回：
//   - servers：按名字排序（工具顺序稳定，模型看到的工具列表不会每次重启都变）；
//     其中配置无效的条目 Err 非空，仍然返回，供状态面板展示原因；
//   - warnings：文件级问题（读失败、JSON 非法）。文件不存在不算问题——
//     绝大多数用户根本没有 mcp.json，为此每次启动告警只会制造噪音。
func LoadFiles(paths []string, lookupEnv func(string) (string, bool)) ([]ServerConfig, []error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	merged := map[string]ServerConfig{}
	var warnings []error

	for _, path := range paths {
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			warnings = append(warnings, fmt.Errorf("读取 MCP 配置 %s 失败: %w", path, err))
			continue
		}
		var f fileFormat
		if err := json.Unmarshal(data, &f); err != nil {
			warnings = append(warnings, fmt.Errorf("MCP 配置 %s 不是合法 JSON: %w", path, err))
			continue
		}
		for name, raw := range f.MCPServers {
			if prev, ok := merged[name]; ok {
				slog.Info("MCP server 配置被后读的文件覆盖",
					"server", name, "previous", prev.Source, "current", path)
			}
			merged[name] = parseServer(name, path, raw, lookupEnv)
		}
	}

	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ServerConfig, 0, len(names))
	for _, name := range names {
		out = append(out, merged[name])
	}
	return out, warnings
}

// parseServer 把一条原始配置转换为 ServerConfig；任何问题都写进 Err 而不是返回 error，
// 保证「一条配置坏了」不影响同文件里的其他 server。
func parseServer(name, source string, raw rawServer, lookupEnv func(string) (string, bool)) ServerConfig {
	sc := ServerConfig{
		Name:             name,
		Source:           source,
		Description:      strings.TrimSpace(raw.Description),
		Disabled:         raw.Disabled,
		EnabledTools:     raw.EnabledTools,
		DisabledTools:    raw.DisabledTools,
		TrustAnnotations: raw.TrustAnnotations,
		StartupTimeout:   DefaultStartupTimeout,
	}
	if raw.StartupTimeoutSec > 0 {
		sc.StartupTimeout = time.Duration(raw.StartupTimeoutSec * float64(time.Second))
	}

	fail := func(err error) ServerConfig {
		sc.Err = err
		return sc
	}

	if err := ValidateServerName(name); err != nil {
		return fail(err)
	}

	// transport：显式 type 优先；没写 type 时，只有 url 没有 command 视为 HTTP。
	switch strings.ToLower(strings.TrimSpace(raw.Type)) {
	case "", "stdio":
		if raw.Type == "" && raw.Command == "" && raw.URL != "" {
			sc.Transport = "http"
		} else {
			sc.Transport = "stdio"
		}
	case "http", "streamable-http", "streamablehttp", "sse":
		sc.Transport = "http"
	default:
		return fail(fmt.Errorf("未知的 transport 类型 %q", raw.Type))
	}

	// 风险覆盖：安全相关字段，写错值必须报错而不是静默退回默认。
	if len(raw.ToolRisk) > 0 {
		sc.ToolRisk = make(map[string]tool.Risk, len(raw.ToolRisk))
		for toolName, v := range raw.ToolRisk {
			r, err := parseRisk(v)
			if err != nil {
				return fail(fmt.Errorf("tool_risk[%q]: %w", toolName, err))
			}
			sc.ToolRisk[toolName] = r
		}
	}

	exp := &expander{lookup: lookupEnv}
	sc.Command = exp.expand(raw.Command)
	sc.Args = make([]string, len(raw.Args))
	for i, a := range raw.Args {
		sc.Args[i] = exp.expand(a)
	}
	sc.Cwd = exp.expand(raw.Cwd)
	sc.URL = exp.expand(raw.URL)
	extraEnv := make(map[string]string, len(raw.Env))
	for k, v := range raw.Env {
		extraEnv[k] = exp.expand(v)
	}
	for _, v := range raw.Headers {
		exp.expand(v) // HTTP 暂不支持，但变量缺失仍然提前暴露
	}
	if len(exp.missing) > 0 {
		return fail(fmt.Errorf("引用了未设置的环境变量：%s（可写成 ${VAR:-默认值}）",
			strings.Join(exp.missing, ", ")))
	}

	if sc.Transport == "http" {
		// 配置能解析，但本轮不支持：明确标出来，而不是静默忽略让用户以为它在工作。
		return fail(fmt.Errorf("暂不支持 HTTP transport（%s），本版本只支持 stdio", sc.URL))
	}
	if strings.TrimSpace(sc.Command) == "" {
		return fail(fmt.Errorf("stdio server 缺少 command"))
	}
	sc.Env = buildEnv(extraEnv, lookupEnv)
	return sc
}

func parseRisk(v string) (tool.Risk, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "safe":
		return tool.RiskSafe, nil
	case "moderate":
		return tool.RiskModerate, nil
	case "dangerous":
		return tool.RiskDangerous, nil
	default:
		return 0, fmt.Errorf("不合法的风险等级 %q（可选 safe / moderate / dangerous）", v)
	}
}

// varRe 匹配 ${VAR} 与 ${VAR:-default}。
//
// 只支持花括号形式，不支持裸 $VAR：参数里出现 "$" 很常见（正则、价格、
// shell 片段），os.ExpandEnv 会把它们悄悄吃掉，这类 bug 极难排查。
var varRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

type expander struct {
	lookup  func(string) (string, bool)
	missing []string
}

func (e *expander) expand(s string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return varRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := varRe.FindStringSubmatch(m)
		name, hasDefault, def := sub[1], sub[2] != "", sub[3]
		if v, ok := e.lookup(name); ok && v != "" {
			return v
		}
		if hasDefault {
			return def
		}
		if !slices.Contains(e.missing, name) {
			e.missing = append(e.missing, name)
		}
		return ""
	})
}

// baseEnvKeys 是子进程默认继承的环境变量白名单（见 docs/17 3.3）。
//
// 不继承完整环境：Go 进程的环境里有 OPENAI_API_KEY 等密钥，第三方 server
// 没有理由拿到。这和 run_command 的 TestRunCommandDoesNotLeakSecrets 是同一个立场。
// 需要其他变量就在配置的 env 里显式写 "X": "${X}"。
func baseEnvKeys() []string {
	keys := []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TERM", "TMPDIR"}
	if runtime.GOOS == "windows" {
		keys = append(keys, "SystemRoot", "SystemDrive", "APPDATA", "LOCALAPPDATA",
			"USERPROFILE", "HOMEDRIVE", "HOMEPATH", "PATHEXT", "COMSPEC", "TEMP", "TMP", "ProgramFiles")
	}
	return keys
}

// buildEnv 计算子进程的完整环境：白名单 + 配置里的 env（后者覆盖前者）。
func buildEnv(extra map[string]string, lookupEnv func(string) (string, bool)) []string {
	merged := map[string]string{}
	for _, k := range baseEnvKeys() {
		if v, ok := lookupEnv(k); ok {
			merged[k] = v
		}
	}
	for k, v := range extra {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}
