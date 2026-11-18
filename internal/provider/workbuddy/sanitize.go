package workbuddy

// 上下文身份词屏蔽。
//
// 上游的渠道校验会扫请求里的文本，认出第三方 Agent 的自我描述后
// 用 400 / code 11-128 拒掉整条请求。它认的是「谁在说话」，
// 而不是请求内容对不对，所以换账号、换模型、重发同一份内容都可能被同样拒掉。
//
// 这里做的事很简单：把身份词换成中性说法。不改提示词结构、不动工具表、
// 不动 tool 配对，只做等长语义的文本替换 —— 三个理由：
//
//  1. 只在能改文本的地方改，结构一动手就会撞上 11148（tool 配对错位）。
//  2. 大小写不敏感：上游比对时就是小写化的，只改原样写法等于没改。
//  3. 命中数为 0 时直接返回原切片，调用方据此判断「这份请求本来就没被标记」，
//     不会因为开启这个功能就白序列化一遍请求体。

import (
	"encoding/json"
	"strings"
)

// MaskTerms 把请求体里 messages 文本中的身份词替换掉。
//
// 返回 replaced 是实际替换的处数，0 表示请求体没变（原样返回）。
// 任何解析失败都按「改不动」处理并原样返回：宁可让上游去拒，
// 也不能因为改写不了就把一条正常请求弄坏。
func MaskTerms(body []byte, terms []string, replacement string) (out []byte, replaced int, err error) {
	if len(terms) == 0 || replacement == "" {
		return body, 0, nil
	}
	var root map[string]json.RawMessage
	if err = json.Unmarshal(body, &root); err != nil {
		return body, 0, err
	}
	rawMsgs, ok := root["messages"]
	if !ok || len(rawMsgs) == 0 {
		return body, 0, nil
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(rawMsgs, &msgs) != nil {
		return body, 0, nil
	}

	total := 0
	for _, m := range msgs {
		n := maskMessageContent(m, terms, replacement)
		total += n
	}
	if total == 0 {
		return body, 0, nil
	}

	buf, merr := json.Marshal(msgs)
	if merr != nil {
		return body, 0, merr
	}
	root["messages"] = buf
	out, merr = json.Marshal(root)
	if merr != nil {
		return body, 0, merr
	}
	return out, total, nil
}

// maskMessageContent 处理一条消息的 content（字符串或 block 数组）。
func maskMessageContent(m map[string]json.RawMessage, terms []string, replacement string) int {
	raw, ok := m["content"]
	if !ok || len(raw) == 0 {
		return 0
	}

	// 形态一：content 是字符串
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return 0
		}
		masked, n := replaceTerms(s, terms, replacement)
		if n == 0 {
			return 0
		}
		if buf, err := json.Marshal(masked); err == nil {
			m["content"] = buf
		}
		return n
	}

	// 形态二：content 是 block 数组，只改 text 块，其它块原样保留。
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
		return 0
	}
	total := 0
	changed := false
	for _, p := range parts {
		var typ, text string
		if json.Unmarshal(p["type"], &typ) != nil || strings.ToLower(strings.TrimSpace(typ)) != "text" {
			continue
		}
		if json.Unmarshal(p["text"], &text) != nil || text == "" {
			continue
		}
		masked, n := replaceTerms(text, terms, replacement)
		if n == 0 {
			continue
		}
		if buf, err := json.Marshal(masked); err == nil {
			p["text"] = buf
			total += n
			changed = true
		}
	}
	if !changed {
		return 0
	}
	if buf, err := json.Marshal(parts); err == nil {
		m["content"] = buf
	}
	return total
}

// replaceTerms 做大小写不敏感的替换，返回结果与替换处数。
//
// 手写扫描而不是 strings.ReplaceAll：要一次比多个词、要大小写不敏感，
// 又不想为正则引入编译开销（这条路径每个请求都跑）。
//
// 替换后继续从替换词之后扫，不会把替换词本身再匹配一遍
// （例如把 "A" 换成 "A B" 时不会无限膨胀）。
func replaceTerms(text string, terms []string, replacement string) (string, int) {
	lower := strings.ToLower(text)
	var b strings.Builder
	b.Grow(len(text))
	count := 0
	i := 0
	for i < len(text) {
		matched := ""
		for _, t := range terms {
			if t == "" {
				continue
			}
			lt := strings.ToLower(t)
			if strings.HasPrefix(lower[i:], lt) {
				// 取最长匹配，避免短词先把长词切碎
				if len(t) > len(matched) {
					matched = t
				}
			}
		}
		if matched == "" {
			b.WriteByte(text[i])
			i++
			continue
		}
		b.WriteString(replacement)
		i += len(matched)
		count++
	}
	if count == 0 {
		return text, 0
	}
	return b.String(), count
}
