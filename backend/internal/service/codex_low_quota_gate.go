package service

import (
	"sort"
	"strings"
	"time"
)

// codexLowQuotaModelGateExtraKey 是 account.extra 中存放「低额度模型限制门」配置的 key。
// 配置以嵌套 JSON 对象形式存储，无需 ent schema 变更或数据库迁移（与 auto_pause_*_threshold 同样落在 extra JSONB）。
const codexLowQuotaModelGateExtraKey = "codex_low_quota_model_gate"

// CodexLowQuotaModelGateConfig 描述单个 codex 账号的「低额度模型限制门」配置。
//
// 设计意图：当账号 7d 长周期剩余额度紧张时，把它限制为只服务一组「轻量」模型
// （例如仅允许 luna / terra），把稀缺配额留给更便宜的模型，昂贵的模型（如 sol）
// 自动路由到其他账号。若所有账号都被限制，则对外表现为 503 容量错误（模型在配置层面
// 仍受支持，仅是当前账号暂时受限），而非 404 model_not_found。
type CodexLowQuotaModelGateConfig struct {
	// Enabled 是否启用该门控。未启用时一律放行。
	Enabled bool `json:"enabled"`
	// RemainingThreshold 触发阈值，以「剩余额度分数」表示（0-1）。
	// 当 7d 剩余额度 (1 - used) <= 该阈值时进入限制态。例如 0.2 表示剩余 ≤ 20% 触发。
	RemainingThreshold float64 `json:"remaining_threshold"`
	// AllowedModels 进入限制态后仍允许服务的模型集合（归一化比较，支持别名）。
	AllowedModels []string `json:"allowed_models"`
}

// codexLowQuotaGateRestrictReason 是该门控命中时返回给调度过滤统计的拒绝原因，
// 会自动出现在 "no available accounts" 错误摘要中（见 openAISelectionFilterStats）。
const codexLowQuotaGateRestrictReason = "codex_low_quota_model_restricted"

// CodexLowQuotaModelGate 从 account.extra 解析「低额度模型限制门」配置。
// 缺失、非对象或解析失败时返回零值（Enabled=false）。非 OpenAI 账号也返回零值——
// 该特性仅对存在 codex 用量信号的 OpenAI 账号有意义。
func (a *Account) CodexLowQuotaModelGate() CodexLowQuotaModelGateConfig {
	var zero CodexLowQuotaModelGateConfig
	if a == nil || a.Extra == nil {
		return zero
	}
	raw, ok := a.Extra[codexLowQuotaModelGateExtraKey]
	if !ok || raw == nil {
		return zero
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return zero
	}
	cfg := CodexLowQuotaModelGateConfig{
		Enabled:            resolveAccountExtraBool(obj, "enabled"),
		RemainingThreshold: clamp01(parseExtraFloat64(obj["remaining_threshold"])),
		AllowedModels:      parseExtraStringSlice(obj["allowed_models"]),
	}
	return cfg
}

// IsCodexLowQuotaModelRestricted 报告该账号是否因「7d 低额度」而对 requestedModel 触发模型限制。
// 返回 (false, "") 表示放行（不限制）；(true, codexLowQuotaGateRestrictReason) 表示该账号
// 在当前用量下不应服务该模型，调度器应跳过它。
//
// 判定顺序刻意保守：任何缺失信号（无快照 / 快照过期 / 窗口已重置）都不限制，
// 复用 resolveOpenAIQuotaUtilization 的三重保护，避免在过期数据上误伤账号。
func (a *Account) IsCodexLowQuotaModelRestricted(requestedModel string, now time.Time) (bool, string) {
	if a == nil || !a.IsOpenAI() {
		return false, ""
	}
	// 空模型（例如 /models 清单拉取）不参与模型级限制。
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return false, ""
	}
	gate := a.CodexLowQuotaModelGate()
	if !gate.Enabled || len(gate.AllowedModels) == 0 || gate.RemainingThreshold <= 0 {
		return false, ""
	}
	// 复用既有用量解析：ok=false 涵盖「无快照 / 快照过期 / 窗口已重置」三种情况，均放行。
	used, ok := resolveOpenAIQuotaUtilization(a.Extra, "7d", now)
	if !ok || used < 0 {
		return false, ""
	}
	remaining := 1 - used
	if remaining > 1 {
		remaining = 1
	}
	// 配额仍充足：不限制。
	if remaining > gate.RemainingThreshold {
		return false, ""
	}
	// 进入限制态：仅当请求模型不在允许集合时才拒绝。归一化匹配以容忍别名（gpt-5.6 → gpt-5.6-sol）。
	if codexLowQuotaGateModelAllowed(gate.AllowedModels, requestedModel) {
		return false, ""
	}
	return true, codexLowQuotaGateRestrictReason
}

// codexLowQuotaGateModelAllowed 判断 requestedModel（经 codex 模型归一化后）是否落在 allowed 集合中。
// allowed 中的每个条目同样做归一化，因此配置既可写规范名（gpt-5.6-luna）也可写别名（gpt-5.6）。
// 无法归一化的请求模型（非 codex/gpt-5 系）按原始值做大小写不敏感比较，保持与允许列表手动录入值的一致性。
func codexLowQuotaGateModelAllowed(allowed []string, requested string) bool {
	if len(allowed) == 0 || requested == "" {
		return false
	}
	normalizedRequested := normalizeKnownOpenAICodexModel(requested)
	if normalizedRequested == "" {
		normalizedRequested = strings.ToLower(strings.TrimSpace(requested))
	}
	for _, raw := range allowed {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		normalized := normalizeKnownOpenAICodexModel(entry)
		if normalized == "" {
			normalized = strings.ToLower(entry)
		}
		if normalized != "" && normalized == normalizedRequested {
			return true
		}
	}
	return false
}

// parseExtraStringSlice 从 extra 字段的 any 值解析 []string，容忍 JSON 反序列化产生的 []any。
func parseExtraStringSlice(value any) []string {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case []string:
		out := make([]string, 0, len(v))
		for _, s := range v {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				if trimmed := strings.TrimSpace(s); trimmed != "" {
					out = append(out, trimmed)
				}
			}
		}
		return out
	}
	return nil
}

// NormalizedAllowedCodexModels 返回归一化后的允许模型集合（去重、排序），仅供展示/调试使用。
func (c CodexLowQuotaModelGateConfig) NormalizedAllowedCodexModels() []string {
	seen := make(map[string]struct{}, len(c.AllowedModels))
	out := make([]string, 0, len(c.AllowedModels))
	for _, raw := range c.AllowedModels {
		normalized := normalizeKnownOpenAICodexModel(strings.TrimSpace(raw))
		if normalized == "" {
			normalized = strings.ToLower(strings.TrimSpace(raw))
		}
		if normalized == "" {
			continue
		}
		if _, dup := seen[normalized]; dup {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out
}
