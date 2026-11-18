package bridge

// 跨协议流式转换。
//
// 之前这里直接报 400「当前版本还不支持跨协议的流式转换，请把 stream 设为 false」，
// 等于把一个 Claude 客户端（Anthropic 协议）对 OpenAI 上游的请求打回去 ——
// 而 Claude Code 这类客户端默认就走流式，用户根本没法改。所以补上真正的转换：
//
//	Anthropic 客户端 ← OpenAI 上游：chat.completion.chunk → messages 事件流
//	OpenAI 客户端 ← Anthropic 上游：messages 事件流 → chunk 流
//
// 逐帧转换、边转边发，不整包聚合（聚合等于让用户等完整轮生成）。
//
// 几个刻意的取舍：
//   - OpenAI 的 reasoning_content 在 Anthropic 方向**不转成 thinking 块**：
//     Anthropic 的 thinking 块要求 signature，缺了客户端会报错，硬造等于骗客户端。
//     所以只丢思考增量，正文与工具调用完整保留。
//   - Anthropic 的 thinking 块在 OpenAI 方向转成 reasoning_content —— 那是 OpenAI
//     客户端能理解的形式，丢掉反而丢信息。
//   - 工具调用严格按块边界处理：Anthropic 要求 content_block_start/stop 配对，
//     OpenAI 的 arguments 是碎片增量，这里逐片转成 input_json_delta。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// maxSSELine 是单行 SSE 的上限。工具参数一次吐几十 KB 很常见，给足余量。
const maxSSELine = 8 << 20

// sseReader 按 SSE 帧读取，两种协议都能吃：
// OpenAI 只有 data 行；Anthropic 还有 event 行。
type sseReader struct {
	sc     *bufio.Scanner
	event  string
	hasDat bool
}

func newSSEReader(r io.Reader) *sseReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxSSELine)
	return &sseReader{sc: sc}
}

// next 取下一个事件的 (event, data)。data 为 "[DONE]" 时原样返回。
func (s *sseReader) next() (event, data string, ok bool) {
	s.event = ""
	s.hasDat = false
	for s.sc.Scan() {
		line := strings.TrimRight(s.sc.Text(), "\r")
		if line == "" {
			if s.hasDat {
				return s.event, data, true
			}
			s.event = ""
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // 心跳（": heartbeat"）
		}
		if after, found := strings.CutPrefix(line, "event:"); found {
			s.event = strings.TrimSpace(after)
			continue
		}
		if after, found := strings.CutPrefix(line, "data:"); found {
			if s.hasDat {
				data += "\n" // 多行 data 按 SSE 规范拼接
			}
			data += strings.TrimSpace(after)
			s.hasDat = true
		}
	}
	if s.hasDat {
		return s.event, data, true
	}
	return "", "", false
}

func writeSSE(w io.Writer, event string, payload any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if event != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", buf)
	return err
}

// anthropicEmitter 收拢「写 Anthropic 事件 + flush」。
type anthropicEmitter struct {
	dst   io.Writer
	flush func()
}

func (e anthropicEmitter) send(event string, payload any) error {
	if err := writeSSE(e.dst, event, payload); err != nil {
		return err
	}
	if e.flush != nil {
		e.flush()
	}
	return nil
}

// StreamOpenAIToAnthropic 把 OpenAI 的 chunk 流转成 Anthropic 的 messages 事件流。
func StreamOpenAIToAnthropic(src io.Reader, dst io.Writer, model string, flush func()) (inTokens, outTokens int, err error) {
	r := newSSEReader(src)
	em := anthropicEmitter{dst: dst, flush: flush}

	msgID := normalizeID("")
	sentStart := false
	textOpen := false
	nextBlock := 0
	toolBlock := map[int]int{}    // OpenAI tool_calls[].index → Anthropic content block index
	toolStarted := map[int]bool{} // 已发过 content_block_start 的块
	stopReason := "end_turn"

	ensureStart := func() error {
		if sentStart {
			return nil
		}
		sentStart = true
		return em.send("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id": msgID, "type": "message", "role": "assistant", "model": model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": inTokens, "output_tokens": 0},
			},
		})
	}

	for {
		_, data, ok := r.next()
		if !ok || data == "[DONE]" {
			break
		}
		var chunk struct {
			ID      string `json:"id"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue // 单帧坏了不影响整条流
		}
		if chunk.ID != "" {
			msgID = normalizeID(chunk.ID)
		}
		if chunk.Usage != nil {
			if chunk.Usage.PromptTokens > 0 {
				inTokens = chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens > 0 {
				outTokens = chunk.Usage.CompletionTokens
			}
		}
		if serr := ensureStart(); serr != nil {
			return inTokens, outTokens, serr
		}
		for _, ch := range chunk.Choices {
			if t := ch.Delta.Content; t != "" {
				if !textOpen {
					textOpen = true
					if serr := em.send("content_block_start", map[string]any{
						"type": "content_block_start", "index": nextBlock,
						"content_block": map[string]any{"type": "text", "text": ""},
					}); serr != nil {
						return inTokens, outTokens, serr
					}
					nextBlock++
				}
				if serr := em.send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": nextBlock - 1,
					"delta": map[string]any{"type": "text_delta", "text": t},
				}); serr != nil {
					return inTokens, outTokens, serr
				}
			}

			for _, tc := range ch.Delta.ToolCalls {
				// 先关掉文本块：Anthropic 要求一个块内类型单一
				if textOpen {
					if serr := em.send("content_block_stop", map[string]any{
						"type": "content_block_stop", "index": nextBlock - 1,
					}); serr != nil {
						return inTokens, outTokens, serr
					}
					textOpen = false
				}
				idx, seen := toolBlock[tc.Index]
				if !seen {
					idx = nextBlock
					nextBlock++
					toolBlock[tc.Index] = idx
				}
				if !toolStarted[tc.Index] {
					toolStarted[tc.Index] = true
					if serr := em.send("content_block_start", map[string]any{
						"type": "content_block_start", "index": idx,
						"content_block": map[string]any{
							"type": "tool_use", "id": fallbackID(tc.ID, idx),
							"name": tc.Function.Name, "input": map[string]any{},
						},
					}); serr != nil {
						return inTokens, outTokens, serr
					}
				}
				if args := tc.Function.Arguments; args != "" {
					if serr := em.send("content_block_delta", map[string]any{
						"type": "content_block_delta", "index": idx,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
					}); serr != nil {
						return inTokens, outTokens, serr
					}
				}
			}

			if ch.FinishReason != nil {
				stopReason = anthropicStopReason(*ch.FinishReason)
			}
		}
	}

	if serr := ensureStart(); serr != nil {
		return inTokens, outTokens, serr
	}
	if textOpen {
		if serr := em.send("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": nextBlock - 1,
		}); serr != nil {
			return inTokens, outTokens, serr
		}
	}
	// 工具块按开启顺序关闭，保证 Anthropic 的块配对完整
	for i := 0; i < nextBlock; i++ {
		if _, isTool := findKey(toolBlock, i); !isTool {
			continue
		}
		if serr := em.send("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": i,
		}); serr != nil {
			return inTokens, outTokens, serr
		}
	}
	if serr := em.send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outTokens},
	}); serr != nil {
		return inTokens, outTokens, serr
	}
	return inTokens, outTokens, em.send("message_stop", map[string]any{"type": "message_stop"})
}

// StreamAnthropicToOpenAI 把 Anthropic 的事件流转成 OpenAI 的 chunk 流。
func StreamAnthropicToOpenAI(src io.Reader, dst io.Writer, model string) (inTokens, outTokens int, err error) {
	r := newSSEReader(src)
	id := "chatcmpl-allin"
	blockKind := map[int]string{} // Anthropic block index → 块类型
	toolIndex := map[int]int{}    // Anthropic block index → OpenAI tool_calls[].index
	nextTool := 0
	sentRole := false
	finish := "stop"

	emit := func(delta map[string]any, finishReason any) error {
		return writeSSE(dst, "", map[string]any{
			"id": id, "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": delta, "finish_reason": finishReason,
			}},
		})
	}

	for {
		ev, data, ok := r.next()
		if !ok || data == "[DONE]" {
			break
		}
		var raw map[string]json.RawMessage
		if json.Unmarshal([]byte(data), &raw) != nil {
			continue
		}
		typ := ev
		if typ == "" {
			typ = jsonString(raw["type"])
		}
		switch typ {
		case "message_start":
			var msg struct {
				ID    string `json:"id"`
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(raw["message"], &msg) == nil {
				if msg.ID != "" {
					id = normalizeID(msg.ID)
				}
				inTokens = msg.Usage.InputTokens
			}
			if !sentRole {
				sentRole = true
				if e := emit(map[string]any{"role": "assistant", "content": ""}, nil); e != nil {
					return inTokens, outTokens, e
				}
			}

		case "content_block_start":
			var idx int
			_ = json.Unmarshal(raw["index"], &idx)
			var blk struct {
				Type  string `json:"type"`
				ID    string `json:"id"`
				Name  string `json:"name"`
				Text  string `json:"text"`
				Think string `json:"thinking"`
			}
			_ = json.Unmarshal(raw["content_block"], &blk)
			blockKind[idx] = blk.Type
			switch blk.Type {
			case "text":
				if blk.Text != "" {
					if e := emit(map[string]any{"content": blk.Text}, nil); e != nil {
						return inTokens, outTokens, e
					}
				}
			case "thinking":
				if blk.Think != "" {
					if e := emit(map[string]any{"reasoning_content": blk.Think}, nil); e != nil {
						return inTokens, outTokens, e
					}
				}
			case "tool_use":
				ti := nextTool
				nextTool++
				toolIndex[idx] = ti
				if e := emit(map[string]any{"tool_calls": []any{map[string]any{
					"index": ti, "id": blk.ID, "type": "function",
					"function": map[string]any{"name": blk.Name, "arguments": ""},
				}}}, nil); e != nil {
					return inTokens, outTokens, e
				}
			}

		case "content_block_delta":
			var idx int
			_ = json.Unmarshal(raw["index"], &idx)
			var delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
				JSON     string `json:"partial_json"`
			}
			_ = json.Unmarshal(raw["delta"], &delta)
			switch delta.Type {
			case "text_delta":
				if delta.Text != "" {
					if e := emit(map[string]any{"content": delta.Text}, nil); e != nil {
						return inTokens, outTokens, e
					}
				}
			case "thinking_delta":
				if delta.Thinking != "" {
					if e := emit(map[string]any{"reasoning_content": delta.Thinking}, nil); e != nil {
						return inTokens, outTokens, e
					}
				}
			case "input_json_delta":
				if delta.JSON != "" {
					ti, ok := toolIndex[idx]
					if !ok {
						ti = 0
					}
					if e := emit(map[string]any{"tool_calls": []any{map[string]any{
						"index": ti, "function": map[string]any{"arguments": delta.JSON},
					}}}, nil); e != nil {
						return inTokens, outTokens, e
					}
				}
			}

		case "message_delta":
			var d struct {
				StopReason string `json:"stop_reason"`
			}
			_ = json.Unmarshal(raw["delta"], &d)
			finish = openaiFinishReason(d.StopReason)
			var u struct {
				OutputTokens int `json:"output_tokens"`
			}
			if json.Unmarshal(raw["usage"], &u) == nil && u.OutputTokens > 0 {
				outTokens = u.OutputTokens
			}

		case "message_stop", "error":
			goto done
		}
	}

done:
	if !sentRole {
		if e := emit(map[string]any{"role": "assistant", "content": ""}, nil); e != nil {
			return inTokens, outTokens, e
		}
	}
	if e := emit(map[string]any{}, finish); e != nil {
		return inTokens, outTokens, e
	}
	if e := writeSSE(dst, "", map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     inTokens,
			"completion_tokens": outTokens,
			"total_tokens":      inTokens + outTokens,
		},
	}); e != nil {
		return inTokens, outTokens, e
	}
	_, werr := io.WriteString(dst, "data: [DONE]\n\n")
	return inTokens, outTokens, werr
}

// WriteAnthropicSSEFromMessage 把一份完整的 Anthropic message 变成合规的事件流。
//
// 用途：某些上游（网页版那几家）只能整包给结果，但客户端要的是流。
// 与其报 400 让用户去关 stream，不如把整包结果「重放」成一串事件 ——
// 客户端解析逻辑统一，少一条特殊路径。代价是不增量，只影响这几家。
func WriteAnthropicSSEFromMessage(msg []byte, dst io.Writer, model string, flush func()) (inTokens, outTokens int, err error) {
	var src struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if uerr := json.Unmarshal(msg, &src); uerr != nil {
		return 0, 0, fmt.Errorf("响应解析失败: %w", uerr)
	}
	inTokens, outTokens = src.Usage.InputTokens, src.Usage.OutputTokens
	if model == "" {
		model = src.Model
	}
	em := anthropicEmitter{dst: dst, flush: flush}
	if e := em.send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": normalizeID(src.ID), "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": inTokens, "output_tokens": 0},
		},
	}); e != nil {
		return inTokens, outTokens, e
	}
	for i, blk := range src.Content {
		switch blk.Type {
		case "text":
			if e := em.send("content_block_start", map[string]any{
				"type": "content_block_start", "index": i,
				"content_block": map[string]any{"type": "text", "text": ""},
			}); e != nil {
				return inTokens, outTokens, e
			}
			if blk.Text != "" {
				if e := em.send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": i,
					"delta": map[string]any{"type": "text_delta", "text": blk.Text},
				}); e != nil {
					return inTokens, outTokens, e
				}
			}
		case "thinking":
			if e := em.send("content_block_start", map[string]any{
				"type": "content_block_start", "index": i,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			}); e != nil {
				return inTokens, outTokens, e
			}
			if blk.Thinking != "" {
				if e := em.send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": i,
					"delta": map[string]any{"type": "thinking_delta", "thinking": blk.Thinking},
				}); e != nil {
					return inTokens, outTokens, e
				}
			}
		case "tool_use":
			if e := em.send("content_block_start", map[string]any{
				"type": "content_block_start", "index": i,
				"content_block": map[string]any{
					"type": "tool_use", "id": blk.ID, "name": blk.Name, "input": map[string]any{},
				},
			}); e != nil {
				return inTokens, outTokens, e
			}
			if len(blk.Input) > 0 {
				if e := em.send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": i,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": string(blk.Input)},
				}); e != nil {
					return inTokens, outTokens, e
				}
			}
		}
		if e := em.send("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": i,
		}); e != nil {
			return inTokens, outTokens, e
		}
	}
	if e := em.send("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": anthropicStopReason(src.StopReason), "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outTokens},
	}); e != nil {
		return inTokens, outTokens, e
	}
	return inTokens, outTokens, em.send("message_stop", map[string]any{"type": "message_stop"})
}

// findKey 在 map[int]int 里找某个 value 对应的 key。
func findKey(m map[int]int, value int) (int, bool) {
	for k, v := range m {
		if v == value {
			return k, true
		}
	}
	return 0, false
}

// anthropicStopReason 把 OpenAI 的 finish_reason 映射成 Anthropic 的 stop_reason。
func anthropicStopReason(r string) string {
	switch r {
	case "length", "max_tokens":
		return "max_tokens"
	case "tool_calls", "function_call", "tool_use":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// openaiFinishReason 是反向映射。
func openaiFinishReason(r string) string {
	switch r {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// fallbackID 兜底生成工具调用 id：有些上游只在第一帧给 id。
func fallbackID(id string, idx int) string {
	if id != "" {
		return id
	}
	return fmt.Sprintf("toolu_%d", idx)
}

// ---------- 非流式请求碰上流式上游 ----------

// LooksLikeSSE 判断一段响应体是不是 SSE 事件流。
//
// 有些上游（WorkBuddy、Trae）只接受流式，网关会把 stream 强制改成 true，
// 于是「客户端要非流式」时拿到的仍是 SSE。以前这里直接把 SSE 字节当 JSON
// 解析，报的是「上游响应解析失败: invalid character ':'」—— 一句和现场无关
// 的错误。现在先按这里的结果分流：是 SSE 就先聚合，再按需转换。
func LooksLikeSSE(raw []byte) bool {
	head := strings.TrimSpace(string(raw))
	if head == "" {
		return false
	}
	return strings.HasPrefix(head, "data:") || strings.HasPrefix(head, "event:") || strings.HasPrefix(head, ":")
}

// AggregateOpenAISSE 把 OpenAI 的 chunk 流聚合成一个 chat.completion。
func AggregateOpenAISSE(src io.Reader) ([]byte, error) {
	r := newSSEReader(src)
	var (
		id      string
		content strings.Builder
		reason  strings.Builder
		finish  = "stop"
		got     bool
		inTok   int
		outTok  int
		tools   = map[int]*toolAcc{}
		order   []int
	)
	for {
		_, data, ok := r.next()
		if !ok || data == "[DONE]" {
			break
		}
		var chunk struct {
			ID      string `json:"id"`
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue
		}
		got = true
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Usage != nil {
			inTok, outTok = chunk.Usage.PromptTokens, chunk.Usage.CompletionTokens
		}
		for _, ch := range chunk.Choices {
			content.WriteString(ch.Delta.Content)
			reason.WriteString(ch.Delta.ReasoningContent)
			for _, tc := range ch.Delta.ToolCalls {
				acc, ok := tools[tc.Index]
				if !ok {
					acc = &toolAcc{}
					tools[tc.Index] = acc
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				acc.name += tc.Function.Name
				acc.args.WriteString(tc.Function.Arguments)
			}
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
		}
	}
	if !got {
		return nil, fmt.Errorf("上游没有返回任何有效事件")
	}
	msg := map[string]any{"role": "assistant", "content": content.String()}
	if s := reason.String(); s != "" {
		msg["reasoning_content"] = s
	}
	if len(order) > 0 {
		var calls []map[string]any
		for _, idx := range order {
			acc := tools[idx]
			args := acc.args.String()
			if args == "" {
				args = "{}"
			}
			calls = append(calls, map[string]any{
				"id": acc.id, "type": "function",
				"function": map[string]any{"name": acc.name, "arguments": args},
			})
		}
		msg["tool_calls"] = calls
	}
	if id == "" {
		id = "chatcmpl-allin"
	}
	return json.Marshal(map[string]any{
		"id": normalizeID(id), "object": "chat.completion", "model": "",
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage": map[string]any{
			"prompt_tokens":     inTok,
			"completion_tokens": outTok,
			"total_tokens":      inTok + outTok,
		},
	})
}

// AggregateAnthropicSSE 把 Anthropic 的事件流聚合成一个 message。
func AggregateAnthropicSSE(src io.Reader) ([]byte, error) {
	r := newSSEReader(src)
	var (
		id       string
		model    string
		text     strings.Builder
		think    strings.Builder
		stopReas = "end_turn"
		inTok    int
		outTok   int
		tools    = map[int]*toolAcc{}
		order    []int
		got      bool
	)
	for {
		ev, data, ok := r.next()
		if !ok || data == "[DONE]" {
			break
		}
		var raw map[string]json.RawMessage
		if json.Unmarshal([]byte(data), &raw) != nil {
			continue
		}
		typ := ev
		if typ == "" {
			typ = jsonString(raw["type"])
		}
		switch typ {
		case "message_start":
			got = true
			var msg struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(raw["message"], &msg) == nil {
				id, model, inTok = msg.ID, msg.Model, msg.Usage.InputTokens
			}
		case "content_block_start":
			var idx int
			_ = json.Unmarshal(raw["index"], &idx)
			var blk struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			_ = json.Unmarshal(raw["content_block"], &blk)
			if blk.Type == "tool_use" {
				tools[idx] = &toolAcc{id: blk.ID, name: blk.Name}
				order = append(order, idx)
			}
		case "content_block_delta":
			var idx int
			_ = json.Unmarshal(raw["index"], &idx)
			var d struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
				JSON     string `json:"partial_json"`
			}
			_ = json.Unmarshal(raw["delta"], &d)
			switch d.Type {
			case "text_delta":
				text.WriteString(d.Text)
			case "thinking_delta":
				think.WriteString(d.Thinking)
			case "input_json_delta":
				if acc, ok := tools[idx]; ok {
					acc.args.WriteString(d.JSON)
				}
			}
		case "message_delta":
			var d struct {
				StopReason string `json:"stop_reason"`
			}
			_ = json.Unmarshal(raw["delta"], &d)
			if d.StopReason != "" {
				stopReas = d.StopReason
			}
			var u struct {
				OutputTokens int `json:"output_tokens"`
			}
			if json.Unmarshal(raw["usage"], &u) == nil && u.OutputTokens > 0 {
				outTok = u.OutputTokens
			}
		case "message_stop":
			got = true
		}
	}
	if !got {
		return nil, fmt.Errorf("上游没有返回任何有效事件")
	}
	content := []map[string]any{}
	if s := think.String(); s != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": s})
	}
	if s := text.String(); s != "" || len(order) == 0 {
		content = append(content, map[string]any{"type": "text", "text": s})
	}
	for _, idx := range order {
		acc := tools[idx]
		var input json.RawMessage = []byte("{}")
		if acc.args.Len() > 0 {
			input = json.RawMessage(acc.args.String())
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": acc.id, "name": acc.name, "input": input,
		})
	}
	if id == "" {
		id = "msg_allin"
	}
	return json.Marshal(map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model,
		"content": content, "stop_reason": stopReas, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": inTok, "output_tokens": outTok},
	})
}

// toolAcc 累计一条工具调用的碎片（OpenAI 与 Anthropic 两侧共用）。
type toolAcc struct {
	id   string
	name string
	args strings.Builder
}
