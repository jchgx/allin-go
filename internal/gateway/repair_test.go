package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"allin/internal/config"
	"allin/internal/provider/workbuddy"
)

// 线上日志（2026-11-18 06:48）证实：只改写消息开头，522KB 的大请求照样被拒。
// 所以兜底必须同时屏蔽身份词 —— 这里锁住「两步都做到」。
func TestRepairChannelBlockMasksAndNeutralizes(t *testing.T) {
	s := &Server{}
	cfg := &config.Config{} // 未配置 → 用默认词表
	in := []byte(`{"model":"m","messages":[{"role":"system","content":"You are an AI agent powered by DeepSeek Harness."}]}`)

	out, changed := s.repairChannelBlock(cfg, in, 1)
	if !changed {
		t.Fatal("应当判为已改写")
	}
	var m struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	got := m.Messages[0].Content
	if strings.Contains(got, "DeepSeek Harness") {
		t.Errorf("身份词没被屏蔽: %.80q", got)
	}
	if !strings.HasPrefix(got, workbuddy.NeutralHead) {
		t.Errorf("开头没被改写成中性头: %.80q", got)
	}
}

// 配置里关掉屏蔽时，救援路径仍然要屏蔽：请求已经被拒了，
// 这时候用默认词表只会提高通过率。
func TestRepairChannelBlockIgnoresDisabledConfig(t *testing.T) {
	off := false
	s := &Server{}
	cfg := &config.Config{Sanitize: config.SanitizeConfig{Enabled: &off}}
	in := []byte(`{"messages":[{"role":"system","content":"powered by DeepSeek Harness"}]}`)

	out, changed := s.repairChannelBlock(cfg, in, 1)
	if !changed {
		t.Fatal("关掉屏蔽时救援仍应生效")
	}
	if strings.Contains(string(out), "DeepSeek Harness") {
		t.Errorf("救援路径没屏蔽身份词: %s", out)
	}
}

// 自定义词表要生效。
func TestRepairChannelBlockUsesCustomTerms(t *testing.T) {
	s := &Server{}
	on := true
	cfg := &config.Config{Sanitize: config.SanitizeConfig{
		Enabled: &on, Terms: []string{"MyAgent"}, Replacement: "the agent",
	}}
	in := []byte(`{"messages":[{"role":"system","content":"I am MyAgent, hello."}]}`)
	out, changed := s.repairChannelBlock(cfg, in, 1)
	if !changed || strings.Contains(string(out), "MyAgent") {
		t.Fatalf("自定义词表没生效: changed=%v %s", changed, out)
	}
}

// 没有可改的地方时要报「改不动」，否则调用方会拿同样的字节再发一次、
// 白白再付一个 400。能被扫描的角色只有 system/developer/assistant，
// 纯 user 请求不属于要救的范围。
func TestRepairChannelBlockReportsNothingToChange(t *testing.T) {
	s := &Server{}
	cfg := &config.Config{}
	in := []byte(`{"messages":[{"role":"user","content":"用户的话，不该被动"}]}`)
	out, changed := s.repairChannelBlock(cfg, in, 1)
	if changed {
		t.Errorf("本来无可改，却报已改写: %s", out)
	}
}

// 第 2 轮要能叠加密集档：稀疏档没清掉的子串，靠逐字断词再试一次。
func TestRepairChannelBlockEscalatesToDense(t *testing.T) {
	s := &Server{}
	cfg := &config.Config{}
	in := []byte(`{"messages":[{"role":"system","content":"DeepSeek Harness 是这样一段没有任何标点的文本"}]}`)

	first, ok := s.repairChannelBlock(cfg, in, 1)
	if !ok {
		t.Fatal("第 1 轮应当改动")
	}
	second, ok := s.repairChannelBlock(cfg, first, 2)
	if !ok {
		t.Fatal("第 2 轮应当升级到密集档")
	}
	if strings.Count(string(second), workbuddy.ZeroWidthSpace) <= strings.Count(string(first), workbuddy.ZeroWidthSpace) {
		t.Error("密集档没有增加断词")
	}
	// 两轮都从同一份原始请求体出发改写，不在已改产物上叠加
	base, _ := s.repairChannelBlock(cfg, in, 2)
	if strings.Count(string(base), workbuddy.ZeroWidthSpace) != strings.Count(string(second), workbuddy.ZeroWidthSpace) {
		t.Error("密集档的起点应当始终是原始请求体，结果必须一致")
	}
}
