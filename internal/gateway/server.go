// Package gateway 是 HTTP 入口：对外提供 OpenAI / Anthropic 兼容接口，
// 对内按 model 名路由到上游供应商，并在协议不一致时自动互转。
package gateway

import (
	"bytes"
	"context"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"allin/internal/bridge"
	"allin/internal/config"
	"allin/internal/fallback"
	"allin/internal/prompt"
	"allin/internal/provider"
	"allin/internal/provider/codex"
	"allin/internal/provider/qoder"
	"allin/internal/provider/trae"
	"allin/internal/provider/workbuddy"
)

// maxBodyBytes 是单个请求体的上限，防止有人拿它当上传通道。
const maxBodyBytes = 8 << 20 // 8 MiB

// maxAccountAttempts 是一条请求最多尝试几个账号（换号重试）。
const maxAccountAttempts = 3

// Server 是网关实例。
type Server struct {
	store *config.Store
	pool  *provider.Pool
	mux   *http.ServeMux

	requests atomic.Int64
	errors   atomic.Int64
	switches atomic.Int64
	started  time.Time

	// ttft 保存最近请求的首字延迟采样，供「日志」页展示。
	// 受 config.Server.LogCapture 开关控制。
	ttft *ttftBuffer
}

// New 构造网关。
func New(store *config.Store) *Server {
	s := &Server{store: store, pool: provider.NewPool(), mux: http.NewServeMux(), started: time.Now(), ttft: newTTFTBuffer()}
	s.routes()
	return s
}

func (s *Server) routes() {
	// 模型接口
	s.mux.HandleFunc("/v1/models", s.withAuth(s.handleModels))
	s.mux.HandleFunc("/v1/chat/completions", s.withAuth(s.handleChat))
	s.mux.HandleFunc("/v1/messages", s.withAuth(s.handleMessages))
	s.mux.HandleFunc("/v1/responses", s.withAuth(s.handleResponses))
	s.mux.HandleFunc("/v1/images/generations", s.withAuth(s.handleImages))

	// 管理接口
	s.mux.HandleFunc("/api/status", s.withAuth(s.handleStatus))
	s.mux.HandleFunc("/api/config", s.withAuth(s.handleConfig))
	s.mux.HandleFunc("/api/models", s.withAuth(s.handleModels))
	s.mux.HandleFunc("/api/accounts", s.withAuth(s.handleAccounts))
	s.mux.HandleFunc("/api/health", s.withAuth(s.handleHealth))
	s.mux.HandleFunc("/api/update/check", s.withAuth(s.handleUpdateCheck))
	s.mux.HandleFunc("/api/update/apply", s.withAuth(s.handleUpdateApply))
	s.mux.HandleFunc("/api/providers", s.withAuth(s.handleProvidersOrWrite))
	// 模型目录同步：POST 拉上游目录并写回配置，GET 只看不写。
	s.mux.HandleFunc("/api/providers/sync", s.withAuth(s.handleProviderSync))
	s.mux.HandleFunc("/api/providers/catalog", s.withAuth(s.handleProviderCatalog))
	s.mux.HandleFunc("/api/providers/write", s.withAuth(s.handleProviderWrite))
	// 模型筛选：保存「对外暴露哪些模型」的勾选结果（见 expose.go）。
	s.mux.HandleFunc("/api/providers/models/expose", s.withAuth(s.handleProviderExpose))
	s.mux.HandleFunc("/api/accounts/write", s.withAuth(s.handleAccountWrite))
	// 日志页：采样查询、总开关、清空
	s.mux.HandleFunc("/api/logs", s.withAuth(s.handleLogs))
	s.mux.HandleFunc("/api/logs/toggle", s.withAuth(s.handleLogsToggle))
	s.mux.HandleFunc("/api/logs/clear", s.withAuth(s.handleLogsClear))
	s.mux.HandleFunc("/api/accounts/export", s.withAuth(s.handleAccountsExport))
	s.mux.HandleFunc("/api/accounts/import", s.withAuth(s.handleAccountsImport))
	s.mux.HandleFunc("/api/effort", s.withAuth(s.handleEffort))
	s.mux.HandleFunc("/api/prompt", s.withAuth(s.handlePrompt))
	s.mux.HandleFunc("/api/prompt/preset", s.withAuth(s.handlePromptPreset))
	s.mux.HandleFunc("/api/sanitize", s.withAuth(s.handleSanitize))
	s.mux.HandleFunc("/api/keys", s.withAuth(s.handleKeys))
	s.mux.HandleFunc("/api/keys/toggle", s.withAuth(s.handleKeyToggle))
	s.mux.HandleFunc("/api/usage", s.withAuth(s.handleUsage))
	s.mux.HandleFunc("/api/usage/summary", s.withAuth(s.handleUsageSummary))
	s.mux.HandleFunc("/api/usage/reset", s.withAuth(s.handleUsageReset))
	s.mux.HandleFunc("/api/auth/workbuddy/start", s.withAuth(s.handleWorkBuddyStart))
	s.mux.HandleFunc("/api/auth/workbuddy/poll", s.withAuth(s.handleWorkBuddyPoll))
	s.mux.HandleFunc("/api/auth/github-copilot/start", s.withAuth(s.handleCopilotStart))
	s.mux.HandleFunc("/api/auth/github-copilot/poll", s.withAuth(s.handleCopilotPoll))
	s.mux.HandleFunc("/api/auth/trae/start", s.withAuth(s.handleTraeStart))
	s.mux.HandleFunc("/api/auth/trae/poll", s.withAuth(s.handleTraePoll))
	// 第二批供应商登录：antigravity catpaw cline codex minimax qoder raccoon wenxin
	// 统一走 /api/auth/<vendor>/start 与 /api/auth/<vendor>/poll（见 provider_auth.go）。
	// 上面三条精确路径优先匹配，这里只兜住其余子路径。
	s.mux.HandleFunc("/api/auth/", s.withAuth(s.handleProviderAuth))
	s.mux.HandleFunc("/api/providers/models/probe", s.withAuth(s.handleModelProbe))
	s.mux.HandleFunc("/api/presets", s.withAuth(s.handlePresets))
	s.mux.HandleFunc("/api/presets/apply", s.withAuth(s.handlePresetApply))
	s.mux.HandleFunc("/api/port/notice", s.withAuth(s.handlePortNotice))
	s.mux.HandleFunc("/api/port/switch", s.withAuth(s.handlePortSwitch))
	s.mux.HandleFunc("/api/schedule", s.withAuth(s.handleSchedule))
	s.mux.HandleFunc("/api/workbuddy/catalog/diagnose", s.withAuth(s.handleCatalogDiagnose))
	s.mux.HandleFunc("/api/workbuddy/credits", s.withAuth(s.handleWorkBuddyCredits))
	s.mux.HandleFunc("/api/workbuddy/checkin", s.withAuth(s.handleWorkBuddyCheckin))
	s.mux.HandleFunc("/api/workbuddy/checkin-status", s.withAuth(s.handleWorkBuddyCheckin))
	// 任务中心：POST 跑全套（report→checkin→streak→claim-gift→compensation→
	// makeup→buddy→travel→lottery），GET 只读汇总。
	s.mux.HandleFunc("/api/workbuddy/tasks", s.withAuth(s.handleWorkBuddyTasks))
	s.mux.HandleFunc("/api/workbuddy/tasks/status", s.withAuth(s.handleWorkBuddyTaskStatus))

	// 面板
	s.mux.HandleFunc("/", s.handleIndex)
}

// Start 启动后台任务（Token 自动刷新巡检、WorkBuddy 积分快照刷新）。
// ctx 取消时全部退出。
func (s *Server) Start(ctx context.Context) {
	s.startTokenRefresher(ctx)
	s.startCreditsRefresher(ctx)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// ---------- 鉴权 ----------

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := s.store.Snapshot()
		if cfg.Server.AuthKey == "" && len(cfg.APIKeys) == 0 {
			next(w, r)
			return
		}
		got := bearerOf(r)
		if got == "" {
			got = r.URL.Query().Get("key")
		}
		if !keyAllowed(cfg, got) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": "需要网关口令",
				"hint":  "请求头带 Authorization: Bearer <authKey>，或在 URL 加 ?key=<authKey>",
			})
			return
		}
		next(w, r)
	}
}

func bearerOf(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if k := r.Header.Get("x-api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	return ""
}

// ---------- 模型列表 ----------

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()
	names := provider.ModelList(cfg)
	data := make([]map[string]any, 0, len(names))
	for _, n := range names {
		data = append(data, map[string]any{
			"id":       n,
			"object":   "model",
			"created":  s.started.Unix(),
			"owned_by": "allin",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// ---------- 转发核心 ----------

type protocol string

const (
	protocolOpenAI    protocol = "openai"
	protocolAnthropic protocol = "anthropic"
)

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, "/chat/completions", protocolOpenAI)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, "/messages", protocolAnthropic)
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, "/responses", protocolOpenAI)
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	s.forward(w, r, "/images/generations", protocolOpenAI)
}

// forward 是核心：改写 model → 选账号 → 必要时转换协议 → 发上游 → 失败换号重试。
func (s *Server) forward(w http.ResponseWriter, r *http.Request, apiPath string, proto protocol) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	s.requests.Add(1)
	// 首字延迟归因用：从收到请求开始计时，各阶段累加进 ttft。
	// 只用于日志，不参与任何逻辑判断。
	tReq := time.Now()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	// 诊断：记录每个进来的请求（模型名 + 大小 + 是否流式）
	{
		var peek struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &peek)
		log.Printf("[REQ] path=%s model=%q stream=%v size=%d", apiPath, peek.Model, peek.Stream, len(body))
		// 诊断：把请求体落盘，便于复现奇怪的结构问题
		if len(body) > 100*1024 {
			_ = os.WriteFile("/data/local/tmp/allin-last-request.json", body, 0o644)
		}
	}
	if err != nil {
		s.fail(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}

	var head struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		s.fail(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error())
		return
	}

	cfg := s.store.Snapshot()
	// 请求级记账上下文：一条客户端请求可能换号、换模型甚至换供应商，
	// 最终记账以真正提供服务的那个供应商/模型为准（见 usageCtx）。
	uc := newUsageCtx(head.Model)
	// 模型名允许带思考档位后缀：workbuddy/glm-5.3:high
	modelNoSuffix, effortSuffix := splitModelEffort(head.Model)
	// 客户端请求体里的档位也要能读到（resolveEffort 需要它做 auto 模式判断）
	var reqFields map[string]json.RawMessage
	_ = json.Unmarshal(body, &reqFields)
	p, _, realModel, err := provider.Resolve(cfg, modelNoSuffix)
	if err != nil {
		// 模型解析失败都是「客户端参数问题」，统一 400。
		// 同时把可用模型塞进 error 里，客户端/日志能直接看到该改成什么。
		//
		// 日志里记下原始 model 字段与请求体摘要，方便排查
		// 「客户端到底发了个什么名字」这类问题。
		log.Printf("[400 模型解析失败] model=%q err=%v bodyHead=%s",
			head.Model, err, truncate(string(body), 300))
		s.failModel(w, err.Error(), cfg)
		return
	}

	kind := p.Kind
	if kind == "" {
		kind = provider.KindOpenAI
	}
	kind = provider.NormalizeKind(kind)
	// 网页版私有协议三家（妙手 / 文心 / Antigravity）整条链路自己走：请求拼装、会话机、
	// 信封、HTTP/1.1 约束都在各自包里，套不进下面「BuildRequest + 透传/桥接」的框架。
	switch kind {
	case provider.KindCatPaw, provider.KindWenxin, provider.KindAntigravity:
		s.forwardWebChat(w, r, p, kind, realModel, body, head.Stream, proto, uc)
		return
	}
	// DeepSeek 网页版整条链路自己走：端点、PoW、会话链、patch 流都和 OpenAI 兼容
	// 上游不一样，套不进下面的「透传 / 协议桥接」框架。
	if kind == provider.KindDeepSeekW {
		s.forwardDeepSeek(w, r, p, realModel, body, head.Stream, proto, uc)
		return
	}
	// 上游协议：workbuddy 这类特殊供应商走 OpenAI 形态
	upstreamProto := protocolOpenAI
	if kind == provider.KindAnthropic || kind == provider.KindMiniMax {
		upstreamProto = protocolAnthropic
	}

	// 客户端协议与上游协议不一致时做转换
	needBridge := proto != upstreamProto
	upstreamBody := body
	// 跨协议时路径必须跟着上游协议走，否则会打到不存在的端点
	// （Anthropic 客户端 /v1/messages → OpenAI 上游，仍拼 /messages 就是 404）
	if needBridge {
		if upstreamProto == protocolAnthropic {
			apiPath = "/messages"
		} else {
			apiPath = "/chat/completions"
		}
	}
	if needBridge {
		// 跨协议的流式现在做真正的逐帧转换（见 bridge/stream.go），
		// 不再要求客户端关掉 stream —— Claude 客户端默认就是流式，没法关。
		if proto == protocolAnthropic && upstreamProto == protocolOpenAI {
			upstreamBody, err = bridge.AnthropicToOpenAI(body, realModel)
		} else if proto == protocolOpenAI && upstreamProto == protocolAnthropic {
			upstreamBody, err = bridge.OpenAIToAnthropicRequest(body, realModel)
		}
		if err != nil {
			s.fail(w, http.StatusBadRequest, "协议转换失败: "+err.Error())
			return
		}
	} else {
		upstreamBody, err = rewriteModel(body, realModel)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, "改写请求体失败: "+err.Error())
			return
		}
	}

	// 诊断：确认改写后请求体里的 model 是什么
	{
		var chk struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(upstreamBody, &chk)
		log.Printf("[MODEL] client=%q realModel=%q upstreamModel=%q", head.Model, realModel, chk.Model)
	}

	// preTrimBody 保存裁剪前的请求体，供「上游嫌大 → 换更小预算重裁」时使用。
	var preTrimBody []byte
	trimRounds := 0
	trimBudget := 0
	softRetries := 0
	// channelRepairs 是「渠道校验失败后改写消息开头」的重发次数。
	// 每次都会把更多开头挪出匹配区，正常情况下一次就够。
	channelRepairs := 0
	// channelBase 是被渠道校验拒绝时那份请求体的快照，救援改写永远从它出发。
	var channelBase []byte

	// WorkBuddy 需要请求体适配：工具名规范化、tool_choice 归一化、补 reasoning_effort
	if kind == provider.KindWorkBuddy {
		effort := resolveEffort(cfg, effortSuffix, reqFields)
		if effort == "" && p.Headers != nil {
			effort = p.Headers["X-Allin-Effort"]
		}
		if prepared, perr := workbuddy.Prepare(upstreamBody, effort); perr == nil {
			upstreamBody = prepared
		} else {
			log.Printf("WorkBuddy 请求预处理失败（按原样发送）: %v", perr)
		}
		// 官方客户端兼容规则（移植自 dsh-connect-workbuddy 的实测规则集）：
		// developer→system、补首条 system、句子级指纹改词、身份子句断词、
		// 命中前缀指纹的消息补中性开头。都是便宜且幂等的改写。
		if ident, changed := workbuddy.ApplyIdentityRules(upstreamBody); changed {
			upstreamBody = ident
		}
		// 身份词屏蔽：把上下文里会被上游认成「第三方 Agent」的词换成中性说法，
		// 降低被渠道校验（11-128）盯上的概率。放在裁剪之前，后面的快照与重裁
		// 都基于已经屏蔽过的内容，避免重裁时把原文又捡回来。
		if sc := cfg.Sanitize; sc.IsEnabled() {
			terms := sc.EffectiveTerms()
			repl := sc.EffectiveReplacement()
			if masked, n, merr := workbuddy.MaskTerms(upstreamBody, terms, repl); merr == nil && n > 0 {
				upstreamBody = masked
				log.Printf("身份词屏蔽：替换 %d 处（%s → %s）", n, strings.Join(terms, " / "), repl)
			}
		}
		// 上下文裁剪：先按默认阈值裁一次。
		// 若上游仍嫌大（见下面的续写逻辑），会用更小预算重裁。
		// 保留裁剪前快照，续写时从它出发，避免在已裁过的产物上二次裁导致越裁越碎。
		preTrimBody = upstreamBody
		if tr, terr := workbuddy.TrimContext(upstreamBody); terr == nil {
			if tr.Trimmed {
				log.Printf("上下文裁剪：丢弃 %d 条历史，估算 %d tokens", tr.Dropped, tr.Estimated)
			}
			upstreamBody = tr.Body
		}
		// WorkBuddy 只接受流式：上游一律 stream:true，客户端要非流式时本地聚合
		if forced, ferr := workbuddy.ForceStream(upstreamBody); ferr == nil {
			upstreamBody = forced
		}
	}

	// Codex 走 Responses 协议，请求体要从 chat 形态转过去
	if kind == provider.KindCodex {
		if converted, cerr := codex.BuildChatRequest(upstreamBody, realModel); cerr == nil {
			if fin, ferr := codex.FinalizeResponses(converted); ferr == nil {
				upstreamBody = fin
			} else {
				upstreamBody = converted
			}
		} else {
			log.Printf("Codex 请求转换失败: %v", cerr)
		}
	}

	// 系统提示词注入：网关层强制，客户端改不掉
	if ptext, perr := loadPromptText(cfg); perr == nil && ptext != "" {
		if upstreamProto == protocolAnthropic {
			if nb, aerr := prompt.ApplyAnthropic(upstreamBody, ptext, cfg.Prompt.Mode); aerr == nil {
				upstreamBody = nb
			}
		} else {
			if nb, aerr := prompt.ApplyOpenAI(upstreamBody, ptext, cfg.Prompt.Mode); aerr == nil {
				upstreamBody = nb
			}
		}
	} else if perr != nil {
		log.Printf("提示词注入失败（按原样发送）: %v", perr)
	}

	// 其它供应商：把档位按各自协议写进请求体
	if kind != provider.KindWorkBuddy {
		effort := resolveEffort(cfg, effortSuffix, reqFields)
		if effort != "" {
			if nb, eerr := applyEffortToBody(upstreamBody, effort, upstreamProto == protocolAnthropic); eerr == nil {
				upstreamBody = nb
			}
		}
	}

	// 流式请求要上游把 token 用量带回来：OpenAI 系补 stream_options.include_usage。
	// 不认识这个字段的上游会直接忽略，无害；Anthropic 协议本来就在 message_delta 里给。
	if upstreamProto == protocolOpenAI {
		upstreamBody = withStreamUsage(upstreamBody)
	}

	base := p.BaseURL
	if base == "" {
		base = provider.DefaultBaseURL(kind)
	}
	if base == "" {
		s.fail(w, http.StatusBadRequest, fmt.Sprintf("供应商 %s 没有配置 baseUrl", p.ID))
		uc.fail("")
		return
	}

	// Trae 的响应是它自己的事件流，必须逐帧转换，不能透传；转换时要认识「声明过哪些工具」，
	// 否则正文里文本形式的工具调用会被当成普通文字。工具表从送给上游的那份请求体里取。
	// （工具表在下面每个降级步骤里按 stepBody 现算，见 relayTrae 调用处）

	// 三层降级：同模型换号 → 同供应商换模型 → 放弃
	chain := fallback.Chain(cfg, p, realModel)
	modelFallback := true
	var lastErrMsg string

	for _, step := range chain {
		if !modelFallback {
			break // 已经拿到非额度类错误，不再继续换模型
		}
		sp := step.Provider
		if sp == nil {
			continue
		}
		stepBase := sp.BaseURL
		if stepBase == "" {
			stepBase = provider.DefaultBaseURL(kind)
		}

		// 换模型时要重新改写请求体（model 名不同）
		//
		// 注意：必须在「已裁剪」的 upstreamBody 上改，不能用原始 body。
		// 用原始 body 会把上面的裁剪全部丢掉，等于把 4MB 原样重发，
		// 而且 Prepare 会在已裁产物上再跑一遍，工具名映射可能对不上，
		// 上游就会报 11148「tool calls and tool results do not match」。
		stepBody := upstreamBody
		if step.Model != realModel {
			if nb, rerr := rewriteModel(upstreamBody, step.Model); rerr == nil {
				stepBody = nb
				// Prepare 已在上面做过（工具名规范化等），这里只补与模型相关的那步。
				// 重复 Prepare 会让工具名二次映射，破坏 tool_calls 与 tool 结果的对应。
				if kind == provider.KindWorkBuddy {
					if nb3, e3 := workbuddy.ForceStream(stepBody); e3 == nil {
						stepBody = nb3
					}
				}
				if nb4, e4 := applyEffortToBody(stepBody, resolveEffort(cfg, effortSuffix, reqFields), upstreamProto == protocolAnthropic); e4 == nil {
					stepBody = nb4
				}
				if upstreamProto == protocolOpenAI {
					// 上面的改写会重新序列化请求体，include_usage 得补回来
					stepBody = withStreamUsage(stepBody)
				}
			}
		}

		tried := map[string]bool{}
		// 会话绑定：同一段对话钉在同一账号，避免每轮换号触发上游限流
		sessKey := provider.SessionKey(sp.ID, sessionFingerprint(body))
		modelExhausted := false
		for attempt := 0; attempt < maxAccountAttempts; attempt++ {
			acct := s.pool.PickForSession(sp, tried, sessKey)
			if acct == nil {
				modelExhausted = true
				break
			}
			if attempt > 0 {
				s.switches.Add(1)
				log.Printf("换号重试: %s/%s（第 %d 次）", sp.ID, acct.ID, attempt+1)
			}
			tried[acct.ID] = true

			upreq, err := provider.BuildRequest(kind, stepBase, apiPath, acct.Key(), acct, stepBody, sp.Headers)
			if err != nil {
				s.fail(w, http.StatusInternalServerError, "构造上游请求失败: "+err.Error())
				return
			}
			// 客户端要流式就告诉上游要 SSE。跨协议时也一样：请求体已经被转成
			// 上游协议的 stream:true，这里再因为 needBridge 去掉 Accept 头，
			// 有些上游会按非流式回 JSON，转换器就一口也读不到。
			if head.Stream || kind == provider.KindWorkBuddy || kind == provider.KindTrae || kind == provider.KindOpenAI {
				upreq.Header.Set("Accept", "text/event-stream")
			}
			upreq = upreq.WithContext(r.Context())

			tTry := time.Now()
			resp, err := provider.HTTPClient.Do(upreq)
			if err != nil {
				uc.fail(sp.ID, step.Model)
				s.pool.Fail(sp.ID, acct.ID, 0, err.Error())
				// 绑定的账号不可用了，解绑让下一轮重新选
				s.pool.UnbindSession(sessKey)
				lastErrMsg = err.Error()
				log.Printf("上游请求失败 %s/%s: %v", sp.ID, acct.ID, err)
				continue
			}

			// 记录所有非 2xx 响应（含 4xx），之前只有 401/429/5xx 才记
			// 首字延迟归因：这一轮到上游拿到响应头用了多久。
			// 同时写进环形缓冲，供面板「日志」页展示（受开关控制）。
			upMs := time.Since(tTry).Milliseconds()
			totalMs := time.Since(tReq).Milliseconds()
			log.Printf("[TTFT] upstream=%dms attempt=%d acct=%s status=%d total=%dms",
				upMs, attempt+1, acct.ID, resp.StatusCode, totalMs)
			if cfg.Server.LogCaptureEnabled() {
				s.ttft.Add(ttftSample{
					At:         time.Now().Unix(),
					Model:      head.Model,
					Account:    acct.ID,
					TotalMs:    totalMs,
					UpstreamMs: upMs,
					Attempts:   attempt + 1,
					Status:     resp.StatusCode,
					OverheadMs: totalMs - upMs,
				})
			}
			if resp.StatusCode >= 400 {
				log.Printf("[UPSTREAM] %d %s/%s model=%s", resp.StatusCode, sp.ID, acct.ID, step.Model)
				// 渠道校验失败是「请求来源不被认可」的硬拒绝，换号/换模型都不会成功。
				// 不在这里拦住的话会白跑满 3 次换号 + 整条降级链，用户干等十几秒
				// 却只看到一句「本机出错」。
				sniff, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
				// 4xx 的响应体一定要留证：上游常说「参数无效」但不告诉你是哪个参数，
				// 没有原文根本没法排查。
				if resp.StatusCode >= 400 {
					log.Printf("[UPSTREAM-BODY] %d %s: %s", resp.StatusCode, acct.ID, truncate(string(sniff), 600))
				}
				if fallback.IsChannelBlocked(string(sniff)) {
					resp.Body.Close()
					// 渠道校验是上游按「请求像不像官方客户端」做的判定。
					// 换号、换模型发出去的都是同一份内容，所以换号无效。
					//
					// 兜底分两步，两步都要做：
					//   1) 屏蔽身份词（全量、任意位置）—— 上游若在全文中找指纹，
					//      只挪开头没有用（线上日志证实：改写开头后仍被拒）。
					//   2) 改写被扫描消息的开头 —— 对付只看开头的那类规则。
					// 之后再退避重发：上游自己回的 displayMsg 就写着「请求被拦截，
					// 请重新发送」，同一份内容重发本来就常常能过。
					if kind == provider.KindWorkBuddy && channelRepairs < maxChannelRepairs {
						// 始终从「被拒那一版的原始请求体」出发改写：
						// 在已经改过的产物上再改会让断词层层叠加（实测每轮多加
						// 一批 U+200B），既浪费也不可预测。
						if channelBase == nil {
							channelBase = stepBody
						}
						if nb, changed := s.repairChannelBlock(cfg, channelBase, channelRepairs+1); changed {
							channelRepairs++
							stepBody = nb
							if forced, ferr := workbuddy.ForceStream(stepBody); ferr == nil {
								stepBody = forced
							}
							log.Printf("渠道校验失败（code 11-128），屏蔽身份词+改写开头后重发第 %d 次: %s/%s",
								channelRepairs, sp.ID, acct.ID)
							// 内容已变，同一个号值得再试一次：把这次尝试退回，既不消耗
							// 换号次数，也不能把账号留在「已试过」里 —— 否则下一轮
							// PickForSession 无人可选，会误判成「额度耗尽」进降级链。
							delete(tried, acct.ID)
							s.pool.Reset(sp.ID, acct.ID)
							s.pool.UnbindSession(sessKey)
							attempt--
							// 退避：连发同一份内容更容易再次撞上同一个判定。
							select {
							case <-time.After(time.Duration(400*channelRepairs) * time.Millisecond):
							case <-r.Context().Done():
								return
							}
							continue
						}
					}
					log.Printf("渠道校验失败（code 11128），改写后仍被拒: %s/%s", sp.ID, acct.ID)
					s.fail(w, http.StatusBadRequest, fallback.ChannelBlockedHint())
					return
				}
				// code 11148（tool calls and tool results do not match）。
				//
				// 实测结论：同一份字节重发就能成功，与请求大小、图片数量、
				// tool 配对结构都无关（配对 743 对零缺失，5 张图连发 5 次全过）。
				// 这是上游的瞬态拒绝，所以正确做法是「原样重发」，
				// 而不是改动请求 —— 改请求反而会引入新问题。
				if kind == provider.KindWorkBuddy && fallback.IsTransientToolReject(string(sniff)) &&
					softRetries < maxSoftRetries {
					resp.Body.Close()
					softRetries++
					log.Printf("上游瞬态拒绝（11148），原样重发第 %d 次: %s/%s",
						softRetries, sp.ID, acct.ID)
					// 不换号、不改请求、不消耗换号次数
					attempt--
					select {
					case <-time.After(time.Duration(300*softRetries) * time.Millisecond):
					case <-r.Context().Done():
						return
					}
					continue
				}
				// 其它「请求太大」类错误：才用更小预算重裁再发。
				// 不限制图片、不无脑截断，只在真被拒时才逐级收紧。
				if kind == provider.KindWorkBuddy && fallback.IsContextTooLarge(string(sniff)) &&
					trimRounds < maxTrimRounds && len(preTrimBody) > 0 {
					resp.Body.Close()
					next := trimBudget
					if next == 0 {
						next = workbuddy.DefaultPromptBudget()
					}
					next = next * 2 / 3 // 每轮砍掉 1/3
					if next < minTrimBudget {
						next = minTrimBudget
					}
					trimBudget = next
					trimRounds++
					tr, terr := workbuddy.TrimContextTo(preTrimBody, trimBudget)
					if terr != nil {
						lastErrMsg = "重新裁剪失败: " + terr.Error()
						continue
					}
					stepBody = tr.Body
					log.Printf("上游嫌请求过大，第 %d 轮重裁：预算 %d tokens，建议 %d tokens",
						trimRounds, trimBudget, tr.Estimated)
					// 用同一账号、同一个 attempt 重发，不消耗换号次数
					attempt--
					continue
				}
				// 读出来的内容要还原回去，否则下面 relay 拿到的是空 body
				resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(sniff), resp.Body))
			}
			if resp.StatusCode == 401 || resp.StatusCode == 429 || resp.StatusCode >= 500 {
				raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				bodyText := string(raw)
				uc.fail(sp.ID, step.Model)
				s.pool.Fail(sp.ID, acct.ID, resp.StatusCode, truncate(bodyText, 160))
				s.pool.UnbindSession(sessKey)
				lastErrMsg = truncate(bodyText, 200)
				log.Printf("上游 %d，换号: %s/%s", resp.StatusCode, sp.ID, acct.ID)
				// 额度类错误 → 整个模型都别试了，直接进下一个模型
				if fallback.ShouldFallbackModel(resp.StatusCode, bodyText) {
					modelExhausted = true
					break
				}
				// 429 换号前退避：连发会触发更严的风控
				if resp.StatusCode == 429 {
					backoff := time.Duration(600+300*attempt) * time.Millisecond
					select {
					case <-time.After(backoff):
					case <-r.Context().Done():
						return
					}
				}
				continue
			}

			if step.Model != realModel {
				log.Printf("模型降级生效: %s → %s（%s/%s）", realModel, step.Model, sp.ID, acct.ID)
				s.switches.Add(1)
			}
			s.pool.Success(sp.ID, acct.ID)
			if kind == provider.KindTrae {
				// Trae 的流式转换要知道请求里声明了哪些工具
				s.relayTrae(w, resp, head.Stream, needBridge, proto, step.Model, trae.ToolSpecs(stepBody), sp.ID, uc)
				return
			}
			s.relay(w, resp, head.Stream, needBridge, proto, step.Model, sp.ID, uc)
			return
		}

		if !modelExhausted {
			// 出现了非额度类失败（如网络错误），换模型帮助不大，但也试一次
			continue
		}
		log.Printf("模型 %s 在 %s 上额度耗尽，尝试降级", step.Model, step.ProviderID)
	}

	uc.fail("")
	if lastErrMsg != "" {
		s.fail(w, http.StatusBadGateway, "所有账号与模型均失败，最后一次错误: "+lastErrMsg)
	} else {
		s.fail(w, http.StatusBadGateway, "所有账号尝试均失败")
	}
	return
}

// relay 把上游响应回写给客户端，必要时转换协议，并在收尾时记一次用量。
//
// providerID 是真正提供服务的供应商（换号换模型后可能与请求最初选中的不同），
// uc 是这条客户端请求的记账上下文。
func (s *Server) relay(w http.ResponseWriter, resp *http.Response, stream, needBridge bool, clientProto protocol, model, providerID string, uc *usageCtx) {
	// 关闭传播（上游 1.2.39 的 Antigravity 教训）：客户端断开后取消上游请求，
	// 否则转发 goroutine 会陪着上游慢慢吐完才退出。
	if rctx, ok := resp.Request.Context().(context.Context); ok {
		ctx, cancel := context.WithCancel(rctx)
		_ = ctx
		_ = cancel
		resp.Body = &cancelOnCloseBody{ReadCloser: resp.Body, cancel: cancel}
	}
	defer resp.Body.Close()

	// 流式且不需要转换：边透传边扫 usage（SSE 最后一帧里），不缓存整条响应
	if stream && !needBridge {
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "text/event-stream"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(resp.StatusCode)
		insp := &sseInspector{}
		flushCopy(w, resp.Body, insp.feed)
		// 已经写完流：ok 只看上游状态码，token 拿不到就记 0
		uc.record(providerID, model, resp.StatusCode < 400, insp.result())
		return
	}

	// 流式且需要转换：逐帧转换后写给客户端，不缓存整条响应
	if stream && needBridge {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(resp.StatusCode)
		flusher, canFlush := w.(http.Flusher)
		flush := func() {
			if canFlush {
				flusher.Flush()
			}
		}
		var inTok, outTok int
		var cerr error
		if clientProto == protocolAnthropic {
			// 客户端 Anthropic ← 上游 OpenAI
			inTok, outTok, cerr = bridge.StreamOpenAIToAnthropic(resp.Body, w, model, flush)
		} else {
			// 客户端 OpenAI ← 上游 Anthropic
			inTok, outTok, cerr = bridge.StreamAnthropicToOpenAI(resp.Body, w, model)
			flush()
		}
		if cerr != nil {
			// 已经写过帧，状态码改不了了：记日志 + 记账，别假装成功
			log.Printf("流式协议转换中断（%s ← %s）: %v", clientProto, model, cerr)
			uc.record(providerID, model, false, usageTokens{In: inTok, Out: outTok, Total: inTok + outTok})
			return
		}
		uc.record(providerID, model, true, usageTokens{In: inTok, Out: outTok, Total: inTok + outTok})
		return
	}

	// 非流式：完整读取后按需转换
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		uc.fail(providerID, model)
		s.fail(w, http.StatusBadGateway, "读取上游响应失败: "+err.Error())
		return
	}
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300

	// 客户端要非流式，但上游只肯流式（WorkBuddy / Trae 会被强制 stream:true）：
	// 先把事件流聚合成整包，再走下面的转换/回写。不做这一步的话，
	// 下一步会拿 SSE 字节当 JSON 解析，报「invalid character ':'」这种看不懂的错。
	if ok && bridge.LooksLikeSSE(raw) {
		var aggErr error
		if needBridge && clientProto != protocolAnthropic {
			// 客户端 OpenAI、上游 Anthropic
			raw, aggErr = bridge.AggregateAnthropicSSE(bytes.NewReader(raw))
		} else {
			raw, aggErr = bridge.AggregateOpenAISSE(bytes.NewReader(raw))
		}
		if aggErr != nil {
			uc.record(providerID, model, false, usageTokens{})
			s.fail(w, http.StatusBadGateway, "上游返回事件流但聚合失败: "+aggErr.Error())
			return
		}
	}

	u := usageFromJSON(raw)
	if !needBridge {
		uc.record(providerID, model, ok, u)
		ct := resp.Header.Get("Content-Type")
		if ct == "" || bridge.LooksLikeSSE(raw) {
			ct = "application/json"
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	if !ok {
		uc.record(providerID, model, false, u)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	var converted []byte
	if clientProto == protocolAnthropic {
		converted, err = bridge.OpenAIToAnthropic(raw, model)
	} else {
		converted, err = bridge.AnthropicToOpenAIResponse(raw, model)
	}
	if err != nil {
		uc.record(providerID, model, false, u)
		s.fail(w, http.StatusBadGateway, "响应协议转换失败: "+err.Error())
		return
	}
	uc.record(providerID, model, true, u)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(converted)
}

// relayTrae 把 Trae 的上游事件流转成标准 OpenAI 响应。
//
// Trae 上游只会回它自己的 SSE（event/data 帧），必须逐帧转换后写给客户端，
// 不能像其他供应商那样直接透传字节流；客户端要非流式时用 ConvertStream 整条聚合。
func (s *Server) relayTrae(w http.ResponseWriter, resp *http.Response, stream, needBridge bool, clientProto protocol, model string, tools []trae.ToolSpec, providerID string, uc *usageCtx) {
	defer resp.Body.Close()

	// 上游明确报错时回的是普通 JSON（不是事件流），原样转出去让客户端看清原因。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		uc.record(providerID, model, false, usageFromJSON(raw))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	// 非流式：把整条上游流聚合成一个 chat.completion。
	if !stream {
		out, err := trae.ConvertStreamWithTools(resp.Body, model, tools)
		if err != nil {
			log.Printf("Trae 响应聚合失败: %v", err)
			uc.fail(providerID, model)
			s.fail(w, http.StatusBadGateway, "Trae 响应聚合失败: "+err.Error())
			return
		}
		u := usageFromJSON(out) // 聚合结果里带着上游 token_usage 帧累加出来的 usage
		if needBridge {
			converted, cerr := bridge.OpenAIToAnthropic(out, model)
			if cerr != nil {
				log.Printf("Trae 响应协议转换失败: %v", cerr)
				uc.record(providerID, model, false, u)
				s.fail(w, http.StatusBadGateway, "响应协议转换失败: "+cerr.Error())
				return
			}
			out = converted
		}
		uc.record(providerID, model, true, u)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		return
	}

	// 流式：一行一行喂给状态机，转换结果逐帧写回。
	st := trae.NewStreamState(model)
	if err := st.DeclareTools(tools); err != nil {
		uc.fail(providerID, model)
		s.fail(w, http.StatusBadGateway, "Trae 工具声明失败: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	send := func(frames []string) bool {
		for _, f := range frames {
			if _, err := io.WriteString(w, f); err != nil {
				return false
			}
		}
		if canFlush && len(frames) > 0 {
			flusher.Flush()
		}
		return true
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20) // 单帧上限 1 MiB，留够余量
	// token 用量在 Trae 自己的 token_usage 事件里，这里按行扫一遍上游原始帧
	insp := &sseInspector{}
	for sc.Scan() {
		line := sc.Text()
		insp.feed([]byte(line + "\n"))
		frames, err := trae.TransformEvent(line, st)
		if err != nil {
			// 已经开始写响应体，改不了状态码，只能收尾（客户端读到 EOF 会视为结束）。
			log.Printf("Trae 流转换中断: %v", err)
			uc.record(providerID, model, false, insp.result())
			return
		}
		if !send(frames) {
			uc.record(providerID, model, false, insp.result())
			return
		}
		if st.Terminal {
			uc.record(providerID, model, true, insp.result())
			return
		}
	}
	if err := sc.Err(); err != nil {
		log.Printf("Trae 上游读取中断: %v", err)
		uc.record(providerID, model, false, insp.result())
		return
	}
	// 兜底：最后一个没以空行收尾的帧补发掉（含末尾的 [DONE]）。
	frames, err := trae.Flush(st)
	if err != nil {
		log.Printf("Trae 流收尾失败: %v", err)
		uc.record(providerID, model, false, insp.result())
		return
	}
	send(frames)
	uc.record(providerID, model, true, insp.result())
}

// relayQoder 把 Qoder 的上游事件流转成标准 OpenAI 响应。
//
// Qoder 的聊天端点只给 SSE，而且每帧 data 外面还包着 {"statusCodeValue":200,"body":"…"}，
// body 里才是 OpenAI 形态的 chunk（请求带 Encode=1 时 body 还可能被自定义 base64 编过）。
// 所以这里不能透传字节流：流式逐帧解出来重写，非流式整条聚合成一个 chat.completion。
func (s *Server) relayQoder(w http.ResponseWriter, resp *http.Response, stream, needBridge bool, clientProto protocol, model, providerID string, uc *usageCtx) {
	defer resp.Body.Close()

	// 上游明确报错时回的是普通 JSON（不是事件流），原样转出去让客户端看清原因。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		uc.record(providerID, model, false, usageFromJSON(raw))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return
	}

	// 非流式：整条流聚合成一个 chat.completion，需要时再转成 Anthropic 形态。
	if !stream {
		out, err := qoder.Aggregate(resp.Body, model)
		if err != nil {
			log.Printf("Qoder 响应聚合失败: %v", err)
			uc.fail(providerID, model)
			s.fail(w, http.StatusBadGateway, "Qoder 响应聚合失败: "+err.Error())
			return
		}
		u := usageFromJSON(out) // 聚合结果里的 usage 来自上游帧
		if needBridge && clientProto == protocolAnthropic {
			converted, cerr := bridge.OpenAIToAnthropic(out, model)
			if cerr != nil {
				log.Printf("Qoder 响应协议转换失败: %v", cerr)
				uc.record(providerID, model, false, u)
				s.fail(w, http.StatusBadGateway, "响应协议转换失败: "+cerr.Error())
				return
			}
			out = converted
		}
		uc.record(providerID, model, true, u)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	// 解一帧写一帧并 flush，客户端才能看到增量；写出去的字节顺带扫 usage。
	insp := &sseInspector{}
	dst := &flushWriter{w: w, canFlush: canFlush, tap: insp.feed}
	if err := qoder.RelayStream(dst, resp.Body); err != nil {
		// 已经开始写响应体，改不了状态码，只能收尾（客户端读到 EOF 会视为结束）。
		log.Printf("Qoder 流转换中断: %v", err)
		uc.record(providerID, model, false, insp.result())
		return
	}
	if canFlush {
		flusher.Flush()
	}
	uc.record(providerID, model, true, insp.result())
}

// flushWriter 让 qoder 的逐帧写出立即落到客户端。
//
// tap 可选：在写之前把字节交给它扫描（用来看流里的 usage），不影响写出内容。
type flushWriter struct {
	w        http.ResponseWriter
	canFlush bool
	tap      func([]byte)
}

func (f *flushWriter) Write(p []byte) (int, error) {
	if f.tap != nil {
		f.tap(p)
	}
	n, err := f.w.Write(p)
	if f.canFlush {
		if flusher, ok := f.w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	return n, err
}

// flushCopy 把 body 原样写给 w 并逐块 flush。
//
// onChunk 可为 nil；提供时每读到一块就先回调（用于边转发边抽 SSE 的 usage），
// 回调里只允许做轻量扫描，不能改动字节内容。
// sseIdleTimeout 是流式响应「上游多久没吐数据就认为它死了」的上限。
// 上游卡住时 body.Read 会永久阻塞，客户端只能干等到自己超时断开——
// 表现就是「回答到一半突然中断」。设上限后能主动收尾并写一条错误事件。
const sseIdleTimeout = 90 * time.Second

// sseHeartbeat 是空闲时向客户端写 SSE 注释的间隔。
// 中间层（尤其移动网络、反向代理）会掐掉长时间无数据的连接，定期发心跳能保住。
const sseHeartbeat = 15 * time.Second

// relayContext 把「客户端连接断开」传播给上游响应体。
// 上游 1.2.39 的教训：Antigravity 之前下游关闭时上游连接没关，转发 goroutine 一直挂着。
// 这里在上游 Body 外再包一层：客户端写失败（已断开）时，关闭包装层 → 上游读取随之退出。
type cancelOnCloseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelOnCloseBody) Close() error {
	if b.cancel != nil {
		b.cancel()
	}
	return b.ReadCloser.Close()
}

func flushCopy(w http.ResponseWriter, body io.Reader, onChunk func([]byte)) {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)

	for {
		// 把阻塞读放进 goroutine，外面用 select 做超时与心跳
		type readResult struct {
			n   int
			err error
		}
		ch := make(chan readResult, 1)
		go func() {
			n, err := body.Read(buf)
			ch <- readResult{n, err}
		}()

		heartbeat := time.NewTimer(sseHeartbeat)
		deadline := time.NewTimer(sseIdleTimeout)

		var res readResult
	waitLoop:
		for {
			select {
			case res = <-ch:
				heartbeat.Stop()
				deadline.Stop()
				break waitLoop
			case <-heartbeat.C:
				// 上游还没吐数据，先给客户端发个 SSE 注释保活
				if canFlush {
					if _, werr := w.Write([]byte(": keepalive\n\n")); werr != nil {
						heartbeat.Stop()
						deadline.Stop()
						return
					}
					flusher.Flush()
				}
				heartbeat.Reset(sseHeartbeat)
			case <-deadline.C:
				log.Printf("流式响应超过 %s 没有数据，主动收尾", sseIdleTimeout)
				heartbeat.Stop()
				// 告诉客户端这是异常结束，别让它一直等
				if canFlush {
					_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"上游长时间无响应，已中断\",\"type\":\"gateway_timeout\"}}\n\n"))
					flusher.Flush()
				}
				return
			}
		}

		if res.n > 0 {
			if onChunk != nil {
				onChunk(buf[:res.n])
			}
			if _, werr := w.Write(buf[:res.n]); werr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if res.err == io.EOF {
			return
		}
		if res.err != nil {
			log.Printf("上游读取中断: %v", res.err)
			return
		}
	}
}

// rewriteModel 只替换 JSON 里的 model 字段，保持其余字节不变。
func rewriteModel(body []byte, model string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	m["model"] = raw
	return json.Marshal(m)
}

// failModel 是模型解析失败时的专用响应：附带可用模型列表，方便直接改配置。
func (s *Server) failModel(w http.ResponseWriter, msg string, cfg *config.Config) {
	s.errors.Add(1)
	available := provider.ModelList(cfg)
	// 只取前 30 个，避免响应过大
	trimmed := available
	if len(trimmed) > 30 {
		trimmed = trimmed[:30]
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "invalid_request_error",
			"code":    "model_not_available",
			"param":   "model",
			// 客户端不会解析这个字段，但用户看日志/抓包能直接看到
			"available_models": trimmed,
			"total_models":     len(available),
			"hint":             "模型名要写成 provider/model，可在面板「供应商」页的模型筛选里查看已暴露的模型",
		},
	})
}

// repairChannelBlock 构造「渠道校验被拒之后」的重发请求体。
//
// 返回 changed=false 表示这份请求已经没有可改的地方了 —— 再发一次就是
// 同样的字节，只会再付一次同样的 400，所以调用方应当直接报错。
//
// 两步一起做，因为线上日志证实单靠一步救不回来：
//
//   - 屏蔽身份词：上游若在全文里找第三方 Agent 的自我描述，只挪开头没有用。
//     这里用配置的词表；配置里关掉了屏蔽也仍然用默认词表兜这次重发 ——
//     请求已经被拒了，此时再用默认词表只会提高通过率，不会更糟。
//   - 改写开头：对付只看消息开头的那类规则。
//
// 两步都只动文本，不碰 tools / tool_choice / tool 配对，避免换来 11148。
func (s *Server) repairChannelBlock(cfg *config.Config, body []byte, round int) ([]byte, bool) {
	out := body
	changed := false

	terms := config.DefaultSanitizeTerms
	repl := config.DefaultSanitizeReplacement
	if cfg != nil && cfg.Sanitize.IsEnabled() {
		terms = cfg.Sanitize.EffectiveTerms()
		repl = cfg.Sanitize.EffectiveReplacement()
	}
	// 第一步：屏蔽身份词（全量、任意位置）
	if masked, n, err := workbuddy.MaskTerms(out, terms, repl); err == nil && n > 0 {
		out = masked
		changed = true
	}
	// 第二步：强制给每条被扫描消息补中性开头（未知指纹一样被挪走）
	if neutral, ok := workbuddy.ForceNeutralHead(out); ok {
		out = neutral
		changed = true
	}
	// 第三步：插不可见断词，破精确子串匹配。第 1 轮用稀疏档（实测够用、改动最小），
	// 第 2 轮用密集档（每个词内部都断），用来对付前两档都没清掉的子串指纹。
	dense := round >= 2
	if perturbed, ok := workbuddy.PerturbFingerprints(out, dense); ok {
		out = perturbed
		changed = true
	}
	return out, changed
}

func (s *Server) fail(w http.ResponseWriter, code int, msg string) {
	s.errors.Add(1)
	writeJSON(w, code, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "gateway_error",
		},
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// decodeJSON 读取有上限的 JSON 请求体。
func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

// extractUsage 从非流式响应体里抽 (输入, 输出) token。
//
// 实际解析在 usage_stream.go 的 usageFromJSON：OpenAI 的
// prompt_tokens/completion_tokens 与 Anthropic 的 input_tokens/output_tokens 都认。
func extractUsage(raw []byte) (int, int) {
	u := usageFromJSON(raw)
	return u.In, u.Out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// 渐进式裁剪参数。
//
// 设计目标：不要一上来就把图片、长文本砍掉。
// 第一次按默认阈值裁，只有上游真的嫌大才逐轮收紧，
// 每轮砍掉当前预算的 1/3，最多几轮。
const (
	maxTrimRounds = 4
	minTrimBudget = 120_000
)

// maxSoftRetries 是「上游瞬态拒绝」的原地重发次数。
// 11148 属瞬时状态，重发通常就过；给 2 次足够，避免掩盖真问题。
const maxSoftRetries = 2

// maxChannelRepairs 是「渠道校验失败 → 改写消息开头 → 重发」的次数。
// 改写对所有被扫描的开头生效，一轮即可把整份请求挪出匹配区；
// 给 2 次是为了兜住首轮只改到部分开头的情况。
const maxChannelRepairs = 2
