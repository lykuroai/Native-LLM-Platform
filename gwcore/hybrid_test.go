package gwcore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingRuntime records hits and can be told to fail.
type countingRuntime struct {
	*httptest.Server
	hits      atomic.Int64
	status    atomic.Int64 // 0 = 200
	lastModel atomic.Value
	lastAuth  atomic.Value
}

func newCountingRuntime(t *testing.T) *countingRuntime {
	t.Helper()
	rt := &countingRuntime{}
	mux := http.NewServeMux()
	handler := func(w http.ResponseWriter, r *http.Request) {
		rt.hits.Add(1)
		rt.lastAuth.Store(r.Header.Get("Authorization"))
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		if m, ok := p["model"].(string); ok {
			rt.lastModel.Store(m)
		}
		if s := rt.status.Load(); s != 0 {
			w.WriteHeader(int(s))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2}}`)
	}
	mux.HandleFunc("/v1/chat/completions", handler)
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		rt.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"response","usage":{"input_tokens":11,"output_tokens":22}}`)
	})
	mux.HandleFunc("/v1/rerank", func(w http.ResponseWriter, r *http.Request) {
		rt.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"results":[{"index":0,"relevance_score":0.9}]}`)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	rt.Server = httptest.NewServer(mux)
	t.Cleanup(rt.Server.Close)
	return rt
}

// hybridFixture builds a gateway with primary+fallback local runtimes and an
// approved cloud endpoint.
func hybridFixture(t *testing.T, strictLocal bool, allowTools *bool) (url, key string, primary, fallback, cloud *countingRuntime, auditPath string) {
	t.Helper()
	primary = newCountingRuntime(t)
	fallback = newCountingRuntime(t)
	cloud = newCountingRuntime(t)

	plain, hash, err := GenerateVirtualKey()
	require.NoError(t, err)

	keyFile := filepath.Join(t.TempDir(), "cloud-key")
	require.NoError(t, os.WriteFile(keyFile, []byte("lk_live_cloudkey\n"), 0o600))

	toolsLine := ""
	if allowTools != nil {
		toolsLine = fmt.Sprintf("      allow_tools: %v\n", *allowTools)
	}
	hybridSection := ""
	if !strictLocal {
		hybridSection = fmt.Sprintf(`
hybrid:
  enabled: true
  endpoint: %q
  api_key_file: %q
  allowed_data_classes: [public, internal]
`, cloud.URL, keyFile)
	}
	cfgYAML := fmt.Sprintf(`
schema_version: "1"
gateway:
  id: gw-hybrid
  strict_local_mode: %v
  allowed_data_classes: [public, internal, confidential, restricted]
auth:
  virtual_keys:
    - id: vk-1
      name: t
      key_hash: %s
%s
models:
  - logical_name: company-llm
    runtime: vllm
    endpoint: %q
    physical_model: local-primary
    fallback:
      - runtime: ollama
        endpoint: %q
        physical_model: local-fallback
%s
`, strictLocal, hash, toolsLine, primary.URL, fallback.URL,
		func() string {
			if strictLocal {
				return ""
			}
			return `    cloud_fallback:
      model: deepseek/deepseek-chat`
		}())
	cfgYAML += hybridSection

	cfg, err := ParseConfig([]byte(cfgYAML))
	require.NoError(t, err)

	auditPath = filepath.Join(t.TempDir(), "audit.jsonl")
	audit, err := NewAuditLogger(auditPath)
	require.NoError(t, err)
	t.Cleanup(func() { audit.Close() })
	srv := httptest.NewServer(NewServer(cfg, audit).Router())
	t.Cleanup(srv.Close)
	return srv.URL, plain, primary, fallback, cloud, auditPath
}

func chatReq(t *testing.T, url, key string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	return doJSON(t, "POST", url+"/v1/chat/completions", key, map[string]any{
		"model":    "company-llm",
		"messages": []map[string]string{{"role": "user", "content": "機密の質問"}},
	}, headers)
}

func TestLocalFallbackGroup(t *testing.T) {
	url, key, primary, fallback, _, auditPath := hybridFixture(t, true, nil)

	// primary 停止 → fallback 候補で成功(BD §21)
	primary.Close()
	resp, _ := chatReq(t, url, key, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int64(1), fallback.hits.Load())
	assert.Equal(t, "local-fallback", fallback.lastModel.Load())

	records := readAudit(t, auditPath)
	require.Len(t, records, 1)
	assert.Equal(t, "local", records[0].Routing)
	assert.Equal(t, "local-fallback", records[0].PhysicalModel)
}

func TestFallbackOn503(t *testing.T) {
	url, key, primary, fallback, _, _ := hybridFixture(t, true, nil)
	primary.status.Store(503) // Runtime未ロード相当
	resp, _ := chatReq(t, url, key, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int64(1), primary.hits.Load())
	assert.Equal(t, int64(1), fallback.hits.Load())
}

func TestStrictLocalNeverUsesCloud(t *testing.T) {
	url, key, primary, fallback, cloud, _ := hybridFixture(t, true, nil)
	primary.Close()
	fallback.Close()
	resp, body := chatReq(t, url, key, map[string]string{"X-Data-Class": "public"})
	// 全ローカル候補が死んでも cloud へは出ない(strict local)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Contains(t, string(body), "model_unavailable")
	assert.Equal(t, int64(0), cloud.hits.Load())
}

func TestHybridCloudFallback(t *testing.T) {
	url, key, primary, fallback, cloud, auditPath := hybridFixture(t, false, nil)
	primary.Close()
	fallback.Close()

	// public 宣言 + ローカル全滅 → 承認cloudへ(モデルは cloud_fallback.model)
	resp, _ := chatReq(t, url, key, map[string]string{"X-Data-Class": "public"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int64(1), cloud.hits.Load())
	assert.Equal(t, "deepseek/deepseek-chat", cloud.lastModel.Load())
	assert.Equal(t, "Bearer lk_live_cloudkey", cloud.lastAuth.Load())

	records := readAudit(t, auditPath)
	require.Len(t, records, 1)
	assert.Equal(t, "hybrid-cloud", records[0].Routing)
}

func TestHybridPolicyPrecedence(t *testing.T) {
	// ポリシー最優先(BD §8.2): 以下はどれも cloud へ出ない
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"confidential is local-only", map[string]string{"X-Data-Class": "confidential"}},
		{"restricted is local-only", map[string]string{"X-Data-Class": "restricted"}},
		{"undeclared class is local-only (fail closed)", nil},
		{"local-only header wins", map[string]string{"X-Data-Class": "public", "X-Routing-Mode": "local-only"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, key, primary, fallback, cloud, _ := hybridFixture(t, false, nil)
			primary.Close()
			fallback.Close()
			resp, _ := chatReq(t, url, key, tc.headers)
			assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			assert.Equal(t, int64(0), cloud.hits.Load(), "本文が cloud 経路へ出てはならない")
		})
	}
}

func TestHybridNotUsedWhenLocalHealthy(t *testing.T) {
	url, key, primary, _, cloud, _ := hybridFixture(t, false, nil)
	resp, _ := chatReq(t, url, key, map[string]string{"X-Data-Class": "public"})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, int64(1), primary.hits.Load())
	assert.Equal(t, int64(0), cloud.hits.Load(), "ローカル健在時は cloud を使わない")
}

func TestHybridConfigValidation(t *testing.T) {
	// strict_local_mode(既定ON)のまま hybrid.enabled は設定段階で拒否
	_, err := ParseConfig([]byte(`
schema_version: "1"
gateway:
  id: gw-x
hybrid:
  enabled: true
  endpoint: "https://api.lykuro.ai"
  api_key_file: /tmp/k
models:
  - logical_name: m
    runtime: vllm
    endpoint: "http://localhost:8000"
    physical_model: pm
`))
	assert.ErrorContains(t, err, "strict_local_mode")

	// hybrid の許可 class に confidential は書けない
	_, err = ParseConfig([]byte(`
schema_version: "1"
gateway:
  id: gw-x
  strict_local_mode: false
hybrid:
  enabled: true
  endpoint: "https://api.lykuro.ai"
  api_key_file: /tmp/k
  allowed_data_classes: [public, confidential]
models:
  - logical_name: m
    runtime: vllm
    endpoint: "http://localhost:8000"
    physical_model: pm
`))
	assert.ErrorContains(t, err, "public/internal")

	// cloud_fallback は hybrid.enabled が前提
	_, err = ParseConfig([]byte(`
schema_version: "1"
gateway:
  id: gw-x
models:
  - logical_name: m
    runtime: vllm
    endpoint: "http://localhost:8000"
    physical_model: pm
    cloud_fallback:
      model: deepseek/deepseek-chat
`))
	assert.ErrorContains(t, err, "requires hybrid.enabled")

	// cloud_fallback.model は方式Bの複合キー(provider/model)必須
	_, err = ParseConfig([]byte(`
schema_version: "1"
gateway:
  id: gw-x
  strict_local_mode: false
hybrid:
  enabled: true
  endpoint: "https://api.lykuro.ai"
  api_key_file: /tmp/k
models:
  - logical_name: m
    runtime: vllm
    endpoint: "http://localhost:8000"
    physical_model: pm
    cloud_fallback:
      model: deepseek-chat
`))
	assert.ErrorContains(t, err, "composite key")
}

func TestIsTimeoutErr(t *testing.T) {
	assert.False(t, isTimeoutErr(fmt.Errorf("connection refused")))
	assert.True(t, isTimeoutErr(fmt.Errorf("wrapped: %w", context.DeadlineExceeded)))
	assert.True(t, isTimeoutErr(&timeoutNetErr{}))
}

// timeoutNetErr implements net.Error with Timeout()=true.
type timeoutNetErr struct{}

func (*timeoutNetErr) Error() string   { return "i/o timeout" }
func (*timeoutNetErr) Timeout() bool   { return true }
func (*timeoutNetErr) Temporary() bool { return false }

func TestToolPolicy(t *testing.T) {
	deny := false
	url, key, primary, _, _, auditPath := hybridFixture(t, true, &deny)

	// tools 付きは policy_denied
	resp, body := doJSON(t, "POST", url+"/v1/chat/completions", key, map[string]any{
		"model": "company-llm",
		"tools": []map[string]any{{"type": "function"}},
	}, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "tool use is not allowed")
	assert.Equal(t, int64(0), primary.hits.Load())

	// tool_choice・旧OpenAI形式(functions/function_call)も同様に遮断
	for _, body := range []map[string]any{
		{"model": "company-llm", "tool_choice": "auto"},
		{"model": "company-llm", "functions": []map[string]any{{"name": "f"}}},
		{"model": "company-llm", "function_call": "auto"},
	} {
		resp, _ = doJSON(t, "POST", url+"/v1/chat/completions", key, body, nil)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	}

	// tools なしは通る
	resp, _ = chatReq(t, url, key, nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	records := readAudit(t, auditPath)
	assert.GreaterOrEqual(t, len(records), 3)
}

func TestResponsesAndRerankProxy(t *testing.T) {
	url, key, primary, _, _, auditPath := hybridFixture(t, true, nil)

	resp, body := doJSON(t, "POST", url+"/v1/responses", key, map[string]any{
		"model": "company-llm",
		"input": "こんにちは",
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), `"object":"response"`)

	resp, body = doJSON(t, "POST", url+"/v1/rerank", key, map[string]any{
		"model":     "company-llm",
		"query":     "q",
		"documents": []string{"a", "b"},
	}, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "relevance_score")
	assert.Equal(t, int64(2), primary.hits.Load())

	// Responses API 形式の usage(input_tokens/output_tokens)も sniff できる
	records := readAudit(t, auditPath)
	require.GreaterOrEqual(t, len(records), 1)
	require.NotNil(t, records[0].InputTokens)
	assert.Equal(t, int64(11), *records[0].InputTokens)
	assert.Equal(t, int64(22), *records[0].OutputTokens)
}
