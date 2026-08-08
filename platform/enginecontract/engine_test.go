package enginecontract

import "testing"

// Phase 0 報告 §4.3 の凍結表(Engine error 17種 → platform code 正規化)への
// 適合を検証する。request_cancelled は呼出側でクライアント切断として扱うため
// この表の対象外(orchestrator native.go 参照)。
func TestFrozenEngineErrorNormalization(t *testing.T) {
	tests := []struct {
		engineCode string
		want       string
	}{
		{CodeInvalidRequest, "platform_invalid_request"},
		{CodeContextLengthExceeded, "platform_invalid_request"},
		{CodeAuthenticationFailed, "internal_contract_error"},
		{CodeInternalError, "internal_contract_error"},
		{CodeInferenceFailed, "internal_contract_error"},
		{CodeStreamConsumerSlow, "internal_contract_error"},
		{CodeModelNotLoaded, "model_not_available"},
		{CodeUnsupportedModel, "model_not_available"},
		{CodeArtifactVerificationFailed, "model_not_available"},
		{CodeGPUUnhealthy, "model_not_available"},
		{CodeEngineDraining, "model_not_available"},
		{CodeResourceExhausted, "capacity_exhausted"},
		{CodeCapacityExhausted, "capacity_exhausted"},
		{CodeGPUOOM, "capacity_exhausted"},
		{CodeDeadlineRejected, "inference_timeout"},
		{CodeDeadlineExceeded, "inference_timeout"},
		// 未知 code は internal_contract_error(実装済み能力のみ公開の原則)
		{"unknown_future_code", "internal_contract_error"},
	}
	for _, tt := range tests {
		if got := PlatformCode(tt.engineCode); got != tt.want {
			t.Errorf("PlatformCode(%s) = %s, want %s", tt.engineCode, got, tt.want)
		}
	}
}
