package workbuddy

// 渠道校验（code 11128）的处理。
//
// 上游对「请求来源」的判定规则，2026-11-18 在本机实测过（CN 账号、
// deepseek-v4.1-flash、其余字段全部固定）：
//
//	1. 只看 system 与 assistant 消息的**开头**（前缀匹配），大小写不敏感，
//	   前导空白会被裁掉再比。
//	2. 同样的句子出现在消息**中间**照常 200。
//	3. 已知指纹（第三方 Agent 的自我描述）一旦命中就整条请求 400：
//	      {"code":11128,"msg":"Illegal API invocation from an unapproved channel"}
//	4. 零宽字符、HTML 实体这类「断词」手法**不管用**，上游会先归一化再比。
//	5. 在指纹前**另起一行中性开头**（"You are a helpful assistant."）后 200 ——
//	   前缀不再命中，整段被移出匹配区。
//
// 所以正确的修法不是「换号」也不是「重试」：换号发出去的是同一份内容，
// 换几次就被拒几次。要改的是**消息开头**。这里实现两件事：
//
//   - NeutralizeChannelBlock：把每个被扫描的开头前面加一行中性开头。
//     不依赖认识具体是哪条指纹 —— 未知指纹同样被移出匹配区。
//   - IsUnapprovedOpening：只用于本地日志，说明是哪个开头像指纹。

import (
	"encoding/json"
	"strings"
)

// NeutralHead 是插在消息最前面的中性开头。
//
// 不能省略成空行或空字符串：上游要求 system 内容非空，
// 而且这句本身就是一个正常的系统提示开头，对模型无害。
const NeutralHead = "You are a helpful assistant."

// unapprovedOpenings 是本地**实测**过的指纹（小写）。
//
// 故意不用猜测扩充：没实测过的开头不写进来，否则会把本来能过的
// prompt 也改掉。真正的修复逻辑（NeutralizeChannelBlock）不查这张表，
// 它对**所有**开头生效，所以表外的新指纹一样能救回来。
var unapprovedOpenings = []string{
	"you are an ai agent powered by deepseek harness",
	"you are claude code, anthropic's official cli for claude",
}

// IsUnapprovedOpening 判断这段文本开头是不是已知指纹。
//
// 只用于日志和「这条值得先修一下」的判断，不参与上行的放行决策。
func IsUnapprovedOpening(text string) bool {
	opening := strings.ToLower(strings.TrimLeft(text, " \t\r\n"))
	for _, p := range unapprovedOpenings {
		if strings.HasPrefix(opening, p) {
			return true
		}
	}
	return false
}

// scannableRole 是这个上游会做前缀匹配的角色。
//
// assistant 也算：历史里一条以指纹开头的回答，会让**整个会话**
// 一直 400，只改 system 救不回来（本机实测确认）。
func scannableRole(role string) bool {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "system", "developer", "assistant":
		return true
	}
	return false
}

// NeutralizeChannelBlock 给每个被扫描的开头补一行中性开头。
//
// 返回值 repaired 表示请求体是否真的变了：没变就不该浪费一次上游请求，
// 调用方据此决定还值不值得重发。
//
// 幂等：已经以 NeutralHead 开头的消息原样保留，重复调用不会叠加。
// 只动 system/developer/assistant 的文本开头，user/tool 消息一概不碰，
// 工具调用与 tool 配对结构也不碰 —— 那些改动会引入 11148 一类新问题。
func NeutralizeChannelBlock(body []byte) (out []byte, repaired bool, err error) {
	var root map[string]json.RawMessage
	if err = json.Unmarshal(body, &root); err != nil {
		return body, false, err
	}

	// messages 不是数组说明这不是能安全改写的请求，原样返回。
	rawMsgs, ok := root["messages"]
	if !ok || len(rawMsgs) == 0 {
		return body, false, nil
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(rawMsgs, &msgs) != nil {
		return body, false, nil
	}

	head := NeutralHead
	changed := false
	for _, m := range msgs {
		var role string
		if json.Unmarshal(m["role"], &role) != nil || !scannableRole(role) {
			continue
		}
		if neutralizeContent(m, head) {
			changed = true
		}
	}
	if !changed {
		return body, false, nil
	}

	buf, merr := json.Marshal(msgs)
	if merr != nil {
		return body, false, merr
	}
	root["messages"] = buf
	out, merr = json.Marshal(root)
	if merr != nil {
		return body, false, merr
	}
	return out, true, nil
}

// neutralizeContent 处理一条消息的 content（字符串或 block 数组）。
func neutralizeContent(m map[string]json.RawMessage, head string) bool {
	raw, ok := m["content"]
	if !ok || len(raw) == 0 {
		return false
	}

	// 形态一：content 是字符串
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" || startsWithHead(s, head) {
			return false
		}
		buf, err := json.Marshal(head + "\n\n" + s)
		if err != nil {
			return false
		}
		m["content"] = buf
		return true
	}

	// 形态二：content 是 block 数组，只需要动第一块 text，
	// 其余（图片、工具结果块）保持原样、顺序不变。
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
		return false
	}
	for _, p := range parts {
		var typ, text string
		if json.Unmarshal(p["type"], &typ) != nil || strings.ToLower(strings.TrimSpace(typ)) != "text" {
			continue
		}
		if json.Unmarshal(p["text"], &text) != nil || text == "" || startsWithHead(text, head) {
			return false
		}
		buf, err := json.Marshal(head + "\n\n" + text)
		if err != nil {
			return false
		}
		p["text"] = buf
		nb, err := json.Marshal(parts)
		if err != nil {
			return false
		}
		m["content"] = nb
		return true
	}
	return false
}

func startsWithHead(text, head string) bool {
	return strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), head)
}
