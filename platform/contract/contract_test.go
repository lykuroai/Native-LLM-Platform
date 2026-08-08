package contract

import "testing"

// Phase 0 報告 §4.2 の凍結表(platform code → HTTP status / gwcore 外部 code)
// への適合を検証する。表の変更は contract 改版(gateway-platform-v2)を要する。
func TestFrozenErrorMapping(t *testing.T) {
	tests := []struct {
		code       string
		wantStatus int
		wantGWCode string
	}{
		{ErrInvalidRequest, 400, "invalid_request"},
		{ErrConversationConflict, 409, "conversation_conflict"},
		{ErrModelNotAllowed, 403, "policy_denied"},
		{ErrModelNotAvailable, 503, "model_unavailable"},
		{ErrCapacityExhausted, 429, "rate_limit_exceeded"},
		{ErrInferenceTimeout, 504, "inference_timeout"},
		{ErrPlatformNotReady, 503, "model_unavailable"},
		{ErrInternalContract, 502, "server_error"},
		// 未知 code は 502 server_error に丸める(契約外 code を外部へ漏らさない)
		{"some_future_code", 502, "server_error"},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			if got := HTTPStatus(tt.code); got != tt.wantStatus {
				t.Errorf("HTTPStatus(%s) = %d, want %d", tt.code, got, tt.wantStatus)
			}
			if got := GatewayErrorCode(tt.code); got != tt.wantGWCode {
				t.Errorf("GatewayErrorCode(%s) = %s, want %s", tt.code, got, tt.wantGWCode)
			}
		})
	}
}

func TestContractVersionFrozen(t *testing.T) {
	if Version != "gateway-platform-v1" {
		t.Errorf("contract version = %s — 変更は契約改版手続きが必要", Version)
	}
}
