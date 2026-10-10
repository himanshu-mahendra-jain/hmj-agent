package chat

import "testing"

func TestEffectiveCachedTokens(t *testing.T) {
	var pNil *PromptTokensDetails
	if pNil.EffectiveCachedTokens() != 0 {
		t.Fatal("expected 0 for nil")
	}
	p1 := &PromptTokensDetails{CachedTokens: 10}
	if p1.EffectiveCachedTokens() != 10 {
		t.Fatalf("expected 10, got %d", p1.EffectiveCachedTokens())
	}
	p2 := &PromptTokensDetails{CacheReadInputTokens: 20}
	if p2.EffectiveCachedTokens() != 20 {
		t.Fatalf("expected 20, got %d", p2.EffectiveCachedTokens())
	}
	p3 := &PromptTokensDetails{CachedPromptTokens: 30}
	if p3.EffectiveCachedTokens() != 30 {
		t.Fatalf("expected 30, got %d", p3.EffectiveCachedTokens())
	}

	var uNil *TokenUsage
	if uNil.EffectiveCachedTokens() != 0 {
		t.Fatal("expected 0 for nil")
	}
	u1 := &TokenUsage{CachedTokens: 40}
	if u1.EffectiveCachedTokens() != 40 {
		t.Fatalf("expected 40, got %d", u1.EffectiveCachedTokens())
	}
	u2 := &TokenUsage{CacheReadTokens: 50}
	if u2.EffectiveCachedTokens() != 50 {
		t.Fatalf("expected 50, got %d", u2.EffectiveCachedTokens())
	}
	u3 := &TokenUsage{InputTokensDetails: &PromptTokensDetails{CachedTokens: 60}}
	if u3.EffectiveCachedTokens() != 60 {
		t.Fatalf("expected 60, got %d", u3.EffectiveCachedTokens())
	}
	u4 := &TokenUsage{InputTokenDetails: &PromptTokensDetails{CachedTokens: 70}}
	if u4.EffectiveCachedTokens() != 70 {
		t.Fatalf("expected 70, got %d", u4.EffectiveCachedTokens())
	}
	u5 := &TokenUsage{PromptTokensDetails: &PromptTokensDetails{CachedTokens: 80}}
	if u5.EffectiveCachedTokens() != 80 {
		t.Fatalf("expected 80, got %d", u5.EffectiveCachedTokens())
	}
	u6 := &TokenUsage{PromptDetails: &PromptTokensDetails{CachedTokens: 90}}
	if u6.EffectiveCachedTokens() != 90 {
		t.Fatalf("expected 90, got %d", u6.EffectiveCachedTokens())
	}
	u7 := &TokenUsage{}
	if u7.EffectiveCachedTokens() != 0 {
		t.Fatal("expected 0 for empty TokenUsage")
	}
}
