package bridge

// 跨协议流式转换的测试。
//
// 这里刻意用「上游真实会发的那几帧」当夹具，而不是构造理想输入：
// 转换器出错时客户端看到的是半截流，比直接报错更难查，所以逐帧钉住输出形状。

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// parseAnthropicEvents 把转换结果的 SSE 拆成 (event, data) 列表。
func parseAnthropicEvents(t *testing.T, raw string) []struct {
	Event string
	Data  map[string]any
} {
	t.Helper()
	var out []struct {
		Event string
		Data  map[string]any
	}
	var cur string
	for _, line := range strings.Split(raw, "\n") {
		if after, ok := strings.CutPrefix(line, "event: "); ok {
			cur = after
			continue
		}
		if after, ok := strings.CutPrefix(line, "data: "); ok {
			var m map[string]any
			if err := json.Unmarshal([]byte(after), &m); err != nil {
				t.Fatalf("data 不是合法 JSON: %q", after)
			}
			out = append(out, struct {
				Event string
				Data  map[string]any
			}{cur, m})
			cur = ""
		}
	}
	return out
}

// OpenAI 上游 → Anthropic 客户端：纯文本流。
func TestStreamOpenAIToAnthropicText(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"cmb-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		``,
		`data: {"id":"cmb-1","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}`,
		``,
		`data: {"id":"cmb-1","choices":[{"index":0,"delta":{"content":"，世界"},"finish_reason":null}]}`,
		``,
		`data: {"id":"cmb-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":5}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var out bytes.Buffer
	in, outTok, err := StreamOpenAIToAnthropic(strings.NewReader(upstream), &out, "deepseek-v4.1-flash", nil)
	if err != nil {
		t.Fatal(err)
	}
	if in != 11 || outTok != 5 {
		t.Errorf("用量 = (%d,%d)，期望 (11,5)", in, outTok)
	}

	events := parseAnthropicEvents(t, out.String())
	if len(events) < 6 {
		t.Fatalf("事件太少: %d\n%s", len(events), out.String())
	}
	if events[0].Event != "message_start" {
		t.Errorf("首帧应为 message_start，实际 %s", events[0].Event)
	}
	// 拼回正文
	var text strings.Builder
	for _, e := range events {
		if e.Event == "content_block_delta" {
			if d, ok := e.Data["delta"].(map[string]any); ok {
				if s, ok := d["text"].(string); ok {
					text.WriteString(s)
				}
			}
		}
	}
	if text.String() != "你好，世界" {
		t.Errorf("正文 = %q，期望 你好，世界", text.String())
	}
	// 结尾必须是 message_delta(带 stop_reason) + message_stop
	last := events[len(events)-1]
	if last.Event != "message_stop" {
		t.Errorf("末帧应为 message_stop，实际 %s", last.Event)
	}
	foundDelta := false
	for _, e := range events {
		if e.Event == "message_delta" {
			foundDelta = true
			d := e.Data["delta"].(map[string]any)
			if d["stop_reason"] != "end_turn" {
				t.Errorf("stop_reason = %v，期望 end_turn", d["stop_reason"])
			}
		}
	}
	if !foundDelta {
		t.Error("缺少 message_delta")
	}
	if strings.Contains(out.String(), "event: message_stop\nevent:") {
		t.Error("事件之间缺少 data 行")
	}
}

// OpenAI 上游的工具调用碎片 → Anthropic 的 tool_use 块。
func TestStreamOpenAIToAnthropicToolUse(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	var out bytes.Buffer
	if _, _, err := StreamOpenAIToAnthropic(strings.NewReader(upstream), &out, "m", nil); err != nil {
		t.Fatal(err)
	}
	events := parseAnthropicEvents(t, out.String())

	var toolStart map[string]any
	var args strings.Builder
	openBlocks, closedBlocks := map[int]bool{}, map[int]bool{}
	for _, e := range events {
		switch e.Event {
		case "content_block_start":
			idx := int(e.Data["index"].(float64))
			openBlocks[idx] = true
			if cb, ok := e.Data["content_block"].(map[string]any); ok && cb["type"] == "tool_use" {
				toolStart = cb
			}
		case "content_block_delta":
			if d, ok := e.Data["delta"].(map[string]any); ok && d["type"] == "input_json_delta" {
				args.WriteString(d["partial_json"].(string))
			}
		case "content_block_stop":
			closedBlocks[int(e.Data["index"].(float64))] = true
		}
	}
	if toolStart == nil {
		t.Fatal("没有 tool_use 块")
	}
	if toolStart["name"] != "read_file" || toolStart["id"] != "call_a" {
		t.Errorf("工具块 = %v", toolStart)
	}
	if args.String() != `{"path":"a.txt"}` {
		t.Errorf("参数拼回 = %q", args.String())
	}
	for idx := range openBlocks {
		if !closedBlocks[idx] {
			t.Errorf("块 %d 没有关闭", idx)
		}
	}
	// 工具结束时 stop_reason 必须是 tool_use
	for _, e := range events {
		if e.Event == "message_delta" {
			if d := e.Data["delta"].(map[string]any); d["stop_reason"] != "tool_use" {
				t.Errorf("stop_reason = %v，期望 tool_use", d["stop_reason"])
			}
		}
	}
}

// Anthropic 上游 → OpenAI 客户端。
func TestStreamAnthropicToOpenAI(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":7}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"嗨"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	var out bytes.Buffer
	in, outTok, err := StreamAnthropicToOpenAI(strings.NewReader(upstream), &out, "m")
	if err != nil {
		t.Fatal(err)
	}
	if in != 7 || outTok != 3 {
		t.Errorf("用量 = (%d,%d)，期望 (7,3)", in, outTok)
	}
	raw := out.String()
	if !strings.HasSuffix(raw, "data: [DONE]\n\n") {
		t.Error("结尾没有 [DONE]")
	}
	var content strings.Builder
	finishSeen := ""
	for _, line := range strings.Split(raw, "\n") {
		after, ok := strings.CutPrefix(line, "data: ")
		if !ok || after == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(after), &chunk); err != nil {
			t.Fatalf("chunk 不合法: %q", after)
		}
		for _, c := range chunk.Choices {
			content.WriteString(c.Delta.Content)
			if c.FinishReason != nil {
				finishSeen = *c.FinishReason
			}
		}
	}
	if content.String() != "嗨" {
		t.Errorf("正文 = %q", content.String())
	}
	if finishSeen != "stop" {
		t.Errorf("finish_reason = %q，期望 stop", finishSeen)
	}
}

// 整包 message → 事件流（网页版那几家的兜底路径）。
func TestWriteAnthropicSSEFromMessage(t *testing.T) {
	msg := []byte(`{"id":"msg_x","content":[{"type":"text","text":"完整回答"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":6}}`)
	var out bytes.Buffer
	in, outTok, err := WriteAnthropicSSEFromMessage(msg, &out, "m", nil)
	if err != nil {
		t.Fatal(err)
	}
	if in != 4 || outTok != 6 {
		t.Errorf("用量 = (%d,%d)", in, outTok)
	}
	events := parseAnthropicEvents(t, out.String())
	if events[0].Event != "message_start" || events[len(events)-1].Event != "message_stop" {
		t.Errorf("首尾事件不对: %s ... %s", events[0].Event, events[len(events)-1].Event)
	}
	var text strings.Builder
	for _, e := range events {
		if e.Event == "content_block_delta" {
			if d, ok := e.Data["delta"].(map[string]any); ok {
				if s, ok := d["text"].(string); ok {
					text.WriteString(s)
				}
			}
		}
	}
	if text.String() != "完整回答" {
		t.Errorf("正文 = %q", text.String())
	}
}

// 上游只发 [DONE] 或干脆空流时，也必须给出合法的事件序列，不能吐半截流。
func TestStreamEmptyUpstreamStillValid(t *testing.T) {
	var out bytes.Buffer
	if _, _, err := StreamOpenAIToAnthropic(strings.NewReader("data: [DONE]\n\n"), &out, "m", nil); err != nil {
		t.Fatal(err)
	}
	events := parseAnthropicEvents(t, out.String())
	if len(events) < 3 {
		t.Fatalf("事件太少: %d", len(events))
	}
	if events[0].Event != "message_start" || events[len(events)-1].Event != "message_stop" {
		t.Errorf("首尾事件不对: %s ... %s", events[0].Event, events[len(events)-1].Event)
	}
}

// 心跳与坏帧不能中断整条流。
func TestStreamToleratesHeartbeatAndBadFrame(t *testing.T) {
	upstream := strings.Join([]string{
		`: heartbeat`,
		``,
		`data: {坏帧}`,
		``,
		`data: {"id":"c","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	var out bytes.Buffer
	if _, _, err := StreamOpenAIToAnthropic(strings.NewReader(upstream), &out, "m", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"text":"ok"`) {
		t.Errorf("坏帧之后的内容没转出来:\n%s", out.String())
	}
}

// 客户端要非流式、上游只肯流式：必须聚合出合法整包，而不是把 SSE 当 JSON 解析。
func TestAggregateOpenAISSE(t *testing.T) {
	upstream := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","content":"你"}}]}`,
		``,
		`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"好"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	raw, err := AggregateOpenAISSE(strings.NewReader(upstream))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("聚合结果不是合法 JSON: %v\n%s", err, raw)
	}
	if got.Object != "chat.completion" || got.Choices[0].Message.Content != "你好" {
		t.Errorf("聚合结果不对: %s", raw)
	}
	if got.Usage.TotalTokens != 5 {
		t.Errorf("用量 = %d，期望 5", got.Usage.TotalTokens)
	}
}

// Anthropic 事件流也能聚合成整包（反向的非流式场景）。
func TestAggregateAnthropicSSE(t *testing.T) {
	upstream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	raw, err := AggregateAnthropicSSE(strings.NewReader(upstream))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("聚合结果不是合法 JSON: %v", err)
	}
	if got.Type != "message" || len(got.Content) == 0 || got.Content[0].Text != "好" {
		t.Errorf("聚合结果不对: %s", raw)
	}
}

func TestLooksLikeSSE(t *testing.T) {
	cases := map[string]bool{
		`data: {"a":1}`:        true,
		`: heartbeat`:          true,
		"event: x\ndata: {}\n": true,
		`{"choices":[]}`:       false,
		``:                     false,
	}
	for in, want := range cases {
		if got := LooksLikeSSE([]byte(in)); got != want {
			t.Errorf("LooksLikeSSE(%.20q) = %v，期望 %v", in, got, want)
		}
	}
}
