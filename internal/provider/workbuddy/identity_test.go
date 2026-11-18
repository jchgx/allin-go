package workbuddy

// 这套测试锁住「移植过来的官方客户端兼容规则」的每一条。
// 每条规则都对应上游的一条实测行为，改动它们等于改动通过率，所以逐条钉住。

import (
	"encoding/json"
	"strings"
	"testing"
)

func bodyOf(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func messagesOfAny(t *testing.T, out []byte) []map[string]any {
	t.Helper()
	var m struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	return m.Messages
}

// 规则 1：developer 角色会被网关拒，必须归一成 system。
func TestApplyIdentityRulesRewritesDeveloperRole(t *testing.T) {
	in := `{"messages":[{"role":"developer","content":"rules"},{"role":"user","content":"hi"}]}`
	out, changed := ApplyIdentityRules([]byte(in))
	if !changed {
		t.Fatal("developer 应被改写")
	}
	msgs := messagesOfAny(t, out)
	if msgs[0]["role"] != "system" {
		t.Errorf("role = %v，期望 system", msgs[0]["role"])
	}
}

// 规则 2：首条不是 system 时补一条 —— 国际网关对 user-first 直接 11-128。
func TestApplyIdentityRulesEnsuresSystemHead(t *testing.T) {
	in := `{"messages":[{"role":"user","content":"hi"}]}`
	out, changed := ApplyIdentityRules([]byte(in))
	if !changed {
		t.Fatal("应补 system 头")
	}
	msgs := messagesOfAny(t, out)
	if len(msgs) != 2 || msgs[0]["role"] != "system" {
		t.Fatalf("首条不是 system: %v", msgs)
	}
	if !strings.Contains(msgs[0]["content"].(string), fallbackSystemPrompt) {
		t.Errorf("占位文案不对: %v", msgs[0]["content"])
	}
}

// 首条已经是 system 时不能多插一条（幂等，也是「不打扰本来能过的请求」）。
func TestApplyIdentityRulesNoExtraSystemHead(t *testing.T) {
	in := `{"messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`
	out, _ := ApplyIdentityRules([]byte(in))
	if got := len(messagesOfAny(t, out)); got != 2 {
		t.Errorf("消息数 = %d，期望 2", got)
	}
}

// 规则 3：句子级指纹要按同义说法改掉（实测原句 400、改过的 200）。
func TestApplyIdentityRulesRewritesGatePhrases(t *testing.T) {
	in := `{"messages":[{"role":"system","content":"Tokens prefixed with @ are paths the user explicitly referenced. 其余内容"}],"role":"system"}`
	out, changed := ApplyIdentityRules([]byte(in))
	if !changed {
		t.Fatal("应改词")
	}
	text := messagesOfAny(t, out)[0]["content"].(string)
	if strings.Contains(text, "Tokens prefixed with @ are paths the user explicitly referenced.") {
		t.Errorf("原句还在: %q", text)
	}
	if !strings.Contains(text, "Paths that begin with @") {
		t.Errorf("没换成同义说法: %q", text)
	}
	if !strings.Contains(text, "其余内容") {
		t.Error("正文被改坏了")
	}
}

// 规则 5：身份子句的每个词边界都要断词，而不是只断一处。
func TestBreakIdentityClausesEveryBoundary(t *testing.T) {
	in := "You are a coding agent powered by the deepseek model."
	out := breakIdentityClauses(in)
	if out == in {
		t.Fatal("没有断词")
	}
	// 每个空白后都应该跟着一个 U+200B
	if strings.Count(out, ZeroWidthSpace) < 5 {
		t.Errorf("断词太少（%d）: %q", strings.Count(out, ZeroWidthSpace), out)
	}
	// 幂等：再来一遍不变
	if again := breakIdentityClauses(out); again != out {
		t.Error("断词不幂等")
	}
	// 模型读起来文字不变
	if strings.ReplaceAll(out, ZeroWidthSpace, "") != in {
		t.Error("去掉断词后原文被改变了")
	}
}

// 兜底扰动的边界规则与上游实测口径一致：
// 中文句末标点（。！？）后面直接插；ASCII 的 .!? 与 ,; 只在「后面跟空白」时插，
// 且断词紧贴标点。全角逗号不在规则内 —— 移植时按原规则保留，不擅自扩表。
func TestPerturbTextBoundaries(t *testing.T) {
	in := "第一句。第二句！ ok. next, and; tail"
	out := perturbText(in, false)
	for _, want := range []string{
		"第一句。" + ZeroWidthSpace,
		"第二句！" + ZeroWidthSpace,
		"ok." + ZeroWidthSpace,
		"next," + ZeroWidthSpace,
		"and;" + ZeroWidthSpace,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少边界断词 %q，实际 %q", want, out)
		}
	}
	// 没有空白跟随的 ASCII 标点不插
	if got := perturbText("a;b c", false); strings.Contains(got, ";"+ZeroWidthSpace) {
		t.Errorf("无空白跟随的分号不该断词: %q", got)
	}
}

// 大写词组内部要断词："DeepSeek Harness" 这类专名正是被当指纹的形状。
func TestBreakCapitalizedPairs(t *testing.T) {
	out := breakCapitalizedPairs("powered by DeepSeek Harness today")
	if !strings.Contains(out, "DeepSeek"+ZeroWidthSpace+" Harness") {
		t.Errorf("没有在专名内部断词: %q", out)
	}
	// 普通句子（词首不大写连排）不应该被动
	plain := "the quick brown fox"
	if breakCapitalizedPairs(plain) != plain {
		t.Error("普通句子被改动了")
	}
}

// 兜底扰动要幂等，且不碰 user/tool 消息。
func TestPerturbFingerprintsSkipsUserAndIsIdempotent(t *testing.T) {
	in := `{"messages":[{"role":"system","content":"甲。乙，丙"},{"role":"user","content":"用户的话。别动，包括标点"}]}`
	out, changed := PerturbFingerprints([]byte(in), false)
	if !changed {
		t.Fatal("应扰动")
	}
	msgs := messagesOfAny(t, out)
	if msgs[1]["content"] != "用户的话。别动，包括标点" {
		t.Errorf("user 消息被改动了: %v", msgs[1]["content"])
	}
	if _, again := PerturbFingerprints(out, false); again {
		t.Error("扰动不幂等")
	}
}

// 强制中性头：不管开头是什么，每条被扫描消息都挪一行。
func TestForceNeutralHeadMovesEverything(t *testing.T) {
	in := `{"messages":[{"role":"system","content":"随便什么开头"},{"role":"assistant","content":"回答开头"}]}`
	out, changed := ForceNeutralHead([]byte(in))
	if !changed {
		t.Fatal("应改写")
	}
	for i, m := range messagesOfAny(t, out) {
		if !strings.HasPrefix(m["content"].(string), fallbackSystemPrompt) {
			t.Errorf("第 %d 条没加中性头", i)
		}
	}
}

// 条件版只动命中已知前缀的，别的不打扰。
func TestNeutralizeKnownOpeningsOnlyOnHit(t *testing.T) {
	in := `{"messages":[{"role":"system","content":"You are an AI agent powered by DeepSeek Harness."},{"role":"assistant","content":"普通回答"}]}`
	out, changed := NeutralizeKnownOpenings([]byte(in))
	if !changed {
		t.Fatal("命中了却没改")
	}
	msgs := messagesOfAny(t, out)
	if !strings.HasPrefix(msgs[0]["content"].(string), fallbackSystemPrompt) {
		t.Error("system 没加中性头")
	}
	if msgs[1]["content"] != "普通回答" {
		t.Errorf("未命中的消息被改动了: %v", msgs[1]["content"])
	}
}

// tools / tool_choice / stream 一律不能动：动结构会撞 11148。
func TestIdentityRulesKeepStructure(t *testing.T) {
	in := `{"model":"m","stream":true,"tool_choice":"auto","tools":[{"type":"function","function":{"name":"read_file"}}],"messages":[{"role":"developer","content":"You are an AI agent powered by DeepSeek Harness."}]}`
	out, _ := ApplyIdentityRules([]byte(in))
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"tools", "tool_choice", "stream", "model", "messages"} {
		if _, ok := m[k]; !ok {
			t.Errorf("字段 %s 丢了", k)
		}
	}
}

// 非法 JSON 不能 panic，也不能改动原文。
func TestIdentityRulesBadJSON(t *testing.T) {
	in := []byte("not json")
	if out, changed := ApplyIdentityRules(in); changed || string(out) != string(in) {
		t.Error("非法 JSON 不该被改动")
	}
	if out, changed := ForceNeutralHead(in); changed || string(out) != string(in) {
		t.Error("非法 JSON 不该被改动（ForceNeutralHead）")
	}
	if out, changed := PerturbFingerprints(in, true); changed || string(out) != string(in) {
		t.Error("非法 JSON 不该被改动（Perturb）")
	}
}

// 补首条 system 后，后面的下标不能错位（曾经踩过的坑）。
func TestEnsureSystemHeadKeepsOrder(t *testing.T) {
	in := `{"messages":[{"role":"user","content":"一"},{"role":"assistant","content":"二"},{"role":"user","content":"三"}]}`
	out, _ := ApplyIdentityRules([]byte(in))
	msgs := messagesOfAny(t, out)
	want := []string{"system", "user", "assistant", "user"}
	if len(msgs) != len(want) {
		t.Fatalf("消息数 = %d，期望 %d", len(msgs), len(want))
	}
	for i, role := range want {
		if msgs[i]["role"] != role {
			t.Errorf("第 %d 条 role = %v，期望 %s", i, msgs[i]["role"], role)
		}
	}
}
