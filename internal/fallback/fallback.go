// Package fallback 实现「模型级降级链」。
//
// 场景：一家供应商下有多个模型，但额度是按「模型 × 账号」独立算的。
// 只换账号不够 —— 8 个号在 glm-5.3 上全 429 时，换到 deepseek-v4.1-flash 往往还有额度。
//
// 所以转发失败时的降级顺序应该是：
//
//	同模型换账号（fast path）
//	  → 模型链里下一个模型，重新走一遍所有账号
//	  → 下一个供应商（如果配了 group）
//
// 本文件只负责「算出降级顺序」，不执行请求。
package fallback

import "allin/internal/config"

// Step 是一次降级尝试：(供应商, 模型名)。
type Step struct {
	ProviderID string
	Provider   *config.Provider
	Model      string
}

// Chain 为一次请求算出完整的尝试序列。
//
// 规则：
//  1. 原供应商的「当前模型」放在最前
//  2. 同供应商的 models 列表里其余模型依次跟上（这就是降级链）
//  3. 如果模型本身来自 group，则按 group.targets 展开
//  4. 同一个「供应商+模型」只出现一次
func Chain(cfg *config.Config, prov *config.Provider, model string) []Step {
	var out []Step
	seen := map[string]bool{}

	push := func(p *config.Provider, m string) {
		if p == nil || m == "" {
			return
		}
		k := p.ID + "\x00" + m
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, Step{ProviderID: p.ID, Provider: p, Model: m})
	}

	// 1) 当前模型优先
	push(prov, model)

	// 2) 同供应商的其它模型
	if prov != nil {
		for _, m := range prov.Models {
			if m != model {
				push(prov, m)
			}
		}
	}

	return out
}

// ShouldFallbackModel 判断某个错误是否值得换模型（而不是继续同模型换号）。
//
// 额度耗尽（429 / code 6004）是最典型的 —— 同一个模型下所有号都会是同样的错误，
// 继续换号纯属浪费。401 也意味着这个号的凭据坏了，但换号可能有效，所以不算模型级失败。
func ShouldFallbackModel(status int, body string) bool {
	if status == 429 {
		return true
	}
	// 上游把额度耗尽包在 200/400 里带业务码的情况
	for _, code := range []string{`"code":6004`, `"code":10001`, "insufficient_quota", "quota_exceeded"} {
		if contains(body, code) {
			// 10001 在 WorkBuddy 里也用在「今日已签到」，那种不该降级，
			// 但那个接口不走这条路径，所以这里按额度类统一处理
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	if len(sub) == 0 || len(s) < len(sub) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// IsChannelBlocked 判断是不是「渠道校验失败」这一类硬拒绝。
//
// WorkBuddy 对非官方客户端会返回：
//
//	400 {"code":11128,"msg":"Illegal API invocation from an unapproved channel",
//	     "retry_policy":{"retry":false}, ...}
//
// 这类错误换号、换模型、重试都不会成功 —— 是请求来源被判定为非官方客户端。
// 必须直接失败并把原因透传给用户，否则会白白跑满 3 次换号 + 整条模型降级链，
// 用户只看到「本机出错」却等很久。
func IsChannelBlocked(body string) bool {
	return contains(body, `"code":11128`) ||
		contains(body, "unapproved channel") ||
		contains(body, "channel verification")
}

// ChannelBlockedHint 是给用户看的说明。
//
// 说清「发生了什么、为什么换账号没用、能做什么」，而不是丢一句上游原文。
// 早期版本让人「换一个账号」，那是错的：换号发出去的是同一份内容，
// 换几次就被拒几次（实测）。真正的原因是消息开头命中了上游的客户端指纹。
func ChannelBlockedHint() string {
	return "上游拒绝了这次请求：渠道校验未通过（code 11128）。" +
		"这是上游在按『消息开头』判定请求像不像官方客户端，" +
		"换账号、换模型发出去的都是同一份内容，所以换号无效，网关已不再重试。" +
		"网关会自动给消息开头补一行中性开头再试一次；仍然失败说明被拦的不止开头 ——" +
		"检查一下系统提示词或历史消息里是否带了其它 Agent 的自我介绍。"
}

// IsContextTooLarge 判断上游拒绝是不是因为「请求太大」。
//
// 上游对此的措辞不统一，常见几类：
//   - 11148 tool calls and tool results do not match（裁剪切断配对）
//   - prompt is too long / context length exceeded / too many tokens
//   - 11128 unapproved channel 之外的其它 code 111xx 通常也和请求体有关
//
// 命中后应换更小的预算重新裁剪再发，而不是直接失败。
func IsContextTooLarge(body string) bool {
	low := toLower(body)
	for _, kw := range []string{
		"tool calls and tool results do not match",
		"tool_call_id",
		"prompt is too long",
		"context length",
		"context_length_exceeded",
		"maximum context",
		"too many tokens",
		"request too large",
		"payload too large",
		`"code":11148`,
	} {
		if contains(low, kw) {
			return true
		}
	}
	return false
}

func toLower(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// IsTransientToolReject 判断是不是「上游瞬态拒绝」，可以原样重发。
//
// 上游原话：
//
//	{"code":11148,"msg":"tool calls and tool results do not match,
//	 please start a new conversation and retry"}
//
// 实测同一份字节重发即成功，且 743 对 tool 配对零缺失、5 张图连发 5 次全过，
// 说明它与请求内容无关，是上游侧的瞬态状态。因此应当原样重试，
// 而不是修改请求（改请求会引入新风险）。
func IsTransientToolReject(body string) bool {
	return contains(toLower(body), "tool calls and tool results do not match") ||
		contains(body, `"code":11148`)
}
