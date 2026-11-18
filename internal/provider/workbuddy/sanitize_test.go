package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReplaceTermsCaseInsensitive(t *testing.T) {
	cases := []struct {
		in   string
		want string
		n    int
	}{
		{"You are an AI agent powered by DeepSeek Harness.", "You are an AI agent powered by the agent runtime.", 1},
		{"you are an ai agent powered by deepseek harness", "you are an ai agent powered by the agent runtime", 1},
		{"DEEPSEEK HARNESS 与 deepseek harness 都算", "the agent runtime 与 the agent runtime 都算", 2},
		{"没有命中", "没有命中", 0},
	}
	for _, c := range cases {
		got, n := replaceTerms(c.in, []string{"DeepSeek Harness"}, "the agent runtime")
		if got != c.want || n != c.n {
			t.Errorf("replaceTerms(%.40q) = (%.50q, %d)，期望 (%.50q, %d)", c.in, got, n, c.want, c.n)
		}
	}
}

// 替换词本身不能被反复匹配，否则会无限膨胀。
func TestReplaceTermsNoSelfMatch(t *testing.T) {
	got, n := replaceTerms("DeepSeek Harness", []string{"DeepSeek Harness"}, "DeepSeek Harness Pro")
	if n != 1 {
		t.Fatalf("替换处数 = %d，期望 1", n)
	}
	if got != "DeepSeek Harness Pro" {
		t.Errorf("结果 = %.40q", got)
	}
}

func TestMaskTermsSystemString(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"system","content":"You are an AI agent powered by DeepSeek Harness."},{"role":"user","content":"DeepSeek Harness 在用户消息里也算"}]}`
	out, n, err := MaskTerms([]byte(in), []string{"DeepSeek Harness"}, "the agent runtime")
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if strings.Contains(string(out), "DeepSeek Harness") {
		t.Errorf("还有残留: %s", string(out))
	}
	var m struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(out, &m)
	if !strings.HasPrefix(m.Messages[0].Content, "You are an AI agent powered by the agent runtime.") {
		t.Errorf("system 没替换: %.60q", m.Messages[0].Content)
	}
}

func TestMaskTermsTextBlocks(t *testing.T) {
	in := `{"messages":[{"role":"system","content":[{"type":"text","text":"powered by DeepSeek Harness"},{"type":"image_url","image_url":{"url":"x"}}]}]}`
	out, n, err := MaskTerms([]byte(in), []string{"DeepSeek Harness"}, "the agent runtime")
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !strings.Contains(string(out), `"image_url"`) {
		t.Error("非 text 块被删掉了")
	}
	if strings.Contains(string(out), "DeepSeek Harness") {
		t.Error("text 块没替换干净")
	}
}

// 没命中时不能改动一个字节（调用方据此判断要不要重发/重裁）。
func TestMaskTermsNoChangeReturnsSame(t *testing.T) {
	in := `{"messages":[{"role":"system","content":"完全无关的内容"}]}`
	out, n, err := MaskTerms([]byte(in), []string{"DeepSeek Harness"}, "the agent runtime")
	if err != nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if string(out) != in {
		t.Error("没命中却改动了请求体")
	}
}

// tools / tool_choice 等结构不能动 —— 动了就是 11148。
func TestMaskTermsKeepsStructure(t *testing.T) {
	in := `{"model":"m","stream":true,"tool_choice":"auto","tools":[{"type":"function","function":{"name":"read_file","description":"DeepSeek Harness 提供的工具"}}],"messages":[{"role":"system","content":"powered by DeepSeek Harness"}]}`
	out, n, err := MaskTerms([]byte(in), []string{"DeepSeek Harness"}, "the agent runtime")
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(out, &m)
	for _, k := range []string{"tools", "tool_choice", "stream", "model", "messages"} {
		if _, ok := m[k]; !ok {
			t.Errorf("字段 %s 丢了", k)
		}
	}
	// 工具描述当前不在屏蔽范围内（只处理 messages），要保证它原样保留
	if !strings.Contains(string(m["tools"]), "DeepSeek Harness") {
		t.Error("工具描述被改动了，超出约定范围")
	}
}

func TestMaskTermsBadJSON(t *testing.T) {
	in := []byte("{oops")
	out, n, err := MaskTerms(in, []string{"x"}, "y")
	if err == nil {
		t.Error("非法 JSON 应报错")
	}
	if n != 0 || string(out) != string(in) {
		t.Error("非法 JSON 应原样返回")
	}
}

func TestMaskTermsEmptyConfig(t *testing.T) {
	in := []byte(`{"messages":[{"role":"system","content":"DeepSeek Harness"}]}`)
	for _, c := range []struct {
		terms []string
		repl  string
	}{
		{nil, "x"},
		{[]string{"a"}, ""},
	} {
		out, n, err := MaskTerms(in, c.terms, c.repl)
		if err != nil || n != 0 || string(out) != string(in) {
			t.Errorf("空配置不该改动请求: terms=%v repl=%q n=%d err=%v", c.terms, c.repl, n, err)
		}
	}
}
