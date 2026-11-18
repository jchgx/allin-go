// Package config 负责配置的加载、校验与落盘。
//
// 配置结构与 All In（Java 版）的 config.json 保持兼容，这样原来的配置可以直接搬过来：
//
//	{
//	  "server":    {"host":"127.0.0.1","port":6868,"authKey":"mp-..."},
//	  "providers": [{...}],
//	  "routing":   {"groupsEnabled":false,"bareModelFallback":false},
//	  "usage":     {...}
//	}
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Server 是监听与鉴权设置。
type Server struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	AuthKey string `json:"authKey"`
	// AuthKeyOrigin 记录口令来源：first-run-random 表示首次启动自动生成。
	AuthKeyOrigin string `json:"authKeyOrigin,omitempty"`
	// LogCapture 是总日志开关。默认开启。
	// 关闭后：不写 gateway.log（所有 log.Printf 静默），
	// 也不采集「日志」页的请求样本，内存里已有的样本一并清掉。
	LogCapture *bool `json:"logCapture,omitempty"`
}

// LogCaptureEnabled 返回日志采样是否开启（未设置视为开启）。
func (s *Server) LogCaptureEnabled() bool {
	return s.LogCapture == nil || *s.LogCapture
}

// Account 是某个供应商下的一个账号（可能是 API Key，也可能是登录态）。
type Account struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Type    string `json:"type,omitempty"` // token / api / oauth
	Enabled *bool  `json:"enabled,omitempty"`
	Access  string `json:"access,omitempty"`  // 访问令牌 / API Key
	Refresh string `json:"refresh,omitempty"` // 刷新令牌
	UID     string `json:"uid,omitempty"`
	Domain  string `json:"domain,omitempty"`
	Realm   string `json:"realm,omitempty"` // cn / global
	// Email 是账号邮箱：Qoder 的 COSY 信封里要带一份（展示用，可留空）。
	Email   string `json:"email,omitempty"`
	Expires int64  `json:"expires,omitempty"`
	AddedAt int64  `json:"addedAt,omitempty"`
	// 签到记录（WorkBuddy 等支持签到的供应商使用）
	CheckinDate    string `json:"checkinDate,omitempty"`
	CheckinOutcome string `json:"checkinOutcome,omitempty"`
	CheckinCredit  int    `json:"checkinCredit,omitempty"`
	CheckinStreak  int    `json:"checkinStreak,omitempty"`
	// 任务中心记录（WorkBuddy 任务自动化使用）。
	// LastTaskRun 是最近一次跑全套的时间（Unix 秒）；TasksRun 是那次的全套结果，
	// 键为任务名（report/checkin/streak/claim-gift/compensation/makeup/buddy/
	// travel-depart/travel-claim/lottery），值为 ok/already/skipped/locked/failed。
	LastTaskRun int64             `json:"lastTaskRun,omitempty"`
	TasksRun    map[string]string `json:"tasksRun,omitempty"`
	// TaskOutcomeDetail 保存每项任务最近一次的说明文字（上游 msg 或本地判定）。
	TaskOutcomeDetail map[string]string `json:"taskOutcomeDetail,omitempty"`
	// 设备身份（Trae 这类要求设备证明的供应商使用）
	DeviceID   string `json:"deviceId,omitempty"`
	MachineID  string `json:"machineId,omitempty"`
	PublicKey  string `json:"publicKey,omitempty"`
	PrivateKey string `json:"privateKey,omitempty"` // 敏感：不要外发
}

// IsEnabled 默认启用。
func (a *Account) IsEnabled() bool {
	return a == nil || a.Enabled == nil || *a.Enabled
}

// Key 返回可用的凭据：优先 access，其次 refresh。
func (a *Account) Key() string {
	if a == nil {
		return ""
	}
	if a.Access != "" {
		return a.Access
	}
	return a.Refresh
}

// Provider 是一个上游供应商。
type Provider struct {
	ID       string     `json:"id"`
	Name     string     `json:"name,omitempty"`
	Enabled  *bool      `json:"enabled,omitempty"`
	BaseURL  string     `json:"baseUrl,omitempty"`
	Kind     string     `json:"kind,omitempty"` // openai / anthropic / workbuddy / deepseek-web ...
	APIKey   string     `json:"apiKey,omitempty"`
	Accounts []*Account `json:"accounts,omitempty"`
	Models   []string   `json:"models,omitempty"`
	// ExposedModels 是「对外暴露」的模型子集（面板勾选结果）。
	// 为空表示全部暴露（兼容老配置）；非空时只暴露这些，且必须同时出现在 Models 里。
	// 它只影响 /v1/models 与裸模型名解析，不影响 Models 本身（目录同步照常追加）。
	ExposedModels []string `json:"exposedModels,omitempty"`
	// AutoSyncModels 为真时，网关启动后在后台同步一次模型目录（只追加不覆盖，
	// 口径同 POST /api/providers/sync）。默认 false：目录同步要打上游，不该默认消耗。
	AutoSyncModels bool `json:"autoSyncModels,omitempty"`
	// Headers 是发往上游时附加的固定头（个别供应商需要 X-Domain 之类）。
	Headers map[string]string `json:"headers,omitempty"`
	// HeaderPrefix 供特殊供应商在运行时用账号信息拼头。
	Notes string `json:"notes,omitempty"`

	// RouteMode 是账号路由策略：
	//   ""/"sticky"  会话绑定（默认）：同一窗口钉在一个账号上，避免频繁换号触发风控
	//   "rotate"     轮询：每个请求换一个账号
	//   "balance"    用量均衡：优先用完额度比例最低的账号
	//   "expiry"     到期优先：优先用完最早过期的额度
	RouteMode string `json:"routeMode,omitempty"`
}

// 路由策略常量。
const (
	RouteSticky  = "sticky"
	RouteRotate  = "rotate"
	RouteBalance = "balance"
	RouteExpiry  = "expiry"
)

// RouteModeOrDefault 返回规范化后的路由策略。
func (p *Provider) RouteModeOrDefault() string {
	if p == nil || p.RouteMode == "" {
		return RouteSticky
	}
	return p.RouteMode
}

// IsEnabled 默认启用。
func (p *Provider) IsEnabled() bool {
	return p == nil || p.Enabled == nil || *p.Enabled
}

// Exposed 返回真正对外暴露的模型（始终按 provider.Models 的顺序）。
//
// ExposedModels 为空 = 全部暴露（老配置兼容）；非空时只返回其中的，
// 且必须同时存在于 Models —— 面板勾选过的模型被目录同步漏掉/手动删掉时，不留幽灵条目。
func (p *Provider) Exposed() []string {
	if p == nil {
		return nil
	}
	if len(p.ExposedModels) == 0 {
		return p.Models
	}
	want := make(map[string]bool, len(p.ExposedModels))
	for _, m := range p.ExposedModels {
		want[m] = true
	}
	out := make([]string, 0, len(p.ExposedModels))
	for _, m := range p.Models {
		if want[m] {
			out = append(out, m)
		}
	}
	return out
}

// IsModelExposed 判断某个模型名是否对外暴露（口径同 Exposed）。
func (p *Provider) IsModelExposed(model string) bool {
	if p == nil {
		return false
	}
	for _, m := range p.Exposed() {
		if m == model {
			return true
		}
	}
	return false
}

// Group 是路由组：按顺序挑目标。
type Group struct {
	ID       string   `json:"id"`
	Name     string   `json:"name,omitempty"`
	Strategy string   `json:"strategy,omitempty"` // order / round-robin
	Targets  []string `json:"targets,omitempty"`  // provider/model
}

// Routing 路由开关。
type Routing struct {
	GroupsEnabled     bool `json:"groupsEnabled"`
	BareModelFallback bool `json:"bareModelFallback"`
}

// PromptConfig 是系统提示词注入配置。
//
//	mode=passthrough  原样转发（默认）
//	mode=inject       在客户端提示词前面插一段（压过它）
//	mode=replace      丢掉客户端自带的，只用自己的
type PromptConfig struct {
	Mode   string `json:"mode,omitempty"`
	File   string `json:"file,omitempty"`
	Inline string `json:"inline,omitempty"`
}

// ReasoningConfig 是思考强度（reasoning effort）配置。
//
//	mode=auto   客户端带了就用客户端的，没带用 effort
//	mode=force  无条件用 effort，覆盖客户端
type ReasoningConfig struct {
	Mode   string `json:"mode,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// SanitizeConfig 是「身份词屏蔽」配置。
//
// 为什么需要：WorkBuddy 的网关会扫请求里的文本，认出第三方 Agent 的
// 自我描述（比如「You are an AI agent powered by DeepSeek Harness」）后
// 用 400 / code 11-128 拒掉整条请求。把这类身份词换成中性说法，
// 就能在不改提示词含义的前提下降低被识别出来的概率。
//
// 默认开启、默认词表只含实测过的词 —— 不猜着扩表，避免把本来能过的
// 请求也改掉。改完即时生效，不需要重启。
type SanitizeConfig struct {
	// Enabled 为 nil 视为开启（新装即有保护）。
	Enabled *bool `json:"enabled,omitempty"`
	// Terms 是要屏蔽的身份词；为空时用 DefaultSanitizeTerms。
	Terms []string `json:"terms,omitempty"`
	// Replacement 是替换成什么；为空时用 DefaultSanitizeReplacement。
	Replacement string `json:"replacement,omitempty"`
}

// DefaultSanitizeTerms 是实测过的身份词。
//
// 只放真正会被上游当成「第三方 Agent 指纹」的词：
// 这两个都出现在 DSH 自己的系统提示词里，替换后语义不变。
var DefaultSanitizeTerms = []string{"DeepSeek Harness"}

// DefaultSanitizeReplacement 是默认替换词。
//
// 取下位通称而不是另一个产品名：再写一个品牌名只会成为下一个指纹。
const DefaultSanitizeReplacement = "the agent runtime"

// IsEnabled 返回开关状态，未配置时默认开启。
func (sc SanitizeConfig) IsEnabled() bool {
	return sc.Enabled == nil || *sc.Enabled
}

// EffectiveTerms 返回实际生效的词表（去掉空白项）。
func (sc SanitizeConfig) EffectiveTerms() []string {
	out := make([]string, 0, len(sc.Terms))
	for _, t := range sc.Terms {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		out = append(out, DefaultSanitizeTerms...)
	}
	return out
}

// EffectiveReplacement 返回实际生效的替换词。
func (sc SanitizeConfig) EffectiveReplacement() string {
	if r := strings.TrimSpace(sc.Replacement); r != "" {
		return r
	}
	return DefaultSanitizeReplacement
}

// ScheduleConfig 是自动任务的开关。
//
// 这些开关由模块的 service.sh 定时读取执行，改完即时生效（脚本每 30 秒检查一次）。
type ScheduleConfig struct {
	// AutoCheckin 是否自动跑 WorkBuddy 全套任务（签到/连登/出行/抽奖等）。
	// 用指针是为了区分「未配置」与「显式关闭」——未配置时默认开启。
	AutoCheckin *bool `json:"autoCheckin,omitempty"`
	// CheckinHours 是每天执行的小时（默认 9 点与 21 点）。
	CheckinHours []int `json:"checkinHours,omitempty"`
}

// AutoCheckinEnabled 返回开关状态，未配置时默认 true。
func (sc ScheduleConfig) AutoCheckinEnabled() bool {
	return sc.AutoCheckin == nil || *sc.AutoCheckin
}

// Hours 返回执行小时，默认 [9, 21]。
func (sc ScheduleConfig) Hours() []int {
	if len(sc.CheckinHours) == 0 {
		return []int{9, 21}
	}
	return sc.CheckinHours
}

// APIKey 是发给客户端的调用凭据（可与主网关口令并存）。
type APIKey struct {
	ID        string `json:"id"`
	Note      string `json:"note,omitempty"`
	Key       string `json:"key"`
	Enabled   *bool  `json:"enabled,omitempty"`
	CreatedAt int64  `json:"createdAt,omitempty"`
}

// IsEnabled 默认启用。
func (k *APIKey) IsEnabled() bool {
	return k == nil || k.Enabled == nil || *k.Enabled
}

// Config 是完整配置。
type Config struct {
	Server    Server      `json:"server"`
	Providers []*Provider `json:"providers"`
	Groups    []*Group    `json:"groups,omitempty"`
	APIKeys   []*APIKey   `json:"apiKeys,omitempty"`
	Routing   Routing     `json:"routing"`
	// Prompt 是网关级系统提示词注入配置（压过客户端自带的 system）
	Prompt PromptConfig `json:"prompt,omitempty"`
	// Sanitize 是上下文里的身份词屏蔽（防上游渠道校验 11-128）
	Sanitize SanitizeConfig `json:"sanitize,omitempty"`
	// Reasoning 是思考强度默认值
	Reasoning ReasoningConfig `json:"reasoning,omitempty"`
	// Schedule 是自动任务开关（由 service.sh 读取执行）
	Schedule ScheduleConfig `json:"schedule,omitempty"`
	// Setup 记录是否完成首次向导，供面板判断。
	Setup struct {
		Done   bool  `json:"done"`
		DoneAt int64 `json:"doneAt,omitempty"`
	} `json:"setup"`
}

// Store 管理配置文件的读写，所有写操作串行化。
type Store struct {
	mu   sync.RWMutex
	path string
	cfg  *Config
}

// NewStore 打开（必要时创建）配置文件。
func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cfg := &Config{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, cfg); err != nil {
			return fmt.Errorf("配置解析失败: %w", err)
		}
	}
	if cfg.Server.Port == 0 {
		// 首次安装：从 6868 往上找一个没人占的端口。
		//
		// 为什么不是 3425：Magpie 等同类网关默认占用它，装一起必冲突。
		// 为什么不是 6667：Chromium 内核有一份「受限端口」列表，6665-6669（IRC 段）
		// 在里面，浏览器访问会直接报 ERROR_PORT_BLOCKED —— 网关自己正常，
		// 但用户点「运行」跳浏览器就是一片错误页。6868 不在任何受限列表里。
		cfg.Server.Port = pickFreePort(6868)
	}
	if cfg.Server.Host == "" {
		// 默认只监听回环：这个服务能代理到付费账号，绝不该默认暴露到局域网
		cfg.Server.Host = "127.0.0.1"
	}
	firstRun := cfg.Server.AuthKey == ""
	if firstRun {
		cfg.Server.AuthKey = generateKey()
		cfg.Server.AuthKeyOrigin = "first-run-random"
	}
	s.cfg = cfg
	if len(raw) == 0 || firstRun {
		if err := s.saveLocked(); err != nil {
			return err
		}
	}
	return nil
}

// pickFreePort 从 prefer 开始往上找空闲端口，最多试 50 个。
// 找不到就返回 prefer（宁可冲突也让用户看到明确报错，而不是悄悄换个奇怪端口）。
// browserBlockedPorts 是 Chromium 内核拒绝访问的端口。
// 网关自己监听这些端口完全正常，但用户点「运行」用浏览器打开时会直接
// 报 ERROR_PORT_BLOCKED —— 表现就是「拉起来了但面板打不开」。
// 完整列表见 Chromium net/base/port_util.cc。
var browserBlockedPorts = map[int]bool{
	1: true, 7: true, 9: true, 11: true, 13: true, 15: true, 17: true, 19: true,
	20: true, 21: true, 22: true, 23: true, 25: true, 37: true, 42: true, 43: true,
	53: true, 69: true, 77: true, 79: true, 87: true, 95: true, 101: true, 102: true,
	103: true, 104: true, 109: true, 110: true, 111: true, 113: true, 115: true,
	117: true, 119: true, 123: true, 135: true, 137: true, 139: true, 143: true,
	161: true, 179: true, 389: true, 427: true, 465: true, 512: true, 513: true,
	514: true, 515: true, 526: true, 530: true, 531: true, 532: true, 540: true,
	548: true, 554: true, 556: true, 563: true, 587: true, 601: true, 636: true,
	989: true, 990: true, 993: true, 995: true, 1719: true, 1720: true, 1723: true,
	2049: true, 3659: true, 4045: true, 5060: true, 5061: true, 6000: true,
	6566: true, 6665: true, 6666: true, 6667: true, 6668: true, 6669: true,
	6679: true, 6697: true, 10080: true,
}

// IsBrowserBlockedPort 判断端口是否会被浏览器拒绝。
func IsBrowserBlockedPort(p int) bool { return browserBlockedPorts[p] }

// pickFreePort 从 prefer 开始找一个「既没被占用、浏览器也不拦」的端口。
//
// 找的时候向上扫 200 个，足够避开常见的连续占用段（比如同时装了两三个网关）。
// 每个候选都真去 bind 一次，确保不是「看起来空闲但被别的进程独占」。
func pickFreePort(prefer int) int {
	for port := prefer; port < prefer+200 && port < 65536; port++ {
		if browserBlockedPorts[port] {
			continue // 浏览器受限端口，用户打不开面板
		}
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			ln.Close()
			return port
		}
	}
	return prefer
}

// generateKey 生成 mp- 前缀的 32 字节随机口令。
func generateKey() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic("crypto/rand 不可用: " + err.Error())
	}
	return "mp-" + hex.EncodeToString(buf)
}

// Snapshot 返回一份深拷贝，调用方可以随便改。
func (s *Store) Snapshot() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	clone, _ := json.Marshal(s.cfg)
	out := &Config{}
	_ = json.Unmarshal(clone, out)
	return out
}

// Path 返回配置文件路径。
func (s *Store) Path() string { return s.path }

// Save 落盘（先写临时文件再 rename，避免半截文件）。
func (s *Store) Save(cfg *Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	raw, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// FindProvider 按 id 找供应商。
func (c *Config) FindProvider(id string) *Provider {
	for _, p := range c.Providers {
		if p != nil && p.ID == id {
			return p
		}
	}
	return nil
}

// FindGroup 按 id 找路由组。
func (c *Config) FindGroup(id string) *Group {
	for _, g := range c.Groups {
		if g != nil && g.ID == id {
			return g
		}
	}
	return nil
}

// SplitModel 把 "provider/model" 拆开。
func SplitModel(model string) (providerID, modelName string, ok bool) {
	i := strings.IndexByte(model, '/')
	if i <= 0 || i == len(model)-1 {
		return "", "", false
	}
	return model[:i], model[i+1:], true
}

// Redacted 返回脱敏副本，供管理接口展示。
func (c *Config) Redacted() *Config {
	clone := c
	out := *clone
	out.Server = clone.Server
	if out.Server.AuthKey != "" {
		out.Server.AuthKey = mask(out.Server.AuthKey)
	}
	out.Providers = make([]*Provider, 0, len(clone.Providers))
	for _, p := range clone.Providers {
		if p == nil {
			continue
		}
		cp := *p
		cp.APIKey = mask(cp.APIKey)
		cp.Accounts = make([]*Account, 0, len(p.Accounts))
		for _, a := range p.Accounts {
			if a == nil {
				continue
			}
			ca := *a
			ca.Access = mask(ca.Access)
			ca.Refresh = mask(ca.Refresh)
			cp.Accounts = append(cp.Accounts, &ca)
		}
		out.Providers = append(out.Providers, &cp)
	}
	return &out
}

func mask(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 10 {
		return "***"
	}
	return v[:6] + "..." + v[len(v)-4:]
}
