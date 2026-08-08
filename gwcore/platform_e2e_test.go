package gwcore

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lykuroai/Native-LLM-Platform/platform"
	"github.com/lykuroai/Native-LLM-Platform/platform/platformcfg"
)

// fakeRuntime is an OpenAI-compatible upstream for platform E2E tests。
type fakeRuntime struct {
	*httptest.Server
	hits atomic.Int64
	fail atomic.Bool // true なら接続拒否相当(503)
}

func newFakeRuntime(t *testing.T, name string) *fakeRuntime {
	t.Helper()
	f := &fakeRuntime{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{}})
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			chunk := map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "hello from " + name}}}}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			usage := map[string]any{"choices": []map[string]any{}, "usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 3}}
			ub, _ := json.Marshal(usage)
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", ub)
			return
		}
		var lastUser string
		for _, m := range body.Messages {
			if m.Role == "user" {
				lastUser = m.Content
			}
		}
		resp := map[string]any{
			"choices": []map[string]any{{"message": map[string]any{
				"role": "assistant", "content": "echo(" + name + "," + body.Model + "):" + lastUser + " [history:" + fmt.Sprint(len(body.Messages)) + "]",
			}}},
			"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 5},
		}
		json.NewEncoder(w).Encode(resp)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

// newPlatformServer builds a gwcore Server with platform route enabled。
func newPlatformServer(t *testing.T, managed bool, runtimes ...*fakeRuntime) (*Server, string) {
	t.Helper()
	plainKey, keyHash, err := GenerateVirtualKey()
	if err != nil {
		t.Fatalf("GenerateVirtualKey: %v", err)
	}

	var eps []platformcfg.RuntimeEndpoint
	var deps []platformcfg.Deployment
	for i, rt := range runtimes {
		id := fmt.Sprintf("rte-%d", i)
		eps = append(eps, platformcfg.RuntimeEndpoint{
			ID: id, ConnectorType: "openai_compatible", BaseURL: rt.URL,
			ManagementMode: platformcfg.ManageInferenceOnly,
		})
		deps = append(deps, platformcfg.Deployment{
			ID: fmt.Sprintf("dep-%d", i), BackendType: platformcfg.BackendExternalConnector,
			RuntimeEndpointID: id, PhysicalModel: fmt.Sprintf("phys-model-%d", i),
		})
	}
	pcfg := &platformcfg.Config{
		Enabled: true,
		Models: []platformcfg.ModelEntry{{
			LogicalName:    "qwen-local",
			ApprovalStatus: platformcfg.ApprovalApproved,
			Capabilities:   platformcfg.Capabilities{Chat: true, Streaming: true, ContextLength: 8192},
			Deployments:    deps,
		}},
		RuntimeEndpoints: eps,
	}
	if managed {
		dir := t.TempDir()
		keyFile := filepath.Join(dir, "mem.key")
		buf := make([]byte, 32)
		_, err := rand.Read(buf)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(keyFile, []byte(hex.EncodeToString(buf)), 0o600))
		pcfg.Memory = platformcfg.Memory{
			Mode: "managed", DataDir: dir, EncryptionKeyFile: keyFile,
		}
	}
	cfg := &Config{
		SchemaVersion: "1",
		Gateway: GatewaySection{
			ID: "gw_test", AllowedDataClasses: []string{"public", "internal"},
		},
		Auth:     AuthSection{VirtualKeys: []VirtualKeyDef{{ID: "vk_1", KeyHash: keyHash}}},
		Platform: pcfg,
	}
	require.NoError(t, cfg.Validate())

	audit, err := NewAuditLogger("")
	require.NoError(t, err)
	srv := NewServer(cfg, audit)
	pf, err := platform.New(cfg.Platform, platform.Options{})
	require.NoError(t, err)
	t.Cleanup(pf.Close)
	srv.SetPlatformBackend(pf)
	return srv, plainKey
}

func doChat(t *testing.T, h http.Handler, key, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPlatformChatCompletion(t *testing.T) {
	rt := newFakeRuntime(t, "a")
	srv, key := newPlatformServer(t, false, rt)
	h := srv.Router()

	rec := doChat(t, h, key,
		`{"model":"qwen-local","messages":[{"role":"user","content":"hi"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "echo(a,phys-model-0):hi")
	// 物理モデル名は透過 body 内(上流応答)にのみ現れ、model 解決は platform 側
	assert.Equal(t, int64(1), rt.hits.Load())
}

func TestPlatformStreaming(t *testing.T) {
	rt := newFakeRuntime(t, "a")
	srv, key := newPlatformServer(t, false, rt)
	h := srv.Router()

	rec := doChat(t, h, key,
		`{"model":"qwen-local","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
	body := rec.Body.String()
	assert.Contains(t, body, "hello from a")
	assert.Contains(t, body, "[DONE]")
}

func TestPlatformModelsEndpoint(t *testing.T) {
	rt := newFakeRuntime(t, "a")
	srv, key := newPlatformServer(t, false, rt)
	h := srv.Router()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "qwen-local")
	// 物理情報の非開示
	assert.NotContains(t, rec.Body.String(), "phys-model")
	assert.NotContains(t, rec.Body.String(), rt.URL)
}

func TestPlatformUnknownModel(t *testing.T) {
	srv, key := newPlatformServer(t, false, newFakeRuntime(t, "a"))
	rec := doChat(t, srv.Router(), key,
		`{"model":"nope","messages":[{"role":"user","content":"hi"}]}`, nil)
	// 未知 model は model_not_available → 503(凍結表 §4.2)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "model_unavailable")
}

func TestPlatformFailover(t *testing.T) {
	rtA := newFakeRuntime(t, "a")
	rtB := newFakeRuntime(t, "b")
	srv, key := newPlatformServer(t, false, rtA, rtB)
	h := srv.Router()

	rtA.fail.Store(true) // A は 503 → B へ failover(Phase 5 request sharding)
	rec := doChat(t, h, key,
		`{"model":"qwen-local","messages":[{"role":"user","content":"hi"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "echo(b")
}

func TestPlatformAllNodesDown(t *testing.T) {
	rtA := newFakeRuntime(t, "a")
	srv, key := newPlatformServer(t, false, rtA)
	rtA.fail.Store(true)
	rec := doChat(t, srv.Router(), key,
		`{"model":"qwen-local","messages":[{"role":"user","content":"hi"}]}`, nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestPlatformConversationMemory(t *testing.T) {
	rt := newFakeRuntime(t, "a")
	srv, key := newPlatformServer(t, true, rt)
	h := srv.Router()

	// turn 1
	rec := doChat(t, h, key,
		`{"model":"qwen-local","messages":[{"role":"user","content":"first"}]}`,
		map[string]string{"X-Lykuro-Conversation-ID": "conv1"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "[history:1]")
	v1 := rec.Header().Get("X-Lykuro-Conversation-Version")
	assert.NotEmpty(t, v1)

	// turn 2: 履歴(user+assistant)+今回 = 3 messages が上流へ渡る
	rec = doChat(t, h, key,
		`{"model":"qwen-local","messages":[{"role":"user","content":"second"}]}`,
		map[string]string{"X-Lykuro-Conversation-ID": "conv1"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "[history:3]")

	// version conflict(古い version 指定)→ 409
	rec = doChat(t, h, key,
		`{"model":"qwen-local","messages":[{"role":"user","content":"third"}]}`,
		map[string]string{
			"X-Lykuro-Conversation-ID":      "conv1",
			"X-Lykuro-Conversation-Version": "1",
		})
	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestPlatformStatelessNeverPersists(t *testing.T) {
	rt := newFakeRuntime(t, "a")
	srv, key := newPlatformServer(t, false, rt) // stateless mode
	rec := doChat(t, srv.Router(), key,
		`{"model":"qwen-local","messages":[{"role":"user","content":"secret"}]}`,
		map[string]string{"X-Lykuro-Conversation-ID": "conv1"})
	// memory 無効 platform で conversation 要求 → 明示エラー(黙って stateless
	// 継続しない、§18)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPlatformFlagOffKeepsLegacyRoute(t *testing.T) {
	// platform.enabled=false + models 定義あり → 旧透過 route(rollback 経路)
	rt := newFakeRuntime(t, "legacy")
	plainKey, keyHash, err := GenerateVirtualKey()
	if err != nil {
		t.Fatalf("GenerateVirtualKey: %v", err)
	}
	cfg := &Config{
		SchemaVersion: "1",
		Gateway:       GatewaySection{ID: "gw_test"},
		Auth:          AuthSection{VirtualKeys: []VirtualKeyDef{{ID: "vk", KeyHash: keyHash}}},
		Models: []ModelDef{{
			LogicalName: "m1", Runtime: "openai_compatible",
			Endpoint: rt.URL, PhysicalModel: "pm",
		}},
		Platform: &platformcfg.Config{Enabled: false},
	}
	require.NoError(t, cfg.Validate())
	audit, err := NewAuditLogger("")
	require.NoError(t, err)
	srv := NewServer(cfg, audit)
	rec := doChat(t, srv.Router(), plainKey,
		`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "echo(legacy,pm)")
}

func TestPlatformKeyModelRestriction(t *testing.T) {
	rt := newFakeRuntime(t, "a")
	plainKey, keyHash, err := GenerateVirtualKey()
	if err != nil {
		t.Fatalf("GenerateVirtualKey: %v", err)
	}
	srvBase, _ := newPlatformServer(t, false, rt)
	cfg := srvBase.config()
	cfg.Auth.VirtualKeys = append(cfg.Auth.VirtualKeys, VirtualKeyDef{
		ID: "vk_restricted", KeyHash: keyHash, AllowedModels: []string{"other-model"},
	})
	// other-model は catalog に無いが AllowsModel 検証のみが対象
	srvBase.SetConfig(cfg)
	rec := doChat(t, srvBase.Router(), plainKey,
		`{"model":"qwen-local","messages":[{"role":"user","content":"hi"}]}`, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "policy_denied")
}

// TestPlatformStreamSSELines ensures SSE passthrough preserves event framing。
func TestPlatformStreamSSELines(t *testing.T) {
	rt := newFakeRuntime(t, "a")
	srv, key := newPlatformServer(t, false, rt)
	rec := doChat(t, srv.Router(), key,
		`{"model":"qwen-local","stream":true,"messages":[{"role":"user","content":"hi"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	dataLines := 0
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			dataLines++
		}
	}
	assert.GreaterOrEqual(t, dataLines, 3) // delta + usage + [DONE]
}

// TestPlatformStrictLocalNoCloudHits verifies AT: platform 経由の推論は
// hybrid(承認済みcloud)が構成されていても cloud へ一切到達しない
// (cloud hit 0)。platform catalog は local runtime のみを候補にする。
func TestPlatformStrictLocalNoCloudHits(t *testing.T) {
	rt := newFakeRuntime(t, "a")

	var cloudHits atomic.Int64
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cloudHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(cloud.Close)

	srv, key := newPlatformServer(t, false, rt)

	// hybrid を有効化した config を適用(strict_local_mode=false の明示 opt-in)
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "cloud.key")
	require.NoError(t, os.WriteFile(keyFile, []byte("lk_test_dummy"), 0o600))
	cfg := srv.config()
	strict := false
	cfg.Gateway.StrictLocalMode = &strict
	cfg.Hybrid = HybridSection{
		Enabled: true, Endpoint: cloud.URL, APIKeyFile: keyFile,
		AllowedDataClasses: []string{"public", "internal"},
	}
	require.NoError(t, cfg.Validate())
	srv.SetConfig(cfg)

	h := srv.Router()
	for _, headers := range []map[string]string{
		nil,
		{"X-Routing-Mode": "hybrid", "X-Data-Class": "public"},
		{"X-Routing-Mode": "local-only", "X-Data-Class": "internal"},
	} {
		rec := doChat(t, h, key,
			`{"model":"qwen-local","messages":[{"role":"user","content":"hi"}]}`, headers)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	assert.Equal(t, int64(3), rt.hits.Load(), "all requests must hit the local runtime")
	assert.Equal(t, int64(0), cloudHits.Load(), "cloud hit must be zero for platform route")
}
