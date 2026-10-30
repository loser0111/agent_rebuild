package router

import (
	"strings"
	"unicode"
)

// 搜索打分（docs/17 12.3）。不引入依赖：工具数量在百级，线性扫描 + 简单打分足够。
//
// 每个关键词在「名字 / 参数名 / 描述 / server 说明」里打分，关键词之间相加；
// 全部关键词都命中额外加分。名字命中权重最高：用户说「issue」时，
// 名字里带 issue 的工具几乎总比描述里提到 issue 的更相关。
const (
	scoreName       = 6
	scoreParam      = 3
	scoreDesc       = 2
	scoreFuzzy      = 3
	scoreCJKMax     = 2
	scoreAllMatched = 2
)

// tokenize 按空白与常见分隔符切分关键词，统一小写、去重。
func tokenize(q string) []string {
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",，;；、|", r)
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// scoreEntry 返回总分与「是否所有关键词都命中」。
func scoreEntry(ix *indexed, tokens []string) (float64, bool) {
	total, matched := 0.0, 0
	for _, tok := range tokens {
		s := scoreToken(ix, tok)
		if s > 0 {
			matched++
			total += s
		}
	}
	all := matched == len(tokens)
	if all && len(tokens) > 1 {
		total += scoreAllMatched
	}
	return total, all
}

func scoreToken(ix *indexed, tok string) float64 {
	score := 0.0
	nameHit := strings.Contains(ix.nameLower, tok) || strings.Contains(ix.nameJoined, tok)
	if nameHit {
		score += scoreName
	}
	for _, p := range ix.paramNames {
		if strings.Contains(p, tok) {
			score += scoreParam
			break
		}
	}
	descHit := strings.Contains(ix.descLower, tok)
	if descHit {
		score += scoreDesc
	}
	if strings.Contains(ix.serverDesc, tok) {
		score += scoreDesc
	}
	// 拼写容错：只对名字分段做，且只在没有精确命中名字时才算（避免重复计分）。
	if !nameHit && fuzzyNameMatch(ix.segments, tok) {
		score += scoreFuzzy
	}
	// 中文没有空格分词：「创建工单」要能匹配「创建 Jira 工单」，按二字组算命中比例。
	if !descHit && hasHan(tok) {
		score += scoreCJKMax * bigramCoverage(tok, ix.descLower+" "+ix.serverDesc)
	}
	return score
}

// fuzzyNameMatch：关键词与某个名字分段（或分段的同长前缀，容忍复数/后缀）编辑距离足够小。
// 只对 >=4 个字符的 ASCII 关键词启用：短词容错会把 "get" 匹配成 "set"、"put"。
func fuzzyNameMatch(segments []string, tok string) bool {
	if len(tok) < 4 || hasHan(tok) {
		return false
	}
	maxDist := 1
	if len(tok) >= 8 {
		maxDist = 2
	}
	for _, seg := range segments {
		if levenshtein(tok, seg, maxDist) <= maxDist {
			return true
		}
		// 与分段前缀比较：「isue」对「issues」的前缀「issu」距离为 1。
		for _, n := range []int{len(tok) - 1, len(tok), len(tok) + 1} {
			if n > 0 && n < len(seg) && levenshtein(tok, seg[:n], maxDist) <= maxDist {
				return true
			}
		}
	}
	return false
}

// splitSegments 把工具名按 _ - / . 空格 和驼峰切成小写分段。
func splitSegments(name string) []string {
	var segs []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			segs = append(segs, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	runes := []rune(name)
	for i, r := range runes {
		switch {
		case r == '_' || r == '-' || r == '/' || r == '.' || unicode.IsSpace(r):
			flush()
		case unicode.IsUpper(r) && i > 0 && unicode.IsLower(runes[i-1]):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return segs
}

func hasHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// bigramCoverage 返回关键词的二字组在 text 中出现的比例（0~1）。单字关键词按子串算。
func bigramCoverage(tok, text string) float64 {
	r := []rune(tok)
	if len(r) < 2 {
		if strings.Contains(text, tok) {
			return 1
		}
		return 0
	}
	hit := 0
	for i := 0; i+1 < len(r); i++ {
		if strings.Contains(text, string(r[i:i+2])) {
			hit++
		}
	}
	return float64(hit) / float64(len(r)-1)
}

// levenshtein 计算按字符的编辑距离；超过 limit 时提前返回 limit+1。
func levenshtein(a, b string, limit int) int {
	ra, rb := []rune(a), []rune(b)
	if d := len(ra) - len(rb); d > limit || -d > limit {
		return limit + 1
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			rowMin = min(rowMin, cur[j])
		}
		if rowMin > limit {
			return limit + 1
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}
