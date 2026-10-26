package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// NewAskUserTool 创建「向用户提问」工具。
//
// 为什么需要它：模型遇到信息不足时，只能靠猜或者在正文里反问。
// 反问的问题是——正文里的问题**不会中断执行**，模型会继续说下去，
// 用户即使回答了也要重新组织一轮请求。这个工具把「问」变成一次
// 显式的暂停点：拿到答复之前不会继续。
//
// 与审批（Approver）的区别：审批是系统不信任模型，问的是「能不能做」；
// ask_user 是模型主动求助，问的是「是什么」。两者共用挂起机制，
// 但语义、风险、resume 后的处理都不同。
type askUserTool struct{}

// NewAskUserTool 创建「向用户提问」工具实例。
func NewAskUserTool() Tool { return &askUserTool{} }

func (t *askUserTool) Spec() Spec {
	return Spec{
		Name: "ask_user",
		Description: "向用户提问并暂停执行，直到收到答复。用于：需求有歧义、缺少只有用户知道的信息" +
			"（业务口径、密钥位置、部署环境）、需要在几种方案中让用户拍板。" +
			"**一次可以提多个问题（questions 数组），请尽量一次问完**——" +
			"每多问一轮，用户就要多等一次，体验很差。" +
			"不要用它确认显而易见的事，也不要用它代替自己的判断。" +
			"每个问题可带 options（用户点选）或不带（自由输入）。",
		Parameters: json.RawMessage(`{
			"type":"object",
			"properties":{
				"questions":{
					"type":"array",
					"description":"要问用户的问题列表。一次最多 6 个；所有问题会在界面上同时展示，用户一次答完。",
					"items":{
						"type":"object",
						"properties":{
							"id":{"type":"string","description":"可选的短标识（如 db、scope），用于在结果里对应；不传则按序号"},
							"question":{"type":"string","description":"问题文本，要具体、可回答"},
							"options":{
								"type":"array",
								"items":{"type":"string"},
								"description":"可选项（2-5 个）；不传表示让用户自由输入"
							},
							"multi":{"type":"boolean","description":"是否允许多选；默认 false"}
						},
						"required":["question"]
					}
				},
				"question":{"type":"string","description":"（旧格式）单个问题；仅在不方便用 questions 时使用"},
				"options":{"type":"array","items":{"type":"string"},"description":"（旧格式）单问题的选项"}
			}
		}`),
		// 无副作用：只是问一句话，不读不写任何资源。
		Risk: RiskSafe,
		// 但它是「交互型」：必须等用户答复才能产出结果。
		Interactive: true,
	}
}

// Execute 在正常路径下不会被调用——
// 交互型工具由 loop 在执行前拦截并挂起（见 loop.suspendAll）。
//
// 这里保留实现是为了覆盖两种情况：
//  1. 直接调用（测试、脚本）时给出明确说明；
//  2. 未配置 Checkpointer 时，loop 会退化为调用它，
//     返回一条「无法提问」的工具结果让模型自己想办法继续，
//     而不是让整个请求失败。
func (t *askUserTool) Execute(_ context.Context, raw json.RawMessage) (Result, error) {
	qs, err := ParseAskUserQuestions(raw)
	if err != nil {
		return Result{}, err
	}
	var first string
	if len(qs) > 0 {
		first = qs[0].Question
	}
	return Result{
		Output: fmt.Sprintf(
			"当前环境不支持挂起等待用户回答，因此问题「%s」未能送达用户。"+
				"请基于现有信息继续，或直接在最终回答里说明需要用户补充什么。", first),
		IsError: true,
	}, nil
}

// AskQuestion 是 ask_user 里的单个问题。
type AskQuestion struct {
	// ID 短标识，便于在工具结果里对应；为空时由序号生成。
	ID string `json:"id"`
	// Question 问题文本。
	Question string `json:"question"`
	// Options 可选项；为空表示自由输入。
	Options []string `json:"options"`
	// Multi 是否允许多选。
	Multi bool `json:"multi"`
}

// askUserArgs 是 ask_user 的原始参数。
//
// 同时接受两种形态：
//   - questions: [...]  ← 推荐，一次问多个
//   - question + options ← 旧格式，单个问题（保持向后兼容）
type askUserArgs struct {
	Questions []AskQuestion `json:"questions"`
	Question  string        `json:"question"`
	Options   []string      `json:"options"`
}

// 单次调用的上限。
//
// 为什么不设更大：界面上同时摊开十几个问题，用户根本答不下去，
// 反而会关掉窗口。6 个已经接近「一次性表单」的合理上限。
const maxQuestionsPerCall = 6

// 每个问题的选项上限。
const maxOptionsPerQuestion = 5

// ParseAskUserQuestions 解析并校验参数，返回规范化后的问题列表。
//
// 这是 loop 挂起前与工具本体共用的唯一入口：
// 参数不合法必须在挂起前拦下，否则会存下一个用户无从回答的空白卡片。
func ParseAskUserQuestions(raw json.RawMessage) ([]AskQuestion, error) {
	var args askUserArgs
	if err := DecodeArgs(raw, &args); err != nil {
		return nil, err
	}

	// 合并两种形态：questions 优先，question 作为补充。
	candidates := make([]AskQuestion, 0, len(args.Questions)+1)
	candidates = append(candidates, args.Questions...)
	if strings.TrimSpace(args.Question) != "" {
		candidates = append(candidates, AskQuestion{
			Question: args.Question,
			Options:  args.Options,
		})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("ask_user: 至少需要一个问题（questions 或 question）")
	}
	if len(candidates) > maxQuestionsPerCall {
		return nil, fmt.Errorf("ask_user: 一次最多 %d 个问题，实际 %d 个；请拆分或精简",
			maxQuestionsPerCall, len(candidates))
	}

	out := make([]AskQuestion, 0, len(candidates))
	seenID := make(map[string]bool, len(candidates))
	for i, q := range candidates {
		text := strings.TrimSpace(q.Question)
		if text == "" {
			return nil, fmt.Errorf("ask_user: 第 %d 个问题的 question 不能为空", i+1)
		}

		id := strings.TrimSpace(q.ID)
		if id == "" {
			// 没给 id 就用序号：resume 时前端要靠它对应答案，
			// 不能为空。
			id = fmt.Sprintf("q%d", i+1)
		}
		// id 重复会导致答案错位（两条答案写到同一个键上），
		// 加后缀区分而不是报错——模型给重复 id 是常见的小失误。
		if seenID[id] {
			id = fmt.Sprintf("%s_%d", id, i+1)
		}
		seenID[id] = true

		out = append(out, AskQuestion{
			ID:       id,
			Question: text,
			Options:  cleanOptions(q.Options),
			Multi:    q.Multi,
		})
	}
	return out, nil
}

// ParseAskUserArgs 是旧接口的兼容包装，返回第一个问题。
//
// 保留它是因为「一问一答」仍是最常见形态，调用方不该为单个问题
// 去处理列表。
func ParseAskUserArgs(raw json.RawMessage) (question string, options []string, err error) {
	qs, err := ParseAskUserQuestions(raw)
	if err != nil {
		return "", nil, err
	}
	return qs[0].Question, qs[0].Options, nil
}

// cleanOptions 去空、去重、限长。
//
// 模型经常一口气给 8 个选项，界面上排不下、用户也难以选择。
func cleanOptions(raw []string) []string {
	if len(raw) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, opt := range raw {
		opt = strings.TrimSpace(opt)
		if opt == "" || seen[opt] {
			continue
		}
		seen[opt] = true
		out = append(out, opt)
		if len(out) == maxOptionsPerQuestion {
			break
		}
	}
	return out
}
