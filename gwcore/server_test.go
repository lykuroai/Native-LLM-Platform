package gwcore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockRuntime is an OpenAI-compatible upstream that records what it receives.
type mockRuntime struct {
	*httptest.Server
	lastBody map[string]any
}

func newMockRuntime(t *testing.T) *mockRuntime {
	t.Helper()
	m := &mockRuntime{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.lastBody = map[string]any{}
		_ = json.Unmarshal(body, &m.lastBody)
		if stream, _ := m.lastBody["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"こん\"}}]}\n\n")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"にちは\"}}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":4}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"cmpl-1","model":%q,"choices":[{"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			m.lastBody["model"])
	})
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.lastBody = map[string]any{}
		_ = json.Unmarshal(body, &m.lastBody)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":7,"completion_tokens":0}}`)
	})
	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Server.Close)
	return m
}

// newTestServer wires a gateway against the mock runtime and returns
// (server httptest URL, plaintext key, audit file path).
func newTestServer(t *testing.T, runtime *mockRuntime) (string, string, string) {
	t.Helper()
	plain, hash, err := GenerateVirtualKey()
	require.NoError(t, err)

	cfg, err := ParseConfig([]byte(fmt.Sprintf(`
schema_version: "1"
gateway:
  id: gw-test
  allowed_data_classes: [public, internal, confidential, restricted]
auth:
  virtual_keys:
    - id: vk-1
      name: test
      key_hash: %s
      allowed_models: [company-llm-safe, company-embed]
      rpm_limit: 100
    - id: vk-restricted
      name: restricted
      key_hash: %s
      allowed_models: [company-embed]
models:
  - logical_name: company-llm-safe
    runtime: vllm
    endpoint: %q
    physical_model: qwen3-32b
  - logical_name: company-embed
    runtime: openai_compatible
    endpoint: %q
    physical_model: bge-m3
`, hash, HashKey("lkpgw_restrictedkeyrestrictedkeyrestrictedkey12345"), runtime.URL, runtime.URL)))
	require.NoError(t, err)

	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := NewAuditLogger(auditPath)
	require.NoError(t, err)
	t.Cleanup(func() { audit.Close() })

	srv := httptest.NewServer(NewServer(cfg, audit).Router())
	t.Cleanup(srv.Close)
	return srv.URL, plain, auditPath
}

func doJSON(t *testing.T, method, url, key string, body any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, reader)
	require.NoError(t, err)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	resp.Body.Close()
	return resp, data
}

func readAudit(t *testing.T, path string) []AuditRecord {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var out []AuditRecord
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var r AuditRecord
		require.NoError(t, json.Unmarshal([]byte(line), &r))
		out = append(out, r)
	}
	return out
}

func TestChatCompletionProxy(t *testing.T) {
	runtime := newMockRuntime(t)
	url, key, auditPath := newTestServer(t, runtime)

	resp, body := doJSON(t, "POST", url+"/v1/chat/completions", key, map[string]any{
		"model":    "company-llm-safe",
		"messages": []map[string]string{{"role": "user", "content": "秘密の質問"}},
	}, map[string]string{"X-Data-Class": "confidential"})

	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	// 上流には physical model が渡る(logical→physical 書換)
	assert.Equal(t, "qwen3-32b", runtime.lastBody["model"])
	// クライアント応答は上流を透過
	assert.Contains(t, string(body), `"model":"qwen3-32b"`)
	assert.NotEmpty(t, resp.Header.Get("X-Request-ID"))

	// 監査: 本文なし・トークン数あり
	records := readAudit(t, auditPath)
	require.Len(t, records, 1)
	r := records[0]
	assert.Equal(t, "success", r.Result)
	assert.Equal(t, "company-llm-safe", r.LogicalModel)
	assert.Equal(t, "qwen3-32b", r.PhysicalModel)
	assert.Equal(t, "confidential", r.DataClass)
	require.NotNil(t, r.InputTokens)
	assert.Equal(t, int64(10), *r.InputTokens)
	assert.False(t, r.ContentLogged)
	// 監査ファイルに本文・鍵が含まれない(Zero-Retention)
	rawAudit, _ := os.ReadFile(auditPath)
	assert.NotContains(t, string(rawAudit), "秘密の質問")
	assert.NotContains(t, string(rawAudit), key)
}

func TestChatCompletionStreaming(t *testing.T) {
	runtime := newMockRuntime(t)
	url, key, auditPath := newTestServer(t, runtime)

	resp, body := doJSON(t, "POST", url+"/v1/chat/completions", key, map[string]any{
		"model":  "company-llm-safe",
		"stream": true,
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/event-stream")
	assert.Contains(t, string(body), "data: [DONE]")
	assert.Contains(t, string(body), "にちは")

	// SSE 中の usage を覗き見て監査へ記録
	records := readAudit(t, auditPath)
	require.Len(t, records, 1)
	require.NotNil(t, records[0].InputTokens)
	assert.Equal(t, int64(12), *records[0].InputTokens)
	assert.Equal(t, int64(4), *records[0].OutputTokens)
}

func TestEmbeddingsProxy(t *testing.T) {
	runtime := newMockRuntime(t)
	url, key, _ := newTestServer(t, runtime)
	resp, body := doJSON(t, "POST", url+"/v1/embeddings", key, map[string]any{
		"model": "company-embed",
		"input": "テキスト",
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "bge-m3", runtime.lastBody["model"])
	assert.Contains(t, string(body), "embedding")
}

func TestListModels(t *testing.T) {
	runtime := newMockRuntime(t)
	url, key, _ := newTestServer(t, runtime)
	resp, body := doJSON(t, "GET", url+"/v1/models", key, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	// logical のみ公開、physical 名は返さない(BD §9.5)
	assert.Contains(t, string(body), "company-llm-safe")
	assert.NotContains(t, string(body), "qwen3-32b")
}

func TestAuthFailures(t *testing.T) {
	runtime := newMockRuntime(t)
	url, _, _ := newTestServer(t, runtime)

	resp, body := doJSON(t, "GET", url+"/v1/models", "", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, string(body), "authentication_failed")

	resp, _ = doJSON(t, "GET", url+"/v1/models", "lkpgw_wrongkeywrongkeywrongkeywrongkey123456", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// SaaS のキー形式(lk_)は形式段階で拒否
	resp, _ = doJSON(t, "GET", url+"/v1/models", "lk_live_notforthisgateway", nil, nil)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestPolicyDenials(t *testing.T) {
	runtime := newMockRuntime(t)
	url, key, _ := newTestServer(t, runtime)

	// キーに許可されていないモデル
	restrictedKey := "lkpgw_restrictedkeyrestrictedkeyrestrictedkey12345"
	resp, body := doJSON(t, "POST", url+"/v1/chat/completions", restrictedKey, map[string]any{
		"model": "company-llm-safe",
	}, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "policy_denied")

	// 未知のモデル
	resp, _ = doJSON(t, "POST", url+"/v1/chat/completions", key, map[string]any{
		"model": "gpt-4o",
	}, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)

	// hybrid 要求は Strict Local で拒否
	resp, body = doJSON(t, "POST", url+"/v1/chat/completions", key, map[string]any{
		"model": "company-llm-safe",
	}, map[string]string{"X-Routing-Mode": "hybrid"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "strict local")

	// model 欠落
	resp, _ = doJSON(t, "POST", url+"/v1/chat/completions", key, map[string]any{}, nil)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestDataClassPolicy(t *testing.T) {
	runtime := newMockRuntime(t)
	// allowed_data_classes を public のみに絞った構成
	plain, hash, err := GenerateVirtualKey()
	require.NoError(t, err)
	cfg, err := ParseConfig([]byte(fmt.Sprintf(`
schema_version: "1"
gateway:
  id: gw-public-only
  allowed_data_classes: [public]
auth:
  virtual_keys:
    - id: vk-1
      name: t
      key_hash: %s
models:
  - logical_name: m
    runtime: vllm
    endpoint: %q
    physical_model: pm
`, hash, runtime.URL)))
	require.NoError(t, err)
	audit, err := NewAuditLogger("")
	require.NoError(t, err)
	srv := httptest.NewServer(NewServer(cfg, audit).Router())
	t.Cleanup(srv.Close)

	resp, body := doJSON(t, "POST", srv.URL+"/v1/chat/completions", plain, map[string]any{
		"model": "m",
	}, map[string]string{"X-Data-Class": "confidential"})
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "policy_denied")
}

func TestRateLimitExceeded(t *testing.T) {
	runtime := newMockRuntime(t)
	plain, hash, err := GenerateVirtualKey()
	require.NoError(t, err)
	cfg, err := ParseConfig([]byte(fmt.Sprintf(`
schema_version: "1"
gateway:
  id: gw-rl
  allowed_data_classes: [public]
auth:
  virtual_keys:
    - id: vk-1
      name: t
      key_hash: %s
      rpm_limit: 2
models:
  - logical_name: m
    runtime: vllm
    endpoint: %q
    physical_model: pm
`, hash, runtime.URL)))
	require.NoError(t, err)
	audit, err := NewAuditLogger("")
	require.NoError(t, err)
	srv := httptest.NewServer(NewServer(cfg, audit).Router())
	t.Cleanup(srv.Close)

	for i := 0; i < 2; i++ {
		resp, _ := doJSON(t, "GET", srv.URL+"/v1/models", plain, nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
	resp, body := doJSON(t, "GET", srv.URL+"/v1/models", plain, nil, nil)
	assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	assert.Contains(t, string(body), "rate_limit_exceeded")
	assert.Equal(t, "60", resp.Header.Get("Retry-After"))
}

func TestRuntimeUnavailable(t *testing.T) {
	runtime := newMockRuntime(t)
	url, key, auditPath := newTestServer(t, runtime)
	runtime.Close() // 上流停止 → 503

	resp, body := doJSON(t, "POST", url+"/v1/chat/completions", key, map[string]any{
		"model": "company-llm-safe",
	}, nil)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, string(body), "model_unavailable")

	records := readAudit(t, auditPath)
	require.Len(t, records, 1)
	assert.Equal(t, "model_unavailable", records[0].Result)
}

func TestHealthEndpoints(t *testing.T) {
	runtime := newMockRuntime(t)
	url, _, _ := newTestServer(t, runtime)

	resp, _ := doJSON(t, "GET", url+"/healthz", "", nil, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	resp, body := doJSON(t, "GET", url+"/readyz", "", nil, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), `"ready":true`)

	resp, body = doJSON(t, "GET", url+"/version", "", nil, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), Version)

	// runtime 停止で readyz は 503
	runtime.Close()
	resp, _ = doJSON(t, "GET", url+"/readyz", "", nil, nil)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}
