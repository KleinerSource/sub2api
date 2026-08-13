package service

import (
	"testing"
	"time"
)

// freshCodexUsageExtra 构造一份「新鲜、未重置」的 codex 用量快照，使 resolveOpenAIQuotaUtilization
// 返回 ok=true 且 used = used7dPercent/100。仅设置 7d 窗口字段。
func freshCodexUsageExtra(t *testing.T, used7dPercent float64, now time.Time) map[string]any {
	t.Helper()
	return map[string]any{
		"codex_7d_used_percent":        used7dPercent,
		"codex_7d_reset_after_seconds": 3600, // 1h 后重置 → 当前未重置
		"codex_usage_updated_at":       now.Format(time.RFC3339),
	}
}

func newCodexAccount(extra map[string]any) *Account {
	return &Account{
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    extra,
	}
}

func TestCodexLowQuotaModelGate_ConfigParsing(t *testing.T) {
	t.Run("nil extra returns zero", func(t *testing.T) {
		a := &Account{Platform: PlatformOpenAI}
		cfg := a.CodexLowQuotaModelGate()
		if cfg.Enabled || cfg.RemainingThreshold != 0 || len(cfg.AllowedModels) != 0 {
			t.Fatalf("expected zero config, got %+v", cfg)
		}
	})
	t.Run("missing key returns zero", func(t *testing.T) {
		a := newCodexAccount(map[string]any{"other": 1})
		if cfg := a.CodexLowQuotaModelGate(); cfg.Enabled {
			t.Fatalf("expected disabled, got %+v", cfg)
		}
	})
	t.Run("non-object value returns zero", func(t *testing.T) {
		a := newCodexAccount(map[string]any{codexLowQuotaModelGateExtraKey: "not-an-object"})
		if cfg := a.CodexLowQuotaModelGate(); cfg.Enabled {
			t.Fatalf("expected disabled for non-object, got %+v", cfg)
		}
	})
	t.Run("valid object parses with threshold clamped to [0,1]", func(t *testing.T) {
		a := newCodexAccount(map[string]any{
			codexLowQuotaModelGateExtraKey: map[string]any{
				"enabled":             true,
				"remaining_threshold": 0.2,
				"allowed_models":      []any{"gpt-5.6-luna", "gpt-5.6-terra"},
			},
		})
		cfg := a.CodexLowQuotaModelGate()
		if !cfg.Enabled {
			t.Fatal("expected enabled")
		}
		if cfg.RemainingThreshold != 0.2 {
			t.Fatalf("expected threshold 0.2, got %v", cfg.RemainingThreshold)
		}
		if len(cfg.AllowedModels) != 2 {
			t.Fatalf("expected 2 allowed models, got %v", cfg.AllowedModels)
		}
	})
	t.Run("threshold out of range is clamped", func(t *testing.T) {
		a := newCodexAccount(map[string]any{
			codexLowQuotaModelGateExtraKey: map[string]any{
				"enabled":             true,
				"remaining_threshold": 5.0, // >1 → clamp to 1
				"allowed_models":      []any{"gpt-5.6-luna"},
			},
		})
		if cfg := a.CodexLowQuotaModelGate(); cfg.RemainingThreshold != 1 {
			t.Fatalf("expected clamped to 1, got %v", cfg.RemainingThreshold)
		}
	})
}

func TestIsCodexLowQuotaModelRestricted_NotRestricted(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		account *Account
		model   string
	}{
		{
			name:    "non-openai account",
			account: &Account{Platform: PlatformAnthropic, Extra: map[string]any{}},
			model:   "gpt-5.6-sol",
		},
		{
			name: "empty requested model",
			account: newCodexAccount(map[string]any{
				codexLowQuotaModelGateExtraKey: map[string]any{
					"enabled": true, "remaining_threshold": 0.2, "allowed_models": []any{"gpt-5.6-luna"},
				},
			}),
			model: "",
		},
		{
			name: "gate disabled",
			account: newCodexAccount(mergeExtra(freshCodexUsageExtra(t, 85, now), map[string]any{
				codexLowQuotaModelGateExtraKey: map[string]any{
					"enabled": false, "remaining_threshold": 0.2, "allowed_models": []any{"gpt-5.6-luna"},
				},
			})),
			model: "gpt-5.6-sol",
		},
		{
			name: "no allowed models",
			account: newCodexAccount(mergeExtra(freshCodexUsageExtra(t, 85, now), map[string]any{
				codexLowQuotaModelGateExtraKey: map[string]any{
					"enabled": true, "remaining_threshold": 0.2, "allowed_models": []any{},
				},
			})),
			model: "gpt-5.6-sol",
		},
		{
			name: "zero threshold treated as disabled",
			account: newCodexAccount(mergeExtra(freshCodexUsageExtra(t, 85, now), map[string]any{
				codexLowQuotaModelGateExtraKey: map[string]any{
					"enabled": true, "remaining_threshold": 0, "allowed_models": []any{"gpt-5.6-luna"},
				},
			})),
			model: "gpt-5.6-sol",
		},
		{
			name: "no usage snapshot",
			account: newCodexAccount(map[string]any{
				codexLowQuotaModelGateExtraKey: map[string]any{
					"enabled": true, "remaining_threshold": 0.2, "allowed_models": []any{"gpt-5.6-luna"},
				},
			}),
			model: "gpt-5.6-sol",
		},
		{
			name: "quota sufficient (remaining > threshold)",
			account: newCodexAccount(mergeExtra(freshCodexUsageExtra(t, 50, now), map[string]any{
				codexLowQuotaModelGateExtraKey: map[string]any{
					"enabled": true, "remaining_threshold": 0.2, "allowed_models": []any{"gpt-5.6-luna"},
				},
			})),
			model: "gpt-5.6-sol",
		},
		{
			name: "low quota but requested model is allowed",
			account: newCodexAccount(mergeExtra(freshCodexUsageExtra(t, 85, now), map[string]any{
				codexLowQuotaModelGateExtraKey: map[string]any{
					"enabled": true, "remaining_threshold": 0.2, "allowed_models": []any{"gpt-5.6-luna", "gpt-5.6-terra"},
				},
			})),
			model: "gpt-5.6-luna",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restricted, reason := tc.account.IsCodexLowQuotaModelRestricted(tc.model, now)
			if restricted {
				t.Fatalf("expected not restricted, got restricted (reason=%q)", reason)
			}
		})
	}
}

func TestIsCodexLowQuotaModelRestricted_Restricted(t *testing.T) {
	now := time.Now()
	a := newCodexAccount(mergeExtra(freshCodexUsageExtra(t, 85, now), map[string]any{
		codexLowQuotaModelGateExtraKey: map[string]any{
			"enabled":             true,
			"remaining_threshold": 0.2, // 剩余 15% ≤ 20% → 限制态
			"allowed_models":      []any{"gpt-5.6-luna", "gpt-5.6-terra"},
		},
	}))
	restricted, reason := a.IsCodexLowQuotaModelRestricted("gpt-5.6-sol", now)
	if !restricted {
		t.Fatal("expected gpt-5.6-sol to be restricted when low on quota")
	}
	if reason != codexLowQuotaGateRestrictReason {
		t.Fatalf("expected reason %q, got %q", codexLowQuotaGateRestrictReason, reason)
	}
}

func TestIsCodexLowQuotaModelRestricted_StaleSnapshotNotRestricted(t *testing.T) {
	now := time.Now()
	// 快照写入时间为 3 小时前，超过 openAICodexAutoPauseStaleAfter(2h) → 视为过期，放行自愈。
	stale := now.Add(-3 * time.Hour)
	a := newCodexAccount(map[string]any{
		"codex_7d_used_percent":        85.0,
		"codex_7d_reset_after_seconds": 3600,
		"codex_usage_updated_at":       stale.Format(time.RFC3339),
		codexLowQuotaModelGateExtraKey: map[string]any{
			"enabled": true, "remaining_threshold": 0.2, "allowed_models": []any{"gpt-5.6-luna"},
		},
	})
	if restricted, _ := a.IsCodexLowQuotaModelRestricted("gpt-5.6-sol", now); restricted {
		t.Fatal("stale snapshot must not trigger restriction (self-heal)")
	}
}

func TestIsCodexLowQuotaModelRestricted_WindowResetNotRestricted(t *testing.T) {
	now := time.Now()
	// 绝对重置时间已过 → 窗口已重置 → 放行。
	a := newCodexAccount(map[string]any{
		"codex_7d_used_percent":  85.0,
		"codex_7d_reset_at":      now.Add(-1 * time.Hour).Format(time.RFC3339),
		"codex_usage_updated_at": now.Format(time.RFC3339),
		codexLowQuotaModelGateExtraKey: map[string]any{
			"enabled": true, "remaining_threshold": 0.2, "allowed_models": []any{"gpt-5.6-luna"},
		},
	})
	if restricted, _ := a.IsCodexLowQuotaModelRestricted("gpt-5.6-sol", now); restricted {
		t.Fatal("reset window must not trigger restriction")
	}
}

func TestIsCodexLowQuotaModelRestricted_AliasNormalization(t *testing.T) {
	now := time.Now()
	a := newCodexAccount(mergeExtra(freshCodexUsageExtra(t, 90, now), map[string]any{
		codexLowQuotaModelGateExtraKey: map[string]any{
			"enabled":             true,
			"remaining_threshold": 0.2,
			// 用规范名配置；请求侧用别名 gpt-5.6（归一化为 gpt-5.6-sol）应命中 sol，
			// 但 sol 不在允许列表 → 限制。改用 luna 别名场景验证命中放行。
			"allowed_models": []any{"gpt-5.6-luna"},
		},
	}))
	// gpt-5.6 → gpt-5.6-sol，不在允许列表 → 限制
	if restricted, _ := a.IsCodexLowQuotaModelRestricted("gpt-5.6", now); !restricted {
		t.Fatal("gpt-5.6 (alias of sol) should be restricted when only luna is allowed")
	}
	// luna 规范名/大小写变体都应命中 → 放行（注意：归一化器要求模型名含正确连字符）
	for _, requested := range []string{"gpt-5.6-luna", "gpt-5.6-LUNA"} {
		if restricted, _ := a.IsCodexLowQuotaModelRestricted(requested, now); restricted {
			t.Fatalf("requested %q should be allowed (matches luna)", requested)
		}
	}
}

func TestCodexLowQuotaGateModelAllowed(t *testing.T) {
	allowed := []string{"gpt-5.6-luna", "gpt-5.6-terra"}
	cases := []struct {
		requested string
		want      bool
	}{
		{"gpt-5.6-luna", true},
		{"gpt-5.6-Luna", true},   // 大小写不敏感
		{"gpt-5.6-terra", true},
		{"gpt-5.6-sol", false},
		{"gpt-5.5", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := codexLowQuotaGateModelAllowed(allowed, tc.requested); got != tc.want {
			t.Errorf("codexLowQuotaGateModelAllowed(%q) = %v, want %v", tc.requested, got, tc.want)
		}
	}
	// 空允许集合一律拒绝
	if codexLowQuotaGateModelAllowed(nil, "gpt-5.6-luna") {
		t.Error("empty allowed set must reject")
	}
}

func TestNormalizedAllowedCodexModels(t *testing.T) {
	cfg := CodexLowQuotaModelGateConfig{
		AllowedModels: []string{"gpt-5.6-luna", "GPT-5.6-LUNA", "gpt-5.6-terra", "  ", "unknown-model"},
	}
	got := cfg.NormalizedAllowedCodexModels()
	// 去重 + 排序；未知模型按原始小写保留
	want := []string{"gpt-5.6-luna", "gpt-5.6-terra", "unknown-model"}
	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, got)
		}
	}
}

// mergeExtra 合并多份 extra map（后者覆盖前者），便于测试组装「用量快照 + 门控配置」。
func mergeExtra(parts ...map[string]any) map[string]any {
	out := make(map[string]any)
	for _, p := range parts {
		for k, v := range p {
			out[k] = v
		}
	}
	return out
}
