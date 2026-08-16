package gwcore

// workflow_e2e_test.go — Workflow Orchestrator の Gateway E2E
// (MRCI-002 §12: IT-01/02/13(縮退)/16/17 相当と API 面の検証)。

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lykuroai/Native-LLM-Platform/platform"
	"github.com/lykuroai/Native-LLM-Platform/platform/platformcfg"
	"github.com/lykuroai/Native-LLM-Platform/platform/workflow"
)

// newWorkflowServer wires two runtimes as two logical models and enables the
// Workflow Orchestrator(異種 endpoint 連鎖の最小構成)。
func newWorkflowServer(t *testing.T) (*Server, string, *fakeRuntime, *fakeRuntime) {
	t.Helper()
	rtA := newFakeRuntime(t, "runtimeA")
	rtB := newFakeRuntime(t, "runtimeB")
	plainKey, keyHash, err := GenerateVirtualKey()
	require.NoError(t, err)

	pcfg := &platformcfg.Config{
		Enabled: true,
		Models: []platformcfg.ModelEntry{
			{
				LogicalName:    "model-extract",
				ApprovalStatus: platformcfg.ApprovalApproved,
				Capabilities:   platformcfg.Capabilities{Chat: true, Streaming: true, ContextLength: 8192},
				Deployments: []platformcfg.Deployment{{
					ID: "dep-a", BackendType: platformcfg.BackendExternalConnector,
					RuntimeEndpointID: "rte-a", PhysicalModel: "phys-a",
				}},
			},
			{
				LogicalName:    "model-review",
				ApprovalStatus: platformcfg.ApprovalApproved,
				Capabilities:   platformcfg.Capabilities{Chat: true, Streaming: true, ContextLength: 8192},
				Deployments: []platformcfg.Deployment{{
					ID: "dep-b", BackendType: platformcfg.BackendExternalConnector,
					RuntimeEndpointID: "rte-b", PhysicalModel: "phys-b",
				}},
			},
		},
		RuntimeEndpoints: []platformcfg.RuntimeEndpoint{
			{ID: "rte-a", ConnectorType: "openai_compatible", BaseURL: rtA.URL, ManagementMode: platformcfg.ManageInferenceOnly},
			{ID: "rte-b", ConnectorType: "openai_compatible", BaseURL: rtB.URL, ManagementMode: platformcfg.ManageInferenceOnly},
		},
		Pools: []platformcfg.PoolEntry{
			{ID: "pool-review", DeploymentIDs: []string{"dep-b"}},
		},
		Workflows: platformcfg.Workflows{Enabled: true, DataDir: t.TempDir()},
	}
	cfg := &Config{
		SchemaVersion: "1",
		Gateway:       GatewaySection{ID: "gw_test", AllowedDataClasses: []string{"public", "internal"}},
		Auth:          AuthSection{VirtualKeys: []VirtualKeyDef{{ID: "vk_wf", KeyHash: keyHash}}},
		Platform:      pcfg,
	}
	require.NoError(t, cfg.Validate())

	audit, err := NewAuditLogger("")
	require.NoError(t, err)
	srv := NewServer(cfg, audit)
	pf, err := platform.New(cfg.Platform, platform.Options{})
	require.NoError(t, err)
	t.Cleanup(pf.Close)
	srv.SetPlatformBackend(pf)
	srv.SetWorkflowService(pf.Workflows())
	return srv, plainKey, rtA, rtB
}

const e2eFlowDef = `{
  "name": "Two Runtime Follow-up Chain",
  "alias": "two-runtime-follow-up",
  "execution_mode": "SEQUENTIAL",
  "inputs_schema": {
    "type": "object",
    "required": ["question1"],
    "properties": {
      "question1": {"type": "string"},
      "question2": {"type": "string"}
    }
  },
  "steps": [
    {
      "step_id": "runtime_a",
      "type": "LLM",
      "runtime_target": {"logical_model": "model-extract"},
      "system_prompt": "extract",
      "input_mapping": {"text": "{{inputs.question1}}"}
    },
    {
      "step_id": "runtime_b",
      "type": "LLM",
      "depends_on": ["runtime_a"],
      "runtime_target": {"logical_model": "model-review", "pool_id": "pool-review"},
      "input_mapping": {"text": "A:{{steps.runtime_a.output}} Q2:{{inputs.question2}}"}
    }
  ],
  "output_mapping": {"text": "{{steps.runtime_b.output}}"}
}`

func publishE2EFlow(t *testing.T, srv *Server) {
	t.Helper()
	svc := srv.workflowService()
	require.NotNil(t, svc)
	meta, errs := svc.CreateFlow([]byte(e2eFlowDef))
	require.Empty(t, errs)
	_, errs = svc.PublishFlow(meta.FlowID, 1)
	require.Empty(t, errs)
}

func doWF(t *testing.T, h http.Handler, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// IT-01/02: 異種 endpoint の 2 Runtime 連鎖が sync で成功する。
func TestWorkflowRunSync(t *testing.T) {
	srv, key, rtA, rtB := newWorkflowServer(t)
	publishE2EFlow(t, srv)
	h := srv.Router()

	rec := doWF(t, h, http.MethodPost, "/v1/workflows/two-runtime-follow-up/runs", key,
		`{"inputs":{"question1":"Q1","question2":"Q2"}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var out struct {
		RunID       string `json:"run_id"`
		Status      string `json:"status"`
		FlowVersion int    `json:"flow_version"`
		Output      struct {
			Text string `json:"text"`
		} `json:"output"`
		Usage struct {
			Total int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "COMPLETED", out.Status)
	assert.Equal(t, 1, out.FlowVersion)
	// B の echo に A の echo(=Q1 由来)と Q2 が含まれる
	assert.Contains(t, out.Output.Text, "runtimeB")
	assert.Contains(t, out.Output.Text, "runtimeA")
	assert.Contains(t, out.Output.Text, "Q1")
	assert.Contains(t, out.Output.Text, "Q2")
	assert.Equal(t, int64(1), rtA.hits.Load())
	assert.Equal(t, int64(1), rtB.hits.Load())
	assert.Equal(t, int64(32), out.Usage.Total) // (11+5)*2

	// Run 照会と Steps(データプレーンには deployment/node ID を返さない)
	rec = doWF(t, h, http.MethodGet, "/v1/workflow-runs/"+out.RunID, key, "")
	require.Equal(t, http.StatusOK, rec.Code)
	rec = doWF(t, h, http.MethodGet, "/v1/workflow-runs/"+out.RunID+"/steps", key, "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "dep-a")
	assert.NotContains(t, rec.Body.String(), "dep-b")
	assert.Contains(t, rec.Body.String(), "runtime_a")
	assert.Contains(t, rec.Body.String(), "pool-review")
}

// SSE: response_mode=stream で Event が届き、最終 Output が載る。
func TestWorkflowRunStream(t *testing.T) {
	srv, key, _, _ := newWorkflowServer(t)
	publishE2EFlow(t, srv)
	h := srv.Router()

	rec := doWF(t, h, http.MethodPost, "/v1/workflows/two-runtime-follow-up/runs", key,
		`{"inputs":{"question1":"Q1"},"response_mode":"stream"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
	body := rec.Body.String()
	assert.Contains(t, body, "event: workflow.started")
	assert.Contains(t, body, "event: step.completed")
	assert.Contains(t, body, "event: workflow.completed")
	assert.Contains(t, body, "event: workflow.output")
	assert.Contains(t, body, "runtimeB")
	assert.Contains(t, body, "event: done")
}

// IT-08: Last-Event-ID からの再取得。
func TestWorkflowEventsResume(t *testing.T) {
	srv, key, _, _ := newWorkflowServer(t)
	publishE2EFlow(t, srv)
	h := srv.Router()

	rec := doWF(t, h, http.MethodPost, "/v1/workflows/two-runtime-follow-up/runs", key,
		`{"inputs":{"question1":"Q1"}}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		RunID string `json:"run_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))

	req := httptest.NewRequest(http.MethodGet, "/v1/workflow-runs/"+out.RunID+"/events", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Last-Event-ID", "2")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	require.Equal(t, http.StatusOK, rec2.Code)
	sc := bufio.NewScanner(rec2.Body)
	var ids []string
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "id: ") {
			ids = append(ids, strings.TrimPrefix(sc.Text(), "id: "))
		}
	}
	require.NotEmpty(t, ids)
	assert.Equal(t, "3", ids[0], "resume must start after Last-Event-ID")
	// 観測用 events endpoint には本文を載せない
	assert.NotContains(t, rec2.Body.String(), "runtimeB")
}

// IT-16: OpenAI 互換 model=flow:{alias}(非 stream / stream)。
func TestWorkflowOpenAIAlias(t *testing.T) {
	srv, key, _, _ := newWorkflowServer(t)
	publishE2EFlow(t, srv)
	h := srv.Router()

	rec := doChat(t, h, key,
		`{"model":"flow:two-runtime-follow-up","messages":[{"role":"user","content":"Q1"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, "chat.completion", out.Object)
	assert.Equal(t, "flow:two-runtime-follow-up", out.Model)
	require.Len(t, out.Choices, 1)
	assert.Contains(t, out.Choices[0].Message.Content, "runtimeB")

	rec = doChat(t, h, key,
		`{"model":"flow:two-runtime-follow-up","stream":true,"messages":[{"role":"user","content":"Q1"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/event-stream")
	assert.Contains(t, rec.Body.String(), "chat.completion.chunk")
	assert.Contains(t, rec.Body.String(), "data: [DONE]")
}

// IT-17: workflows 有効でも通常 model 呼出しは従来どおり。
func TestWorkflowDoesNotBreakNormalModels(t *testing.T) {
	srv, key, rtA, _ := newWorkflowServer(t)
	publishE2EFlow(t, srv)
	h := srv.Router()

	rec := doChat(t, h, key,
		`{"model":"model-extract","messages":[{"role":"user","content":"plain"}]}`, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "runtimeA")
	assert.Equal(t, int64(1), rtA.hits.Load())
}

// 認可: allowed_models に flow:{alias} が無ければ 403、Run は他キーから不可視。
func TestWorkflowAuthorization(t *testing.T) {
	srv, key, _, _ := newWorkflowServer(t)
	publishE2EFlow(t, srv)
	h := srv.Router()

	// 別キー(model-extract のみ許可)を追加
	plain2, hash2, err := GenerateVirtualKey()
	require.NoError(t, err)
	cfg, err := cloneConfig(srv.config())
	require.NoError(t, err)
	cfg.Auth.VirtualKeys = append(cfg.Auth.VirtualKeys, VirtualKeyDef{
		ID: "vk_restricted", KeyHash: hash2, AllowedModels: []string{"model-extract"},
	})
	require.NoError(t, cfg.Validate())
	srv.SetConfig(cfg)

	rec := doWF(t, h, http.MethodPost, "/v1/workflows/two-runtime-follow-up/runs", plain2,
		`{"inputs":{"question1":"Q1"}}`)
	require.Equal(t, http.StatusForbidden, rec.Code)

	// 実行キーの Run は制限キーから見えない(404)
	rec = doWF(t, h, http.MethodPost, "/v1/workflows/two-runtime-follow-up/runs", key,
		`{"inputs":{"question1":"Q1"}}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		RunID string `json:"run_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	rec = doWF(t, h, http.MethodGet, "/v1/workflow-runs/"+out.RunID, plain2, "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// 未公開 alias・workflows 無効時の挙動。
func TestWorkflowNotFoundAndDisabled(t *testing.T) {
	srv, key, _, _ := newWorkflowServer(t)
	publishE2EFlow(t, srv)
	h := srv.Router()

	rec := doWF(t, h, http.MethodPost, "/v1/workflows/nope/runs", key, `{"inputs":{}}`)
	require.Equal(t, http.StatusNotFound, rec.Code)

	// async は W1 対象外 — 明示拒否
	rec = doWF(t, h, http.MethodPost, "/v1/workflows/two-runtime-follow-up/runs", key,
		`{"inputs":{"question1":"Q1"},"response_mode":"async"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// workflows 無効 server では flow: model は 404
	rt := newFakeRuntime(t, "solo")
	srv2, key2 := newPlatformServer(t, false, rt)
	rec = doChat(t, srv2.Router(), key2,
		`{"model":"flow:two-runtime-follow-up","messages":[{"role":"user","content":"x"}]}`, nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// Cancel API の疎通(冪等)。
func TestWorkflowCancelEndpoint(t *testing.T) {
	srv, key, _, _ := newWorkflowServer(t)
	publishE2EFlow(t, srv)
	h := srv.Router()

	rec := doWF(t, h, http.MethodPost, "/v1/workflows/two-runtime-follow-up/runs", key,
		`{"inputs":{"question1":"Q1"}}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		RunID string `json:"run_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))

	rec = doWF(t, h, http.MethodPost, "/v1/workflow-runs/"+out.RunID+"/cancel", key, "")
	require.Equal(t, http.StatusAccepted, rec.Code)
}

// 管理 API: Draft → Validate → Publish → Run 一覧(admin token 経由)。
func TestWorkflowAdminAPI(t *testing.T) {
	srv, key, _, _ := newWorkflowServer(t)
	h := srv.Router()

	plainAdmin, hash, err := GenerateAdminToken()
	require.NoError(t, err)
	dir := t.TempDir()
	admin := NewAdminHandler(srv, AdminOptions{TokenHash: hash, ConfigPath: dir + "/gateway.yaml"})
	doAdmin := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+plainAdmin)
		rec := httptest.NewRecorder()
		admin.ServeHTTP(rec, req)
		return rec
	}

	defJSON, err := json.Marshal(map[string]any{"definition": json.RawMessage(e2eFlowDef)})
	require.NoError(t, err)
	rec := doAdmin(http.MethodPost, "/api/workflows", string(defJSON))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var meta struct {
		FlowID string `json:"flow_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &meta))

	rec = doAdmin(http.MethodPost, "/api/workflows/"+meta.FlowID+"/validate", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"valid":true`)

	// revision 不一致は 409(ETag 相当)
	rec = doAdmin(http.MethodPost, "/api/workflows/"+meta.FlowID+"/publish", `{"revision":99}`)
	require.Equal(t, http.StatusConflict, rec.Code)

	rec = doAdmin(http.MethodPost, "/api/workflows/"+meta.FlowID+"/publish", `{"revision":1}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// データプレーンから実行 → 管理面 Run 一覧に載る
	rec2 := doWF(t, h, http.MethodPost, "/v1/workflows/two-runtime-follow-up/runs", key,
		`{"inputs":{"question1":"Q1"}}`)
	require.Equal(t, http.StatusOK, rec2.Code)

	require.Eventually(t, func() bool {
		rec = doAdmin(http.MethodGet, "/api/workflow-runs", "")
		return rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "COMPLETED")
	}, 5*time.Second, 20*time.Millisecond)

	// suspend で新規実行を止める
	rec = doAdmin(http.MethodPost, "/api/workflows/"+meta.FlowID+"/status", `{"status":"suspended"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	rec2 = doWF(t, h, http.MethodPost, "/v1/workflows/two-runtime-follow-up/runs", key,
		`{"inputs":{"question1":"Q1"}}`)
	require.Equal(t, http.StatusNotFound, rec2.Code)

	// 認証なしは 401
	req := httptest.NewRequest(http.MethodGet, "/api/workflows", nil)
	recNoAuth := httptest.NewRecorder()
	admin.ServeHTTP(recNoAuth, req)
	require.Equal(t, http.StatusUnauthorized, recNoAuth.Code)
	_ = workflow.RunCompleted
}

// config: allowed_models の flow: エントリは workflows 有効時のみ許可。
func TestConfigFlowAllowedModels(t *testing.T) {
	base := func(workflowsEnabled bool, allowed []string) *Config {
		_, hash, err := GenerateVirtualKey()
		require.NoError(t, err)
		pcfg := &platformcfg.Config{
			Enabled: true,
			Models: []platformcfg.ModelEntry{{
				LogicalName:    "m1",
				ApprovalStatus: platformcfg.ApprovalApproved,
				Deployments: []platformcfg.Deployment{{
					ID: "d1", BackendType: platformcfg.BackendExternalConnector,
					RuntimeEndpointID: "r1", PhysicalModel: "p1",
				}},
			}},
			RuntimeEndpoints: []platformcfg.RuntimeEndpoint{{
				ID: "r1", ConnectorType: "openai_compatible", BaseURL: "http://127.0.0.1:9",
			}},
			Workflows: platformcfg.Workflows{Enabled: workflowsEnabled, DataDir: t.TempDir()},
		}
		return &Config{
			SchemaVersion: "1",
			Gateway:       GatewaySection{ID: "gw"},
			Auth:          AuthSection{VirtualKeys: []VirtualKeyDef{{ID: "k1", KeyHash: hash, AllowedModels: allowed}}},
			Platform:      pcfg,
		}
	}
	require.NoError(t, base(true, []string{"m1", "flow:my-chain"}).Validate())
	require.Error(t, base(false, []string{"flow:my-chain"}).Validate(), "workflows disabled")
	require.Error(t, base(true, []string{"flow:"}).Validate(), "empty alias")
	require.Error(t, base(true, []string{"unknown-model"}).Validate())
}

// platformcfg: flow: prefix の model 名と pool 検証。
func TestPlatformcfgPoolsAndReservedPrefix(t *testing.T) {
	mk := func(mutate func(c *platformcfg.Config)) error {
		c := &platformcfg.Config{
			Enabled: true,
			Models: []platformcfg.ModelEntry{{
				LogicalName:    "m1",
				ApprovalStatus: platformcfg.ApprovalApproved,
				Deployments: []platformcfg.Deployment{{
					ID: "d1", BackendType: platformcfg.BackendExternalConnector,
					RuntimeEndpointID: "r1", PhysicalModel: "p1",
				}},
			}},
			RuntimeEndpoints: []platformcfg.RuntimeEndpoint{{
				ID: "r1", ConnectorType: "openai_compatible", BaseURL: "http://127.0.0.1:9",
			}},
		}
		if mutate != nil {
			mutate(c)
		}
		return c.Validate()
	}
	require.NoError(t, mk(nil))
	require.Error(t, mk(func(c *platformcfg.Config) { c.Models[0].LogicalName = "flow:x" }))
	require.NoError(t, mk(func(c *platformcfg.Config) {
		c.Pools = []platformcfg.PoolEntry{{ID: "p1", DeploymentIDs: []string{"d1"}}}
	}))
	require.Error(t, mk(func(c *platformcfg.Config) {
		c.Pools = []platformcfg.PoolEntry{{ID: "p1", DeploymentIDs: []string{"nope"}}}
	}))
	require.Error(t, mk(func(c *platformcfg.Config) {
		c.Pools = []platformcfg.PoolEntry{{ID: "p1"}}
	}))
	require.Error(t, mk(func(c *platformcfg.Config) {
		c.Workflows = platformcfg.Workflows{Enabled: true, ContentRetentionDays: 7}
	}), "content retention not supported in W1")
}
