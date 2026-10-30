package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
)

// 参数签名与预校验（docs/17 12.4）。
//
// 模型经代理调用时看不到原生 schema，只能照着 search 返回的说明填参数。
// 把 JSON Schema 压成一行函数签名，比原始 schema 省 token，照着填也更准；
// 预校验只拦最常见、最确定的错误，其余交给 server 自己校验。

type param struct {
	name     string
	types    []string // 允许的 JSON 类型（空 = 不限）
	itemType string   // array 的元素类型
	enum     []json.RawMessage
	required bool
	desc     string
	complex  bool            // 嵌套对象 / 对象数组：签名里只标 object，另附原始 schema
	raw      json.RawMessage // 该参数的原始 schema
}

type signature struct {
	params    []param
	strictKey bool // additionalProperties: false
	parseErr  bool // schema 本身无法解析：放弃预校验，全部交给 server
}

type rawProp struct {
	Type        json.RawMessage            `json:"type"`
	Enum        []json.RawMessage          `json:"enum"`
	Description string                     `json:"description"`
	Items       *rawProp                   `json:"items"`
	Properties  map[string]json.RawMessage `json:"properties"`
}

// parseSignature 解析工具的 inputSchema。属性顺序按 schema 原文（用 Decoder 逐个读键），
// 必填参数排在前面——这是函数签名的常见读法。
func parseSignature(schema json.RawMessage) signature {
	var top struct {
		Properties           json.RawMessage `json:"properties"`
		Required             []string        `json:"required"`
		AdditionalProperties json.RawMessage `json:"additionalProperties"`
	}
	if err := json.Unmarshal(schema, &top); err != nil {
		return signature{parseErr: true}
	}
	sig := signature{strictKey: string(bytes.TrimSpace(top.AdditionalProperties)) == "false"}
	names, props, err := orderedObject(top.Properties)
	if err != nil {
		return signature{parseErr: true}
	}
	var req, opt []param
	for _, name := range names {
		p := param{name: name, raw: props[name], required: slices.Contains(top.Required, name)}
		var rp rawProp
		if json.Unmarshal(props[name], &rp) == nil {
			p.types = typeList(rp.Type)
			p.enum = rp.Enum
			p.desc = strings.TrimSpace(rp.Description)
			if len(rp.Properties) > 0 {
				p.complex = true
			}
			if rp.Items != nil {
				if it := typeList(rp.Items.Type); len(it) > 0 {
					p.itemType = it[0]
				}
				if len(rp.Items.Properties) > 0 || p.itemType == "object" {
					p.complex = true
				}
			}
		}
		if p.required {
			req = append(req, p)
		} else {
			opt = append(opt, p)
		}
	}
	sig.params = append(req, opt...)
	return sig
}

// orderedObject 按原文顺序读出 JSON 对象的键与值。空输入返回空结果。
func orderedObject(raw json.RawMessage) ([]string, map[string]json.RawMessage, error) {
	vals := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, vals, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, fmt.Errorf("properties 不是对象")
	}
	var names []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, nil, fmt.Errorf("properties 键不是字符串")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		if _, dup := vals[key]; !dup {
			names = append(names, key)
		}
		vals[key] = v
	}
	return names, vals, nil
}

// typeList 解析 "type" 字段：可能是字符串，也可能是字符串数组（如 ["string","null"]）。
func typeList(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil && one != "" {
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	return nil
}

func (p param) typeString() string {
	if len(p.enum) > 0 {
		parts := make([]string, 0, len(p.enum))
		for _, e := range p.enum {
			parts = append(parts, string(bytes.TrimSpace(e)))
		}
		return strings.Join(parts, "|")
	}
	if len(p.types) == 0 {
		return "any"
	}
	parts := make([]string, 0, len(p.types))
	for _, t := range p.types {
		if t == "array" && p.itemType != "" {
			t = p.itemType + "[]"
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, "|")
}

// render 生成「签名 + 逐个参数说明」，供 search 结果与参数报错使用。
func (s signature) render(fullName string) string {
	var b strings.Builder
	parts := make([]string, 0, len(s.params))
	for _, p := range s.params {
		opt := ""
		if !p.required {
			opt = "?"
		}
		parts = append(parts, fmt.Sprintf("%s%s: %s", p.name, opt, p.typeString()))
	}
	fmt.Fprintf(&b, "签名：%s(%s)\n", fullName, strings.Join(parts, ", "))
	if s.parseErr {
		b.WriteString("（该工具的参数 schema 无法解析，请按描述填写）\n")
	}
	for _, p := range s.params {
		req := "可选"
		if p.required {
			req = "必填"
		}
		line := fmt.Sprintf("- %s（%s）", p.name, req)
		if p.desc != "" {
			line += "：" + clip(p.desc, 200)
		}
		b.WriteString(line + "\n")
		if p.complex && len(p.raw) <= 600 {
			b.WriteString("  结构：" + compactJSON(p.raw) + "\n")
		}
	}
	return b.String()
}

func compactJSON(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

// validate 做最常见错误的预校验，返回问题列表（空 = 通过）。
func (s signature) validate(args json.RawMessage) []string {
	if s.parseErr {
		return nil
	}
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		trimmed = []byte(`{}`)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &got); err != nil {
		return []string{"arguments 必须是 JSON 对象"}
	}
	var problems []string
	known := map[string]bool{}
	for _, p := range s.params {
		known[p.name] = true
		v, ok := got[p.name]
		if !ok {
			if p.required {
				problems = append(problems, fmt.Sprintf("缺少必填参数 %s（%s）", p.name, p.typeString()))
			}
			continue
		}
		if len(p.types) > 0 && !matchesType(v, p.types) {
			problems = append(problems, fmt.Sprintf("参数 %s 应为 %s，实际是 %s", p.name, strings.Join(p.types, "|"), jsonKind(v)))
			continue
		}
		if len(p.enum) > 0 && !inEnum(v, p.enum) {
			problems = append(problems, fmt.Sprintf("参数 %s 的取值必须是 %s 之一", p.name, p.typeString()))
		}
	}
	if s.strictKey {
		for k := range got {
			if !known[k] {
				problems = append(problems, fmt.Sprintf("未知参数 %s", k))
			}
		}
		slices.Sort(problems) // map 遍历顺序随机，排序让报错稳定
	}
	return problems
}

// jsonKind 按首字符判断 JSON 值的类型。
func jsonKind(v json.RawMessage) string {
	t := bytes.TrimSpace(v)
	if len(t) == 0 {
		return "空"
	}
	switch t[0] {
	case '"':
		return "string"
	case '{':
		return "object"
	case '[':
		return "array"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

func matchesType(v json.RawMessage, types []string) bool {
	kind := jsonKind(v)
	for _, t := range types {
		if t == kind {
			return true
		}
		// integer 是 number 的子集：1、1.0、1e2 都算整数。
		if t == "integer" && kind == "number" {
			var f float64
			if json.Unmarshal(v, &f) == nil && f == math.Trunc(f) {
				return true
			}
		}
	}
	return false
}

// inEnum 按 JSON 语义比较（"a" 与 "a"、1 与 1.0 相等），而不是按原文字节。
func inEnum(v json.RawMessage, enum []json.RawMessage) bool {
	canon := func(raw json.RawMessage) string {
		var x any
		if json.Unmarshal(raw, &x) != nil {
			return string(raw)
		}
		b, _ := json.Marshal(x)
		return string(b)
	}
	cv := canon(v)
	for _, e := range enum {
		if canon(e) == cv {
			return true
		}
	}
	return false
}
