package main

import (
	"testing"

	"github.com/lykuroai/Native-LLM-Platform/gwcore"
)

func TestSanitizeLogicalName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"llama3:8b", "llama3-8b"},
		{"meta-llama/Llama-3-8B", "meta-llama-llama-3-8b"},
		{"Qwen3 4B Instruct", "qwen3-4b-instruct"},
		{"model.v1_x", "model.v1_x"},
		{":::", "model"},
	}
	for _, tc := range cases {
		if got := sanitizeLogicalName(tc.in); got != tc.want {
			t.Errorf("sanitizeLogicalName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBuildInitConfig(t *testing.T) {
	cands := []gwcore.RuntimeCandidate{
		{Endpoint: "http://127.0.0.1:11434/", Runtime: "ollama", Models: []string{"llama3:8b", "qwen3:4b"}},
		{Endpoint: "http://127.0.0.1:8000", Runtime: "vllm", Models: []string{"llama3:8b"}}, // logical 名が衝突
		{Endpoint: "http://127.0.0.1:8080", Runtime: "tgi", Models: nil},                    // モデル報告なし → 不採用
	}
	cfg := buildInitConfig("gw-test", cands)

	if len(cfg.Models) != 3 {
		t.Fatalf("models = %d, want 3", len(cfg.Models))
	}
	if cfg.Models[0].LogicalName != "llama3-8b" || cfg.Models[0].Endpoint != "http://127.0.0.1:11434" {
		t.Errorf("first model unexpected: %+v (末尾スラッシュ除去も確認)", cfg.Models[0])
	}
	if cfg.Models[2].LogicalName != "llama3-8b-2" {
		t.Errorf("collision suffix missing: %q", cfg.Models[2].LogicalName)
	}

	// 生成された設定は Virtual Key を足せばそのまま検証を通ること
	_, hash, err := gwcore.GenerateVirtualKey()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.VirtualKeys = []gwcore.VirtualKeyDef{{ID: "vk-default", KeyHash: hash}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("generated config invalid: %v", err)
	}
}
