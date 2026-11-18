package gateway

import (
	"net/http"
	"strings"

	"allin/internal/config"
)

// handleSanitize 读写「身份词屏蔽」配置。
//
//	GET  /api/sanitize  → {enabled, terms, replacement, defaults}
//	POST /api/sanitize  {enabled, terms, replacement}  保存（即时生效）
//
// 这项功能的由来：上游按「请求像不像官方客户端」判定，认出第三方 Agent 的
// 自我描述就用 400/11-128 拒掉整条请求。把身份词换成中性说法能降低被认出的
// 概率，但它不是万能药 —— 上游的判定带自适应成分，同内容不同时刻结果可能不同，
// 所以网关同时保留了「改写开头后重发」的兜底。面板上如实这么写。
func (s *Server) handleSanitize(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Snapshot()

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"enabled":     cfg.Sanitize.IsEnabled(),
			"terms":       cfg.Sanitize.EffectiveTerms(),
			"replacement": cfg.Sanitize.EffectiveReplacement(),
			"defaults": map[string]any{
				"terms":       config.DefaultSanitizeTerms,
				"replacement": config.DefaultSanitizeReplacement,
			},
		})
		return

	case http.MethodPost:
		var body struct {
			Enabled     *bool    `json:"enabled"`
			Terms       []string `json:"terms"`
			Replacement string   `json:"replacement"`
		}
		if err := decodeJSON(r, &body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "JSON 解析失败: " + err.Error()})
			return
		}
		// 词表支持两种写法：数组，或一行一个/逗号分隔的字符串（面板传数组）。
		terms := make([]string, 0, len(body.Terms))
		for _, t := range body.Terms {
			for _, part := range strings.FieldsFunc(t, func(r rune) bool { return r == '\n' || r == ',' }) {
				if p := strings.TrimSpace(part); p != "" {
					terms = append(terms, p)
				}
			}
		}
		cfg.Sanitize = config.SanitizeConfig{
			Enabled:     body.Enabled,
			Terms:       terms,
			Replacement: strings.TrimSpace(body.Replacement),
		}
		if err := s.store.Save(cfg); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "保存失败: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          true,
			"enabled":     cfg.Sanitize.IsEnabled(),
			"terms":       cfg.Sanitize.EffectiveTerms(),
			"replacement": cfg.Sanitize.EffectiveReplacement(),
			"hint":        "已保存，下一个请求立即生效（无需重启）",
		})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 GET/POST"})
}
