// 本文件是「网页版私有协议」三家的转发层（妙手 CatPaw / 百度文心 / Antigravity）。
//
// 为什么不能复用 forward 的那套代码：forward 的核心假设是「一个上游请求 = 一次 HTTP，
// 请求体透传或按协议桥接」。这三家不成立：
//
//	CatPaw       是 round → event → turn 三步会话机，只有第三步出 SSE，还要求同一份
//	             message 在两次请求里 id 完全一致；
//	Antigravity  要把 chat 请求包进 v1internal 信封（project / sessionId / requestId），
//	             并且传输层必须是 HTTP/1.1；
//	文心         聊天协议未取证，只到认证层（Run 明确回 ErrNotImplemented）。
//
// 所以这里只保留 forward 里那部分「选号 → 调上游 → 失败换号」的骨架，请求拼装与响应
// 翻译全部交给各包的 Run。另一个关键差别：上游吐出第一帧之前**不写响应头**，这样首帧
// 之前失败仍可以换号，或者把真实状态码（401/429/501…）回给客户端。
package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"allin/internal/bridge"
	"allin/internal/config"
	"allin/internal/provider"
	"allin/internal/provider/antigravity"
	"allin/internal/provider/catpaw"
	"allin/internal/provider/wenxin"
)

// forwardWebChat 处理三家网页版协议的聊天请求。
func (s *Server) forwardWebChat(w http.ResponseWriter, r *http.Request, p *config.Provider, kind, realModel string, body []byte, stream bool, proto protocol, uc *usageCtx) {
	// 客户端说 Anthropic 形态时先转成 OpenAI 请求内部使用。
	//
	// 这三家（妙手 / 文心 / Antigravity）的上游只能整包给结果，没有可逐帧转换的
	// OpenAI chunk 流，所以流式请求这里改成「内部非流式 + 把整包重放成事件流」：
	// 客户端拿到的是合规的 Anthropic SSE，只是不增量。比直接报 400 让用户去关
	// stream 要好 —— Claude 客户端默认就是流式，那个开关它根本不给你。
	needBridge := proto == protocolAnthropic
	clientStream := stream
	upstreamBody := body
	if needBridge {
		converted, err := bridge.AnthropicToOpenAI(body, realModel)
		if err != nil {
			uc.fail(p.ID, realModel)
			s.fail(w, http.StatusBadRequest, "协议转换失败: "+err.Error())
			return
		}
		upstreamBody = converted
		// 内部一律走非流式：上游聚合完再整包重放
		stream = false
	}

	tried := map[string]bool{}
	var lastErr error
	// 三家各有一条「调一次上游」的入口。与 forward 里 relayQoder / relayTrae 的位置对应，
	// 区别是这三家连「上游请求怎么拼、响应怎么解」都在各自包里（它们都不是单次透传），
	// 网关只负责选号、写响应、换号。
	var run func(context.Context, *config.Account, string, []byte, io.Writer, bool) error
	switch kind {
	case provider.KindCatPaw:
		run = runCatPaw
	case provider.KindWenxin:
		run = runWenxin
	case provider.KindAntigravity:
		run = runAntigravity
	default:
		uc.fail(p.ID, realModel)
		s.fail(w, http.StatusNotImplemented, "未知的网页版协议: "+kind)
		return
	}

	sessKey := provider.SessionKey(p.ID, sessionFingerprint(body))
	for attempt := 0; attempt < maxAccountAttempts; attempt++ {
		acct := s.pool.PickForSession(p, tried, sessKey)
		if acct == nil {
			uc.fail(p.ID, realModel)
			if attempt == 0 {
				s.fail(w, http.StatusBadRequest, fmt.Sprintf("供应商 %s 没有可用账号", p.ID))
			} else {
				s.fail(w, webChatStatus(lastErr), fmt.Sprintf("供应商 %s 的账号都试过了，全部失败: %v", p.ID, lastErr))
			}
			return
		}
		if attempt > 0 {
			s.switches.Add(1)
			log.Printf("换号重试: %s/%s（第 %d 次）", p.ID, acct.ID, attempt+1)
		}
		tried[acct.ID] = true

		// 流式：先挂一个「首帧才写响应头」的闸门；非流式：整份结果先在内存里攒好。
		var (
			dst  io.Writer
			gate *streamGate
			buf  *bytes.Buffer
		)
		if stream {
			gate = newStreamGate(w)
			dst = gate
		} else {
			buf = &bytes.Buffer{}
			dst = buf
		}

		err := run(r.Context(), acct, realModel, upstreamBody, dst, stream)
		if err == nil {
			s.pool.Success(p.ID, acct.ID)
			if stream {
				// 内容已经边转边写给客户端了；用量从写出去的 SSE 里扫
				uc.record(p.ID, realModel, true, gate.usage())
				return
			}
			out := buf.Bytes()
			u := usageFromJSON(out) // 聚合结果里带不带 usage 取决于供应商包
			if needBridge {
				converted, cerr := bridge.OpenAIToAnthropic(out, realModel)
				if cerr != nil {
					log.Printf("响应协议转换失败: %v", cerr)
					uc.record(p.ID, realModel, false, u)
					s.fail(w, http.StatusBadGateway, "响应协议转换失败: "+cerr.Error())
					return
				}
				out = converted
				if clientStream {
					// 客户端要的是流：把整包重放成合规的 Anthropic 事件流
					w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
					w.Header().Set("Cache-Control", "no-cache")
					w.Header().Set("Connection", "keep-alive")
					w.Header().Set("X-Accel-Buffering", "no")
					w.WriteHeader(http.StatusOK)
					flusher, canFlush := w.(http.Flusher)
					flush := func() {
						if canFlush {
							flusher.Flush()
						}
					}
					if _, _, serr := bridge.WriteAnthropicSSEFromMessage(out, w, realModel, flush); serr != nil {
						log.Printf("%s/%s 流式重放失败: %v", p.ID, acct.ID, serr)
						uc.record(p.ID, realModel, false, u)
						return
					}
					uc.record(p.ID, realModel, true, u)
					return
				}
			}
			uc.record(p.ID, realModel, true, u)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(out)
			return
		}

		lastErr = err
		status := webChatStatus(err)
		// 已经往客户端写过帧：状态码改不了了，只能记日志收尾。
		if gate != nil && gate.opened {
			uc.record(p.ID, realModel, false, gate.usage())
			log.Printf("%s/%s 流式转发中断: %v", p.ID, acct.ID, err)
			return
		}
		uc.fail(p.ID, realModel)
		s.pool.Fail(p.ID, acct.ID, status, truncate(err.Error(), 160))
		log.Printf("%s/%s 上游失败（http=%d）: %v", p.ID, acct.ID, status, err)
		// 请求内容不被协议支持 / 能力未实现：换号也没用，直接回，别拿账号池空转。
		if status == http.StatusBadRequest || status == http.StatusNotImplemented {
			s.fail(w, status, err.Error())
			return
		}
	}
	uc.fail(p.ID, realModel)
	s.fail(w, webChatStatus(lastErr), fmt.Sprintf("供应商 %s 的所有账号尝试均失败: %v", p.ID, lastErr))
}

// runCatPaw 调美团妙手：access 就是 Cookie 里的 X-Passport-Token。
func runCatPaw(ctx context.Context, acct *config.Account, model string, body []byte, dst io.Writer, stream bool) error {
	ca := &catpaw.Account{
		ID: acct.ID, Name: acct.Name, UID: acct.UID,
		Token: acct.Key(), AddedAt: acct.AddedAt, Enabled: true,
	}
	return catpaw.Run(ctx, ca, model, body, dst, stream)
}

// runWenxin 调百度文心网页版：access 是整条浏览器 Cookie 串。
//
// 当前只到认证层：包里的 Run 会明确回 ErrNotImplemented（网关映射成 501）。
func runWenxin(ctx context.Context, acct *config.Account, model string, body []byte, dst io.Writer, stream bool) error {
	wa := wenxin.FromCookie(acct.Key())
	wa.ID, wa.Name, wa.UID = acct.ID, acct.Name, acct.UID
	return wenxin.Run(ctx, wa, model, body, dst, stream)
}

// runAntigravity 调 Code Assist 聊天端点。
//
// 字段映射：access → Access，refresh → Refresh，expires 原样，domain → Project（Cloud 项目 id）。
// 注意 token 刷新只改内存里这份临时账号，没有写回配置 —— 配置里的 expires 仍是旧值，
// 所以过期后每个请求都会多走一次 OAuth 刷新（+200ms 左右）。要彻底消除得把刷新结果落盘，
// 那属于配置层的改动，本轮不动。
func runAntigravity(ctx context.Context, acct *config.Account, model string, body []byte, dst io.Writer, stream bool) error {
	key := acct.Key()
	ga := &antigravity.Account{
		ID: acct.ID, Name: acct.Name, Email: acct.Email,
		Access: key, Token: key, Refresh: acct.Refresh, Expires: acct.Expires,
		Project: acct.Domain, Enabled: true,
	}
	return antigravity.Run(ctx, ga, model, body, dst, stream)
}

// webChatStatus 把上游错误映射成回给客户端的 HTTP 状态码。
func webChatStatus(err error) int {
	if err == nil {
		return http.StatusOK
	}
	// 三家包各自的 UpstreamError 都实现了 StatusCode()，能取到就直接用上游的状态。
	var se interface{ StatusCode() int }
	if errors.As(err, &se) && se.StatusCode() > 0 {
		return se.StatusCode()
	}
	switch {
	case errors.Is(err, catpaw.ErrUnsupported):
		return http.StatusBadRequest
	case errors.Is(err, antigravity.ErrInsecureURL):
		return http.StatusBadRequest
	case errors.Is(err, wenxin.ErrNotImplemented):
		return http.StatusNotImplemented
	case errors.Is(err, catpaw.ErrSignInRequired),
		errors.Is(err, antigravity.ErrSignInRequired),
		errors.Is(err, wenxin.ErrNoCredential):
		return http.StatusUnauthorized
	}
	return http.StatusBadGateway
}

// streamGate 延迟写响应头：首帧到达才提交 200 + SSE 头。
//
// 这么做的直接收益是「上游首帧前报错仍然可以换号或回真实状态码」；
// 一旦 opened 了，后面就只能按流收尾（HTTP 协议上没法再改状态码）。
type streamGate struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	canFlush bool
	opened   bool
	insp     sseInspector // 顺带扫写出去的 SSE，拿 token 用量
}

// newStreamGate 包装客户端的 ResponseWriter。
func newStreamGate(w http.ResponseWriter) *streamGate {
	f, ok := w.(http.Flusher)
	return &streamGate{w: w, flusher: f, canFlush: ok}
}

// usage 返回这条流里扫到的 token 用量（没扫到就是 0）。
func (g *streamGate) usage() usageTokens { return g.insp.result() }

// Write 第一次写入时补响应头，之后每写一次就 flush（客户端才能看到逐字输出）。
func (g *streamGate) Write(p []byte) (int, error) {
	if !g.opened {
		g.opened = true
		g.w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		g.w.Header().Set("Cache-Control", "no-cache")
		g.w.Header().Set("Connection", "keep-alive")
		g.w.Header().Set("X-Accel-Buffering", "no")
		g.w.WriteHeader(http.StatusOK)
	}
	g.insp.feed(p)
	n, err := g.w.Write(p)
	if g.canFlush {
		g.flusher.Flush()
	}
	return n, err
}
