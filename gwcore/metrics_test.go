package gwcore

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func scrape(t *testing.T, srv *Server) string {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Metrics().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

// ADD §17.1 のメトリクスが推論・拒否・ストリームの各経路で集計されること。
func TestMetricsEndpoint(t *testing.T) {
	rt := newFakeRuntime(t, "a")
	srv, key := newPlatformServer(t, false, rt)
	h := srv.Router()

	// 成功(非stream)
	rec := doChat(t, h, key,
		`{"model":"qwen-local","messages":[{"role":"user","content":"hi"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	// 成功(stream → TTFT / active_streams)
	rec = doChat(t, h, key,
		`{"model":"qwen-local","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	// 認証失敗
	rec = doChat(t, h, "lkpgw_wrong",
		`{"model":"qwen-local","messages":[]}`, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	// ポリシー拒否(未許可 data class)
	rec = doChat(t, h, key,
		`{"model":"qwen-local","messages":[]}`,
		map[string]string{"X-Data-Class": "restricted"})
	require.Equal(t, http.StatusForbidden, rec.Code)

	body := scrape(t, srv)
	assert.Contains(t, body, `lykuro_pgw_requests_total{endpoint="/v1/chat/completions",status="200"} 2`)
	assert.Contains(t, body, `lykuro_pgw_requests_total{endpoint="/v1/chat/completions",status="401"} 1`)
	assert.Contains(t, body, `lykuro_pgw_requests_total{endpoint="/v1/chat/completions",status="403"} 1`)
	assert.Contains(t, body, `lykuro_pgw_auth_failures_total 1`)
	assert.Contains(t, body, `lykuro_pgw_policy_denied_total 1`)
	assert.Contains(t, body, `lykuro_pgw_input_tokens_total{model="qwen-local"}`)
	assert.Contains(t, body, `lykuro_pgw_output_tokens_total{model="qwen-local"}`)
	assert.Contains(t, body, `lykuro_pgw_first_token_latency_seconds_count 1`)
	assert.Contains(t, body, `lykuro_pgw_active_streams 0`) // 終了後は0へ戻る
	assert.Contains(t, body, `lykuro_pgw_request_duration_seconds_count{endpoint="/v1/chat/completions"} 2`)
	assert.Contains(t, body, `lykuro_pgw_build_info{version="`+Version+`"} 1`)
}

// BD §19.3: label に request_id・Virtual Key・prompt 断片を載せない。
func TestMetricsNoHighCardinalityLabels(t *testing.T) {
	rt := newFakeRuntime(t, "a")
	srv, key := newPlatformServer(t, false, rt)
	h := srv.Router()
	secret := "SECRET-PROMPT-FRAGMENT"
	rec := doChat(t, h, key,
		`{"model":"qwen-local","messages":[{"role":"user","content":"`+secret+`"}]}`,
		map[string]string{"X-Request-ID": "req-high-cardinality-1"})
	require.Equal(t, http.StatusOK, rec.Code)

	body := scrape(t, srv)
	assert.NotContains(t, body, secret)
	assert.NotContains(t, body, "req-high-cardinality-1")
	assert.NotContains(t, body, key)
	assert.NotContains(t, body, "vk_1") // Virtual Key ID も label にしない
}

// 全滅時の 5xx は model_errors_total{code} に計上される。
func TestMetricsModelErrors(t *testing.T) {
	rt := newFakeRuntime(t, "a")
	srv, key := newPlatformServer(t, false, rt)
	rt.fail.Store(true)
	rec := doChat(t, srv.Router(), key,
		`{"model":"qwen-local","messages":[{"role":"user","content":"hi"}]}`, nil)
	require.GreaterOrEqual(t, rec.Code, 500)

	body := scrape(t, srv)
	assert.Contains(t, body, "lykuro_pgw_model_errors_total{code=")
}

// Content-Type ヘッダの確認(Prometheus text format v0.0.4)。
func TestMetricsContentType(t *testing.T) {
	rt := newFakeRuntime(t, "a")
	srv, _ := newPlatformServer(t, false, rt)
	rec := httptest.NewRecorder()
	srv.Metrics().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Equal(t, "text/plain; version=0.0.4; charset=utf-8",
		rec.Header().Get("Content-Type"))
}
