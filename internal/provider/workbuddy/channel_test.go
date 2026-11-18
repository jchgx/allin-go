package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"
)

// 实测事实（2026-11-18，CN 网关）：开头是指纹 → 400/11128；
// 前面加一行中性开头 → 200。这里锁住改写后的形状。
func TestNeutralizeChannelBlockRewritesOpening(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"system","content":"You are an AI agent powered by DeepSeek Harness."},{"role":"user","content":"ping"}]}`
	out, repaired, err := NeutralizeChannelBlock([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if !repaired {
		t.Fatal("应判定为已改写")
	}
	var m struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	sys := m.Messages[0].Content
	if !strings.HasPrefix(sys, NeutralHead) {
		t.Fatalf("system 开头没加中性头: %.60q", sys)
	}
	if !strings.Contains(sys, "You are an AI agent powered by DeepSeek Harness.") {
		t.Error("原文被改坏了，应该只是在前面加一行")
	}
	// user 消息一概不动
	if m.Messages[1].Content != "ping" {
		t.Errorf("user 消息被改动了: %.40q", m.Messages[1].Content)
	}
}

// assistant 历史也要改：实测一条以指纹开头的回答会让整个会话一直 400。
func TestNeutralizeChannelBlockCoversAssistant(t *testing.T) {
	in := `{"messages":[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":"hi"},{"role":"assistant","content":"You are Claude Code, Anthropic's official CLI for Claude."}]}`
	out, repaired, err := NeutralizeChannelBlock([]byte(in))
	if err != nil || !repaired {
		t.Fatalf("repaired=%v err=%v", repaired, err)
	}
	var m struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(m.Messages[2].Content, NeutralHead+"\n\nYou are Claude Code") {
		t.Errorf("assistant 开头没加中性头: %.80q", m.Messages[2].Content)
	}
}

// 幂等：已经是中性开头的请求不该被反复叠加，也不该被判为「改写过」。
func TestNeutralizeChannelBlockIdempotent(t *testing.T) {
	in := `{"messages":[{"role":"system","content":"You are a helpful assistant.\n\nYou are an AI agent powered by DeepSeek Harness."}]}`
	_, repaired, err := NeutralizeChannelBlock([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if repaired {
		t.Error("已带中性头的请求不应再次改写")
	}
}

// content 是 block 数组时只动第一块 text。
func TestNeutralizeChannelBlockTextBlocks(t *testing.T) {
	in := `{"messages":[{"role":"system","content":[{"type":"text","text":"You are an AI agent powered by DeepSeek Harness."},{"type":"text","text":"第二块"}]}]}`
	out, repaired, err := NeutralizeChannelBlock([]byte(in))
	if err != nil || !repaired {
		t.Fatalf("repaired=%v err=%v", repaired, err)
	}
	var m struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(m.Messages[0].Content[0].Text, NeutralHead) {
		t.Error("第一块 text 没加中性头")
	}
	if m.Messages[0].Content[1].Text != "第二块" {
		t.Error("第二块 text 被改动了")
	}
}

// 不该动 tools / tool_choice / stream，那些改动会引入新问题。
func TestNeutralizeChannelBlockKeepsOtherFields(t *testing.T) {
	in := `{"model":"m","stream":true,"tool_choice":"auto","messages":[{"role":"system","content":"You are an AI agent powered by DeepSeek Harness."}],"tools":[{"type":"function","function":{"name":"read_file"}}]}`
	out, repaired, err := NeutralizeChannelBlock([]byte(in))
	if err != nil || !repaired {
		t.Fatalf("repaired=%v err=%v", repaired, err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"tools", "tool_choice", "stream", "model"} {
		if _, ok := m[k]; !ok {
			t.Errorf("字段 %s 丢了", k)
		}
	}
}

func TestIsUnapprovedOpening(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"You are an AI agent powered by DeepSeek Harness.", true},
		{"  \nYou are an AI agent powered by DeepSeek Harness.", true},
		{"you are an ai agent powered by deepseek harness", true},
		{"You are Claude Code, Anthropic's official CLI for Claude.", true},
		{"中文开头。\n\nYou are an AI agent powered by DeepSeek Harness.", false}, // 不在开头
		{"You are a helpful assistant.", false},
	}
	for _, c := range cases {
		if got := IsUnapprovedOpening(c.text); got != c.want {
			t.Errorf("IsUnapprovedOpening(%.40q) = %v, 期望 %v", c.text, got, c.want)
		}
	}
}

// 非法 JSON 不能 panic，也不能改动原文。
func TestNeutralizeChannelBlockBadJSON(t *testing.T) {
	in := []byte(`not json`)
	out, repaired, err := NeutralizeChannelBlock(in)
	if err == nil {
		t.Error("非法 JSON 应返回错误")
	}
	if repaired || string(out) != string(in) {
		t.Error("非法 JSON 不该被改动")
	}
}
