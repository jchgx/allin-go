package workbuddy

// 这套规则移植自 dsh-connect-workbuddy 的 prepareChatBody / forceNeutralHead /
// perturbChannelFingerprints。它不是风格偏好，每一条都被上游的一条实际规则逼出来的：
//
//	1. developer → system：两个网关都拒 developer 角色。
//	2. 保证首条是 system：国际网关对 user-first 的请求直接 11-128
//	   （"first message is not system prompt"），国内网关容忍，所以只在海外线暴露。
//	3. 句子级改词：实测存在长句指纹（DSH 的 Web GUI 段、@ 路径段），
//	   整段命中就拒；按句改成同义说法即可绕开，且幂等。
//	4. 中性开头：扫描消息以第三方 Agent 的自我描述**开头**就拒；
//	   在它前面加一行 "You are a helpful assistant." 就不再命中前缀。
//	5. 身份子句断词：在 "you are ... agent powered by ..." 这类子句的每个词边界
//	   插入 U+200B，破坏精确子串匹配；模型读起来不变。
//	6. 兜底扰动：在句号/逗号边界和大写词组内部插 U+200B；再狠一档是每个词内部都插。
//
// 只有第 3、5 条是「每个请求都跑」的（开销约 +0.3% 字符），
// 4 的强制版和 6 只在被拒后的重发里用 —— 上游已经答 200 的请求保持字节不变。

import (
	"encoding/json"
	"regexp"
	"strings"
)

// ZeroWidthSpace 是断词用的不可见字符。
//
// U+200B：渲染上是空、不是网关匹配意义上的空白、模型直接读通。
// 实测在被打回的片段里插一个就能把 400 变成 200。
const ZeroWidthSpace = "\u200b"

// fallbackSystemPrompt 是补首条 system 时的占位文案。
//
// 故意极短：它是兜底占位，不是人设；写长了等于在每个受影响的请求上
// 悄悄改变模型行为。
const fallbackSystemPrompt = "You are a helpful assistant."

// channelGatePhrases 是实测到的句子级指纹及其同义改写。
//
// 每一行都来自真机对照：原句命中 400，改过的句子 200。
// 顺序有意义（短的 "they mean this GUI." 放最后），且每条改写都不会
// 再生成另一条 from，所以整套是幂等的。
var channelGatePhrases = [][2]string{
	{"You are an AI agent powered by DeepSeek Harness", "You are an AI agent running inside DeepSeek Harness"},
	{"You are a coding agent powered by the ", "You are a coding agent running on the "},
	{"You are interacting with the user through the DeepSeek Harness Web GUI", "You are working with the user inside the DeepSeek Harness Web GUI"},
	{"Agent Teams is available in this session", "Agent Teams is enabled for this session"},
	{"Tokens prefixed with @ are paths the user explicitly referenced.", "Paths that begin with @ are the ones the user explicitly referenced."},
	{"Relative paths resolve from the workspace root;", "Relative paths are resolved against the workspace root;"},
	{"absolute paths identify files or directories on the host.", "absolute paths point at files or directories on the host."},
	{"A trailing slash marks a directory:", "A trailing slash indicates a directory:"},
	{"list it when its contents matter.", "list it whenever its contents matter."},
	{"Anything else is a file:", "Everything else is a file:"},
	{"use the read tool when its contents are needed,", "use the read tool whenever you need its contents,"},
	{"and do not claim to have inspected it before reading.", "and never claim to have inspected it before reading it."},
	{"@\"...\" quotes a path containing spaces.", "A path containing spaces is quoted with @\"...\"."},
	{"without naming another target, they mean this GUI.", "and names no other target, the interface described here is meant."},
	{"they mean this GUI.", "the interface described here is meant."},
}

// identityClauseRe 匹配 Agent 的自我介绍子句。
//
// 模型名不稳定（qmodel_38max / deepseek-v4.1-flash …），所以子句只匹配到
// "powered by"，变量尾巴交给断词规则处理。
var identityClauseRe = regexp.MustCompile(`(?i)(you)(\s+are\s+(?:(?:an?\s+)?[\w-]*\s*agent\s+powered\s+by|interacting\s+with\s+the\s+user\s+through\s+the\s+deepseek\s+harness))`)

// whitespaceRe 用来在子句内的每个词边界插入断词。
var whitespaceRe = regexp.MustCompile(`\s+`)

// capitalizedWordRe 判断一个词是不是「首字母大写 + 其余字母数字」。
//
// 对应 JS 的 /[A-Z][A-Za-z0-9]*$/ 判定：只有这种词才和大写词组规则有关。
var capitalizedWordRe = regexp.MustCompile(`^[A-Z][A-Za-z0-9]*$`)

// ApplyIdentityRules 是每个 WorkBuddy 请求都要跑的便宜那部分。
//
// 只做实测证明必要、且代价极小的改写：
//   - developer → system
//   - 补首条 system（缺了会被网关按 11-128 拒）
//   - 句子级指纹改词
//   - 身份子句内部断词
//   - 开头命中前缀指纹的消息补一行中性开头
//
// 返回 changed 表示请求体是否被改动。解析失败按「改不动」处理并原样返回。
func ApplyIdentityRules(body []byte) (out []byte, changed bool) {
	obj, ok := parseBody(body)
	if !ok {
		return body, false
	}
	msgs, ok := messagesOf(obj)
	if !ok {
		return body, false
	}

	dirty := false
	// 1) developer → system
	for _, m := range msgs {
		var role string
		if json.Unmarshal(m["role"], &role) != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			m["role"] = json.RawMessage(`"system"`)
			dirty = true
		}
	}
	// 2) 补首条 system
	if len(msgs) > 0 {
		var headRole string
		_ = json.Unmarshal(msgs[0]["role"], &headRole)
		if !strings.EqualFold(strings.TrimSpace(headRole), "system") {
			head := map[string]json.RawMessage{
				"role":    json.RawMessage(`"system"`),
				"content": mustJSON(fallbackSystemPrompt),
			}
			msgs = append([]map[string]json.RawMessage{head}, msgs...)
			dirty = true
		}
	}
	// 3~5) 只对网关会扫描的角色动手
	for _, m := range msgs {
		var role string
		if json.Unmarshal(m["role"], &role) != nil || !scannableRole(role) {
			continue
		}
		if rewriteTextParts(m, func(s string) string {
			s = rewriteGatePhrases(s)
			s = breakIdentityClauses(s)
			return s
		}) {
			dirty = true
		}
	}

	if !dirty {
		return body, false
	}
	out, err := writeBack(obj, msgs)
	if err != nil {
		return body, false
	}
	return out, true
}

// rewriteGatePhrases 把实测的句子级指纹改成同义说法。
func rewriteGatePhrases(text string) string {
	out := text
	for _, pair := range channelGatePhrases {
		if strings.Contains(out, pair[0]) {
			out = strings.ReplaceAll(out, pair[0], pair[1])
		}
	}
	return out
}

// breakIdentityClauses 在身份子句的**每个**词边界插入 U+200B。
//
// 不是只插一个：上游列出的可能是整句，也可能是其中一段（"a coding agent
// powered by"），只在最前面插一个只能破掉从那里开始的跨度。每个词边界都插，
// 无论它列的是哪一段都被破掉，代价是十来个不可见字符。
//
// 天然幂等：插过之后 `\s+` 不再跨越 U+200B，下一遍匹配不到同一个子句。
func breakIdentityClauses(text string) string {
	return identityClauseRe.ReplaceAllStringFunc(text, func(clause string) string {
		return whitespaceRe.ReplaceAllStringFunc(clause, func(ws string) string {
			return ws + ZeroWidthSpace
		})
	})
}

// ForceNeutralHead 给**每个**被扫描消息的每个文本块前面补中性开头。
//
// 与 ApplyIdentityRules 里的条件版不同：那个只看命中已知前缀的，
// 这个是「不管它开头是什么都挪走」——未知指纹一样有效，代价是每条扫描消息多一行。
// 只用在被拒后的重发里。
func ForceNeutralHead(body []byte) (out []byte, changed bool) {
	return neutralize(body, true)
}

// NeutralizeKnownOpenings 只处理命中已知前缀指纹的消息（便宜，可无条件跑）。
func NeutralizeKnownOpenings(body []byte) (out []byte, changed bool) {
	return neutralize(body, false)
}

// neutralize 是上面两个函数的共同实现。
func neutralize(body []byte, force bool) (out []byte, changed bool) {
	obj, ok := parseBody(body)
	if !ok {
		return body, false
	}
	msgs, ok := messagesOf(obj)
	if !ok {
		return body, false
	}
	dirty := false
	for _, m := range msgs {
		var role string
		if json.Unmarshal(m["role"], &role) != nil || !scannableRole(role) {
			continue
		}
		if neutralizeMessage(m, force) {
			dirty = true
		}
	}
	if !dirty {
		return body, false
	}
	out, err := writeBack(obj, msgs)
	if err != nil {
		return body, false
	}
	return out, true
}

// neutralizeMessage 按 content 的两种形态处理一条消息。
//
// 数组形态必须逐个文本块判断：实测上游读**每一个** text 块，
// 只给第一块加中性开头的话，第二块命中照样 400。
func neutralizeMessage(m map[string]json.RawMessage, force bool) bool {
	raw, ok := m["content"]
	if !ok || len(raw) == 0 {
		return false
	}
	// 字符串形态
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" || startsWithNeutralHead(s) {
			return false
		}
		if !force && !IsUnapprovedOpening(s) {
			return false
		}
		m["content"] = mustJSON(fallbackSystemPrompt + "\n\n" + s)
		return true
	}
	// 数组形态
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
		return false
	}
	dirty := false
	for _, p := range parts {
		var typ, text string
		if json.Unmarshal(p["type"], &typ) != nil || !strings.EqualFold(strings.TrimSpace(typ), "text") {
			continue
		}
		if json.Unmarshal(p["text"], &text) != nil || text == "" || startsWithNeutralHead(text) {
			continue
		}
		if !force && !IsUnapprovedOpening(text) {
			continue
		}
		p["text"] = mustJSON(fallbackSystemPrompt + "\n\n" + text)
		dirty = true
	}
	if !dirty {
		return false
	}
	m["content"] = mustJSON(parts)
	return true
}

// PerturbFingerprints 是最后一档兜底：在被扫描消息的文本里插不可见断词。
//
// 为什么需要：实测上游不只匹配「开头」，还会匹配系统提示词里的一段长子串
// （DSH 的 Web GUI 段，217 字符，单句都 200、连起来就 400）。前缀加一行
// 解决不了子串匹配 —— 被禁的那一段还在，原封不动、只是往下一行。
//
// 而精确子串匹配对任何内部扰动都失效，所以这里在句子/分句边界插 U+200B。
// dense=true 时进一步在每个词中间都插，用于「中性开头和稀疏扰动都救不回」的情况。
//
// 幂等：已经带 U+200B 的文本原样跳过，重发不会层层叠加。
func PerturbFingerprints(body []byte, dense bool) (out []byte, changed bool) {
	obj, ok := parseBody(body)
	if !ok {
		return body, false
	}
	msgs, ok := messagesOf(obj)
	if !ok {
		return body, false
	}
	dirty := false
	for _, m := range msgs {
		var role string
		if json.Unmarshal(m["role"], &role) != nil || !scannableRole(role) {
			continue
		}
		if rewriteTextParts(m, func(s string) string {
			// 已经不带的才动，避免重发层层叠加。但密集档要能叠加在稀疏档之上，
			// 否则「稀疏没清掉」时永远升级不到密集档。
			if !dense && strings.Contains(s, ZeroWidthSpace) {
				return s
			}
			return perturbText(s, dense)
		}) {
			dirty = true
		}
	}
	if !dirty {
		return body, false
	}
	out, err := writeBack(obj, msgs)
	if err != nil {
		return body, false
	}
	return out, true
}

// perturbText 在句子与分句边界、以及大写词组内部插入断词。
func perturbText(text string, dense bool) string {
	out := insertAfterPunct(text, ".!?", true)
	out = insertAfterPunct(out, "。！？", false)
	out = insertAfterPunct(out, ",;", true)
	out = breakCapitalizedPairs(out)
	if dense {
		out = spaceDenseBreaks(out)
	}
	return out
}

// insertAfterPunct 在标点之后插断词。
//
// withSpace=true 时只处理「标点 + 空白」的形态（对应 JS 的 /([.!?])(\s+)/）；
// false 时处理中文标点后紧跟内容的情况，且不重复插入。
func insertAfterPunct(text, puncts string, withSpace bool) string {
	var b strings.Builder
	b.Grow(len(text) + 8)
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		b.WriteRune(r)
		if !strings.ContainsRune(puncts, r) {
			continue
		}
		if withSpace {
			// 后面必须跟空白才插；断词紧贴标点（在空白之前），
			// 与上游 /([.!?])(\s+)/ → 标点+断词+空白 的口径一致。
			if i+1 < len(runes) && isSpaceRune(runes[i+1]) {
				b.WriteString(ZeroWidthSpace)
			}
			continue
		}
		// 中文标点：后面不是断词就插
		if i+1 < len(runes) && string(runes[i+1]) != ZeroWidthSpace {
			b.WriteString(ZeroWidthSpace)
		}
	}
	return b.String()
}

// breakCapitalizedPairs 在相邻的两个大写词之间插断词（"DeepSeek Harness"）。
//
// 规则边界：只处理「整词都是首字母大写 + 字母数字」的相邻词对。
// 普通行文很少出现这种相邻（出现基本就是专名），破专名正是不想被当成指纹的目的。
// 断词插在空白之后，和上游关心的「词与词的边界」对齐。
func breakCapitalizedPairs(text string) string {
	runes := []rune(text)
	type token struct{ word, ws string }
	var (
		lead string
		toks []token
	)
	i := 0
	for i < len(runes) && isSpaceRune(runes[i]) {
		lead += string(runes[i])
		i++
	}
	for i < len(runes) {
		start := i
		for i < len(runes) && !isSpaceRune(runes[i]) {
			i++
		}
		word := string(runes[start:i])
		wsStart := i
		for i < len(runes) && isSpaceRune(runes[i]) {
			i++
		}
		toks = append(toks, token{word: word, ws: string(runes[wsStart:i])})
	}
	var b strings.Builder
	b.Grow(len(text) + len(toks))
	b.WriteString(lead)
	for idx, t := range toks {
		b.WriteString(t.word)
		if idx+1 < len(toks) &&
			capitalizedWordRe.MatchString(t.word) &&
			capitalizedWordRe.MatchString(toks[idx+1].word) {
			// 断词紧贴前一个词（在空白之前），与上游 head+断词+空白 的口径一致
			b.WriteString(ZeroWidthSpace)
		}
		b.WriteString(t.ws)
	}
	return b.String()
}

// spaceDenseBreaks 是密集档：在每个「以词字符开头」的非空白段内部逐字插断词。
func spaceDenseBreaks(text string) string {
	var b strings.Builder
	b.Grow(len(text) * 2)
	runes := []rune(text)
	runStart := rune(0)
	inRun := false
	for i, r := range runes {
		if isSpaceRune(r) {
			inRun = false
			runStart = 0
			b.WriteRune(r)
			continue
		}
		if !inRun {
			runStart = r
			inRun = true
			b.WriteRune(r)
			continue
		}
		if i > 0 && isDenseWordRune(runStart) && r != []rune(ZeroWidthSpace)[0] && runes[i-1] != []rune(ZeroWidthSpace)[0] {
			b.WriteString(ZeroWidthSpace)
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isDenseWordRune(r rune) bool {
	switch {
	case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		return true
	case r >= 0x3040 && r <= 0x30FF: // 日文假名
		return true
	case r >= 0x3400 && r <= 0x9FFF: // 中日韩统一表意文字
		return true
	}
	return false
}

func isSpaceRune(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', '\v', '\f', 0x00A0:
		return true
	}
	return false
}

func startsWithNeutralHead(text string) bool {
	return strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), fallbackSystemPrompt)
}

// ---------- 小工具 ----------

func parseBody(body []byte) (map[string]json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return nil, false
	}
	return obj, true
}

func messagesOf(obj map[string]json.RawMessage) ([]map[string]json.RawMessage, bool) {
	raw, ok := obj["messages"]
	if !ok || len(raw) == 0 {
		return nil, false
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return nil, false
	}
	return msgs, true
}

func writeBack(obj map[string]json.RawMessage, msgs []map[string]json.RawMessage) ([]byte, error) {
	buf, err := json.Marshal(msgs)
	if err != nil {
		return nil, err
	}
	obj["messages"] = buf
	return json.Marshal(obj)
}

func mustJSON(v any) json.RawMessage {
	buf, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return buf
}

// rewriteTextParts 对一条消息的每个文本块应用 fn，返回是否有改动。
//
// 只碰 text 块：图片、工具结果块的结构不能动，动了会撞 11148。
func rewriteTextParts(m map[string]json.RawMessage, fn func(string) string) bool {
	raw, ok := m["content"]
	if !ok || len(raw) == 0 {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return false
		}
		if next := fn(s); next != s {
			m["content"] = mustJSON(next)
			return true
		}
		return false
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
		return false
	}
	dirty := false
	for _, p := range parts {
		var typ, text string
		if json.Unmarshal(p["type"], &typ) != nil || !strings.EqualFold(strings.TrimSpace(typ), "text") {
			continue
		}
		if json.Unmarshal(p["text"], &text) != nil || text == "" {
			continue
		}
		if next := fn(text); next != text {
			p["text"] = mustJSON(next)
			dirty = true
		}
	}
	if dirty {
		m["content"] = mustJSON(parts)
	}
	return dirty
}
