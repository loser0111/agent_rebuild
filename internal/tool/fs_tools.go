package tool

// 文件系统工具：读、写、编辑、目录列举、搜索。
//
// 设计要点（参考 Codex / Claude Code）：
//   - 所有路径都限制在 workspace 内，防越界读写
//   - 读文件默认带行号（模型据此定位编辑位置）
//   - 写文件需要先读（避免盲写覆盖），由模型自觉遵循提示
//   - 输出强制截断，防止大文件撑爆上下文

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// 输出上限（字符数）。超过会截断并提示模型。
const (
	maxReadChars    = 30000
	maxListEntries  = 500
	maxSearchResult = 200
	maxLineLen      = 500 // 单行最大显示长度
)

// fsConfig 是文件工具的公共配置。
type fsConfig struct {
	// Root 工作区根目录（所有路径都限制在此之内）。
	Root string
	// MaxReadChars 单次读取上限。
	MaxReadChars int
}

// resolve 把用户给的相对路径解析为绝对路径，并确保不越界。
func (c fsConfig) resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("path 不能为空")
	}
	root, err := filepath.Abs(c.Root)
	if err != nil {
		return "", fmt.Errorf("工作目录无效: %w", err)
	}

	var abs string
	if filepath.IsAbs(p) {
		abs = filepath.Clean(p)
	} else {
		abs = filepath.Join(root, p)
	}
	abs = filepath.Clean(abs)

	// 防越界：解析后的路径必须仍在 root 下。
	// 用 EvalSymlinks 前先做前缀检查（符号链接可能指向外部，见下方说明）。
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径 %q 超出工作目录 %q 的范围", p, root)
	}
	return abs, nil
}

// maxReadCharsOrDefault 返回生效的读取上限。
func (c fsConfig) maxReadCharsOrDefault() int {
	if c.MaxReadChars > 0 {
		return c.MaxReadChars
	}
	return maxReadChars
}

// ---- read_file ----

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type readFileTool struct{ cfg fsConfig }

// NewReadFileTool 创建读取文件工具。
func NewReadFileTool(root string) Tool {
	return &readFileTool{cfg: fsConfig{Root: root}}
}

func (t *readFileTool) Spec() Spec {
	return Spec{
		Name: "read_file",
		Description: "读取文件内容，返回带行号的文本。用于了解代码或配置的现状。" +
			"大文件可用 offset/limit 分段读取（行号从 1 开始）。" +
			"修改文件前应先读取它。",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"相对于工作目录的文件路径"},
				"offset":{"type":"integer","description":"起始行号（从 1 开始），可选"},
				"limit":{"type":"integer","description":"最多读取行数，可选"}
			},
			"required":["path"]
		}`),
		Risk: RiskSafe,
	}
}

func (t *readFileTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var args readFileArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return Result{}, err
	}
	abs, err := t.cfg.resolve(args.Path)
	if err != nil {
		return Result{}, err
	}

	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Result{Output: "文件不存在: " + args.Path, IsError: true}, nil
		}
		return Result{}, err
	}
	if info.IsDir() {
		return Result{Output: args.Path + " 是目录，请用 list_dir", IsError: true}, nil
	}
	// 大小保护：超大文件直接拒绝，避免读入内存。
	if info.Size() > 10<<20 {
		return Result{
			Output:  fmt.Sprintf("文件过大（%.1f MB），请用 offset/limit 分段读取", float64(info.Size())/(1<<20)),
			IsError: true,
		}, nil
	}

	f, err := os.Open(abs)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = f.Close() }()

	offset := args.Offset
	if offset < 0 {
		offset = 0
	}
	var sb strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	lineNo := 0
	written := 0
	truncated := false
	for scanner.Scan() {
		lineNo++
		if offset > 0 && lineNo < offset {
			continue
		}
		if args.Limit > 0 && written >= args.Limit {
			truncated = true
			break
		}
		line := scanner.Text()
		if len([]rune(line)) > maxLineLen {
			line = string([]rune(line)[:maxLineLen]) + " …（本行过长已截断）"
		}
		fmt.Fprintf(&sb, "%6d\t%s\n", lineNo, line)
		written++

		// 字符级上限保护。
		if sb.Len() > t.cfg.maxReadCharsOrDefault() {
			truncated = true
			break
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return Result{}, fmt.Errorf("读取文件失败: %w", err)
	}
	if written == 0 {
		return Result{
			Output:  fmt.Sprintf("文件中没有内容（offset=%d 可能超出文件末尾，共 %d 行）", offset, lineNo),
			IsError: false,
		}, nil
	}

	out := sb.String()
	if truncated {
		out += fmt.Sprintf("\n... 输出已截断（已读到第 %d 行）。可用 offset=%d 继续读取。\n",
			lineNo, lineNo+1)
	}
	return Result{Output: out, Truncated: truncated}, nil
}

// ---- write_file ----

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type writeFileTool struct{ cfg fsConfig }

// NewWriteFileTool 创建写文件工具。
func NewWriteFileTool(root string) Tool {
	return &writeFileTool{cfg: fsConfig{Root: root}}
}

func (t *writeFileTool) Spec() Spec {
	return Spec{
		Name: "write_file",
		Description: "创建新文件或整体覆盖已有文件。父目录会自动创建。" +
			"修改已有文件请优先用 edit_file（只替换局部，更安全）。" +
			"整体覆盖会丢失原内容，务必确认。",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"相对于工作目录的文件路径"},
				"content":{"type":"string","description":"完整的文件内容"}
			},
			"required":["path","content"]
		}`),
		Risk: RiskModerate,
	}
}

func (t *writeFileTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var args writeFileArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return Result{}, err
	}
	abs, err := t.cfg.resolve(args.Path)
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return Result{}, fmt.Errorf("创建目录失败: %w", err)
	}

	existed := false
	var oldSize int64
	if info, statErr := os.Stat(abs); statErr == nil {
		existed = true
		oldSize = info.Size()
	}

	// 权限 0644：常规源码文件权限。
	if err := os.WriteFile(abs, []byte(args.Content), 0o644); err != nil {
		return Result{}, fmt.Errorf("写入失败: %w", err)
	}

	action := "已创建"
	if existed {
		action = "已覆盖"
	}
	lines := strings.Count(args.Content, "\n")
	if !strings.HasSuffix(args.Content, "\n") && args.Content != "" {
		lines++
	}
	out := fmt.Sprintf("%s %s（%d 行，%d 字节）", action, args.Path, lines, len(args.Content))
	if existed && oldSize > 0 {
		out += fmt.Sprintf("，原文件 %d 字节", oldSize)
	}
	return Result{Output: out}, nil
}

// ---- edit_file ----

type editFileArgs struct {
	Path      string `json:"path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
	// ReplaceAll 为 true 时替换全部匹配（默认只替换唯一匹配）。
	ReplaceAll bool `json:"replace_all,omitempty"`
}

type editFileTool struct{ cfg fsConfig }

// NewEditFileTool 创建编辑文件工具（精确字符串替换）。
func NewEditFileTool(root string) Tool {
	return &editFileTool{cfg: fsConfig{Root: root}}
}

func (t *editFileTool) Spec() Spec {
	return Spec{
		Name: "edit_file",
		Description: "在文件中精确替换一段文本。old_string 必须与文件内容**完全一致**（含缩进），" +
			"且默认必须唯一出现；如需替换多处请设 replace_all=true。" +
			"这是修改代码的推荐方式，比整体覆盖安全。",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"相对于工作目录的文件路径"},
				"old_string":{"type":"string","description":"要被替换的原文本（需完全一致）"},
				"new_string":{"type":"string","description":"替换后的新文本"},
				"replace_all":{"type":"boolean","description":"是否替换全部匹配，默认 false"}
			},
			"required":["path","old_string","new_string"]
		}`),
		Risk: RiskModerate,
	}
}

func (t *editFileTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var args editFileArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return Result{}, err
	}
	if args.OldString == "" {
		return Result{Output: "old_string 不能为空（要替换全部内容请用 write_file）", IsError: true}, nil
	}
	if args.OldString == args.NewString {
		return Result{Output: "old_string 与 new_string 相同，无需修改", IsError: true}, nil
	}
	abs, err := t.cfg.resolve(args.Path)
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	original, err := os.ReadFile(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Result{Output: "文件不存在: " + args.Path, IsError: true}, nil
		}
		return Result{}, fmt.Errorf("读取失败: %w", err)
	}
	content := string(original)

	count := strings.Count(content, args.OldString)
	if count == 0 {
		// 给出有用提示：模型常因缩进/空白不匹配而失败。
		return Result{
			Output: "未找到 old_string。请确认它与文件内容完全一致（包括缩进与换行）。" +
				"建议先 read_file 查看实际内容。",
			IsError: true,
		}, nil
	}
	if count > 1 && !args.ReplaceAll {
		return Result{
			Output: fmt.Sprintf("old_string 出现了 %d 次，不唯一。请提供更多上下文使其唯一，"+
				"或设 replace_all=true 替换全部。", count),
			IsError: true,
		}, nil
	}

	var updated string
	if args.ReplaceAll {
		updated = strings.ReplaceAll(content, args.OldString, args.NewString)
	} else {
		updated = strings.Replace(content, args.OldString, args.NewString, 1)
	}

	// 保留原文件权限。
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(abs); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(abs, []byte(updated), mode); err != nil {
		return Result{}, fmt.Errorf("写入失败: %w", err)
	}

	replaced := 1
	if args.ReplaceAll {
		replaced = count
	}
	return Result{
		Output: fmt.Sprintf("已修改 %s（替换 %d 处，%d 字节 → %d 字节）",
			args.Path, replaced, len(content), len(updated)),
	}, nil
}

// ---- list_dir ----

type listDirArgs struct {
	Path      string `json:"path,omitempty"`
	Recursive bool   `json:"recursive,omitempty"`
}

type listDirTool struct{ cfg fsConfig }

// NewListDirTool 创建目录列举工具。
func NewListDirTool(root string) Tool {
	return &listDirTool{cfg: fsConfig{Root: root}}
}

func (t *listDirTool) Spec() Spec {
	return Spec{
		Name: "list_dir",
		Description: "列出目录内容（目录在前，文件在后，含大小）。" +
			"用于了解项目结构。recursive=true 会递归列出（层次较深时慎用）。" +
			"自动跳过 .git、node_modules 等噪音目录。",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"path":{"type":"string","description":"相对于工作目录的路径，默认为工作目录根"},
				"recursive":{"type":"boolean","description":"是否递归，默认 false"}
			}
		}`),
		Risk: RiskSafe,
	}
}

// 跳过的高噪音目录。
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, ".idea": true, ".vscode": true,
	"dist": true, "build": true, "release": true, ".next": true,
	"__pycache__": true, ".venv": true, "venv": true, "target": true,
	".cache": true, "vendor": true,
}

func (t *listDirTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var args listDirArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return Result{}, err
	}
	if args.Path == "" {
		args.Path = "."
	}
	abs, err := t.cfg.resolve(args.Path)
	if err != nil {
		return Result{}, err
	}

	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Result{Output: "目录不存在: " + args.Path, IsError: true}, nil
		}
		return Result{}, err
	}
	if !info.IsDir() {
		return Result{Output: args.Path + " 不是目录，请用 read_file", IsError: true}, nil
	}

	var sb strings.Builder
	count := 0
	truncated := false

	walkFn := func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // 忽略权限错误等，继续遍历
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() && skipDirs[d.Name()] && p != abs {
			return fs.SkipDir
		}
		if p == abs {
			return nil
		}
		if count >= maxListEntries {
			truncated = true
			return fs.SkipAll
		}

		rel, relErr := filepath.Rel(abs, p)
		if relErr != nil {
			rel = p
		}
		indent := strings.Repeat("  ", strings.Count(rel, string(filepath.Separator)))

		if d.IsDir() {
			fmt.Fprintf(&sb, "%s%s/\n", indent, d.Name())
		} else {
			size := int64(0)
			if fi, statErr := d.Info(); statErr == nil {
				size = fi.Size()
			}
			fmt.Fprintf(&sb, "%s%s (%s)\n", indent, d.Name(), humanSize(size))
		}
		count++

		if !args.Recursive && d.IsDir() {
			return fs.SkipDir
		}
		return nil
	}

	if args.Recursive {
		err = filepath.WalkDir(abs, walkFn)
	} else {
		// 非递归：只列一层。
		entries, readErr := os.ReadDir(abs)
		if readErr != nil {
			return Result{}, fmt.Errorf("读取目录失败: %w", readErr)
		}
		err = nil
		for _, e := range entries {
			if count >= maxListEntries {
				truncated = true
				break
			}
			name := e.Name()
			if e.IsDir() {
				if skipDirs[name] {
					continue
				}
				fmt.Fprintf(&sb, "%s/\n", name)
			} else {
				size := int64(0)
				if fi, statErr := e.Info(); statErr == nil {
					size = fi.Size()
				}
				fmt.Fprintf(&sb, "%s (%s)\n", name, humanSize(size))
			}
			count++
		}
	}
	if err != nil {
		return Result{}, err
	}

	if count == 0 {
		return Result{Output: "目录为空: " + args.Path}, nil
	}
	out := sb.String()
	if truncated {
		out += fmt.Sprintf("\n... 已列出 %d 项后截断，请缩小范围\n", count)
	}
	return Result{Output: out, Truncated: truncated}, nil
}

func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// ---- search ----

type searchArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	Glob       string `json:"glob,omitempty"`
	IgnoreCase bool   `json:"ignore_case,omitempty"`
	MaxResults int    `json:"max_results,omitempty"`
}

type searchTool struct{ cfg fsConfig }

// NewSearchTool 创建内容搜索工具（类似 grep）。
func NewSearchTool(root string) Tool {
	return &searchTool{cfg: fsConfig{Root: root}}
}

func (t *searchTool) Spec() Spec {
	return Spec{
		Name: "search",
		Description: "在文件中搜索正则表达式，返回 文件:行号:内容。" +
			"这是定位代码的首选方式（比逐个读文件快得多）。" +
			"可用 glob 限定文件类型，如 \"*.go\"。",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"pattern":{"type":"string","description":"正则表达式"},
				"path":{"type":"string","description":"搜索起点目录，默认为工作目录根"},
				"glob":{"type":"string","description":"文件名通配符，如 *.go、*.ts"},
				"ignore_case":{"type":"boolean","description":"是否忽略大小写"},
				"max_results":{"type":"integer","description":"最多返回条数，默认 100"}
			},
			"required":["pattern"]
		}`),
		Risk: RiskSafe,
	}
}

func (t *searchTool) Execute(ctx context.Context, raw json.RawMessage) (Result, error) {
	var args searchArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return Result{}, err
	}
	if args.Pattern == "" {
		return Result{Output: "pattern 不能为空", IsError: true}, nil
	}
	if args.Path == "" {
		args.Path = "."
	}
	abs, err := t.cfg.resolve(args.Path)
	if err != nil {
		return Result{}, err
	}

	expr := args.Pattern
	if args.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return Result{Output: fmt.Sprintf("正则表达式无效：%v", err), IsError: true}, nil
	}

	maxResults := args.MaxResults
	if maxResults <= 0 || maxResults > maxSearchResult {
		maxResults = 100
	}
	// 二进制文件与超长行保护。
	const maxFileBytes = 2 << 20

	var sb strings.Builder
	matches := 0
	truncated := false

	walkErr := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && p != abs {
				return fs.SkipDir
			}
			return nil
		}
		if args.Glob != "" {
			ok, globErr := filepath.Match(args.Glob, d.Name())
			if globErr != nil || !ok {
				return nil
			}
		}
		info, statErr := d.Info()
		if statErr != nil || info.Size() > maxFileBytes {
			return nil
		}

		f, openErr := os.Open(p)
		if openErr != nil {
			return nil
		}
		defer func() { _ = f.Close() }()

		rel, relErr := filepath.Rel(abs, p)
		if relErr != nil {
			rel = p
		}

		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			// 跳过疑似二进制内容。
			if lineNo == 1 && strings.ContainsRune(line, 0) {
				return nil
			}
			if !re.MatchString(line) {
				continue
			}
			if matches >= maxResults {
				truncated = true
				return fs.SkipAll
			}
			display := line
			if len([]rune(display)) > maxLineLen {
				display = string([]rune(display)[:maxLineLen]) + " …"
			}
			fmt.Fprintf(&sb, "%s:%d: %s\n", rel, lineNo, strings.TrimSpace(display))
			matches++
		}
		return nil
	})
	if walkErr != nil {
		return Result{}, walkErr
	}

	if matches == 0 {
		return Result{Output: fmt.Sprintf("未找到匹配 %q 的内容", args.Pattern)}, nil
	}
	out := sb.String()
	if truncated {
		out += fmt.Sprintf("\n... 已达 %d 条上限，结果被截断。请用更精确的 pattern 或 glob。\n", maxResults)
	}
	return Result{Output: out, Truncated: truncated}, nil
}

// sortStrings 是 sort.Strings 的别名，便于测试使用。
var _ = sort.Strings
