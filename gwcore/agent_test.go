package gwcore

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockControlPlane implements the agent API (BD §15.2) for tests.
type mockControlPlane struct {
	*httptest.Server
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey

	installToken string
	agentToken   string
	registered   bool

	configVersion int64
	configJSON    []byte // canonical
	badSignature  bool

	acks       []map[string]any
	usageRows  []map[string]any
	heartbeats int
	lastHealth string
}

func newMockControlPlane(t *testing.T) *mockControlPlane {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	cp := &mockControlPlane{
		priv:         priv,
		pub:          pub,
		installToken: "lkgwt_testinstalltoken",
		agentToken:   "lkpga_testagentcredential",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/gateway-agent/v1/register", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["install_token"] != cp.installToken || cp.registered {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		cp.registered = true
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"agent_token": cp.agentToken, "gateway_id": "gw-test", "tenant_id": "tn-test",
		})
	})
	authed := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+cp.agentToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("POST /api/gateway-agent/v1/heartbeat", authed(func(w http.ResponseWriter, r *http.Request) {
		cp.heartbeats++
		var hb map[string]any
		_ = json.NewDecoder(r.Body).Decode(&hb)
		cp.lastHealth, _ = hb["health_status"].(string)
		_ = json.NewEncoder(w).Encode(map[string]int64{"latest_config_version": cp.configVersion})
	}))
	mux.HandleFunc("GET /api/gateway-agent/v1/config", authed(func(w http.ResponseWriter, r *http.Request) {
		if cp.configVersion == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		sum := sha256.Sum256(cp.configJSON)
		sig := ed25519.Sign(cp.priv, cp.configJSON)
		if cp.badSignature {
			sig[0] ^= 0xFF
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version":        cp.configVersion,
			"schema_version": "1",
			"config_json":    json.RawMessage(cp.configJSON),
			"sha256":         hex.EncodeToString(sum[:]),
			"signature_ref":  base64.StdEncoding.EncodeToString(sig),
		})
	}))
	mux.HandleFunc("POST /api/gateway-agent/v1/config-ack", authed(func(w http.ResponseWriter, r *http.Request) {
		var ack map[string]any
		_ = json.NewDecoder(r.Body).Decode(&ack)
		cp.acks = append(cp.acks, ack)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	mux.HandleFunc("POST /api/gateway-agent/v1/usage", authed(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Rows []map[string]any `json:"rows"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		cp.usageRows = append(cp.usageRows, req.Rows...)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	cp.Server = httptest.NewServer(mux)
	t.Cleanup(cp.Server.Close)
	return cp
}

// setConfig makes the control plane serve a signed config generation.
func (cp *mockControlPlane) setConfig(t *testing.T, version int64, logicalModel string) {
	t.Helper()
	raw := fmt.Sprintf(`{
		"schema_version": "1",
		"gateway": {"id": "gw-test", "allowed_data_classes": ["public"]},
		"models": [{"logical_name": %q, "runtime": "vllm", "endpoint": "http://localhost:9", "physical_model": "pm"}]
	}`, logicalModel)
	canonical, err := canonicalJSON([]byte(raw))
	require.NoError(t, err)
	cp.configVersion = version
	cp.configJSON = canonical
}

func writePubKey(t *testing.T, pub ed25519.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "signing.pub")
	require.NoError(t, os.WriteFile(path,
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600))
	return path
}

func newAgentFixture(t *testing.T, cp *mockControlPlane) (*Agent, *Server, string) {
	t.Helper()
	baseCfg, err := ParseConfig([]byte(`
schema_version: "1"
gateway:
  id: gw-test
models:
  - logical_name: base-model
    runtime: vllm
    endpoint: "http://localhost:9"
    physical_model: pm
`))
	require.NoError(t, err)
	audit, err := NewAuditLogger("")
	require.NoError(t, err)
	srv := NewServer(baseCfg, audit)

	dataDir := t.TempDir()
	tokenFile := filepath.Join(t.TempDir(), "install-token.txt")
	require.NoError(t, os.WriteFile(tokenFile, []byte(cp.installToken+"\n"), 0o600))

	agent, err := NewAgent(AgentSettings{
		ControlPlaneURL:  cp.URL,
		DataDir:          dataDir,
		SigningPubFile:   writePubKey(t, cp.pub),
		InstallTokenFile: tokenFile,
		SoftwareVersion:  Version,
	}, srv)
	require.NoError(t, err)
	return agent, srv, dataDir
}

func TestAgentRegister(t *testing.T) {
	cp := newMockControlPlane(t)
	agent, _, dataDir := newAgentFixture(t, cp)

	require.False(t, agent.Registered())
	require.NoError(t, agent.Register(context.Background()))
	assert.True(t, agent.Registered())
	assert.True(t, cp.registered)

	// 資格情報は 0600 で保存され、トークンファイルは .used へ退避
	info, err := os.Stat(filepath.Join(dataDir, credentialFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assert.NoFileExists(t, agent.st.InstallTokenFile)
	assert.FileExists(t, agent.st.InstallTokenFile+".used")

	// 再登録は no-op
	require.NoError(t, agent.Register(context.Background()))
}

func TestAgentConfigApply(t *testing.T) {
	cp := newMockControlPlane(t)
	agent, srv, _ := newAgentFixture(t, cp)
	require.NoError(t, agent.Register(context.Background()))

	cp.setConfig(t, 1, "delivered-model")
	require.NoError(t, agent.FetchAndApplyConfig(context.Background()))

	// hot reload: 新モデル表が有効化される
	assert.NotNil(t, srv.config().FindModel("delivered-model"))
	assert.Nil(t, srv.config().FindModel("base-model"))
	require.Len(t, cp.acks, 1)
	assert.Equal(t, true, cp.acks[0]["applied"])
}

func TestAgentRejectsTamperedConfig(t *testing.T) {
	cp := newMockControlPlane(t)
	agent, srv, _ := newAgentFixture(t, cp)
	require.NoError(t, agent.Register(context.Background()))

	cp.setConfig(t, 1, "evil-model")
	cp.badSignature = true
	err := agent.FetchAndApplyConfig(context.Background())
	assert.ErrorContains(t, err, "signature mismatch")

	// 現設定を維持(Fail Closed)し、ack は applied=false
	assert.NotNil(t, srv.config().FindModel("base-model"))
	assert.Nil(t, srv.config().FindModel("evil-model"))
	require.Len(t, cp.acks, 1)
	assert.Equal(t, false, cp.acks[0]["applied"])
	assert.Equal(t, "verification_failed", cp.acks[0]["error_code"])
}

func TestAgentRejectsInvalidStagedConfig(t *testing.T) {
	cp := newMockControlPlane(t)
	agent, srv, _ := newAgentFixture(t, cp)
	require.NoError(t, agent.Register(context.Background()))

	// 署名は正しいが staging 検証(スキーマ)で弾かれる設定
	canonical, err := canonicalJSON([]byte(`{"schema_version": "1", "gateway": {"id": "gw-test"}}`))
	require.NoError(t, err)
	cp.configVersion = 1
	cp.configJSON = canonical

	err = agent.FetchAndApplyConfig(context.Background())
	assert.ErrorContains(t, err, "staging validation")
	assert.NotNil(t, srv.config().FindModel("base-model"))
}

func TestAgentLastKnownGoodRetention(t *testing.T) {
	cp := newMockControlPlane(t)
	agent, _, dataDir := newAgentFixture(t, cp)
	require.NoError(t, agent.Register(context.Background()))

	// v10以上を含めて数値ソートの正しさを検証(文字列ソートだと v10 が
	// v2 より前に並び最新世代が prune される)
	for v := int64(1); v <= 12; v++ {
		cp.setConfig(t, v, fmt.Sprintf("model-v%d", v))
		require.NoError(t, agent.FetchAndApplyConfig(context.Background()))
	}
	// 直前2世代 + 最新 = v10,v11,v12 が残る(BD §13.2)
	entries, err := filepath.Glob(filepath.Join(dataDir, "config-v*.json"))
	require.NoError(t, err)
	assert.Len(t, entries, lkgKeep+1)
	assert.FileExists(t, filepath.Join(dataDir, "config-v12.json"))
	assert.FileExists(t, filepath.Join(dataDir, "config-v10.json"))
	assert.NoFileExists(t, filepath.Join(dataDir, "config-v9.json"))
	assert.NoFileExists(t, filepath.Join(dataDir, "config-v1.json"))
	assert.FileExists(t, filepath.Join(dataDir, "config-current.json"))
	assert.Equal(t, int64(12), agent.applied)
}

func TestAgentLoadStoredConfig(t *testing.T) {
	cp := newMockControlPlane(t)
	agent, _, dataDir := newAgentFixture(t, cp)
	require.NoError(t, agent.Register(context.Background()))
	cp.setConfig(t, 3, "restored-model")
	require.NoError(t, agent.FetchAndApplyConfig(context.Background()))

	// 再起動相当: 新しい Server + Agent が保存済み設定で立ち上がる(BD §13.3)
	baseCfg, err := ParseConfig([]byte(`
schema_version: "1"
gateway:
  id: gw-test
models:
  - logical_name: base-model
    runtime: vllm
    endpoint: "http://localhost:9"
    physical_model: pm
`))
	require.NoError(t, err)
	audit, _ := NewAuditLogger("")
	srv2 := NewServer(baseCfg, audit)
	agent2, err := NewAgent(AgentSettings{
		ControlPlaneURL: cp.URL,
		DataDir:         dataDir,
		SigningPubFile:  writePubKey(t, cp.pub),
	}, srv2)
	require.NoError(t, err)
	require.True(t, agent2.Registered(), "credential must survive restart")
	require.NoError(t, agent2.LoadStoredConfig())
	assert.NotNil(t, srv2.config().FindModel("restored-model"))
	assert.Equal(t, int64(3), agent2.applied)
}

func TestAgentHeartbeatDrivesConfigPull(t *testing.T) {
	cp := newMockControlPlane(t)
	agent, srv, _ := newAgentFixture(t, cp)
	require.NoError(t, agent.Register(context.Background()))

	// usage を積んでから tick: heartbeat + usage + config pull が走る
	in, out := int64(5), int64(7)
	srv.usage.add("base-model", &in, &out, false)
	cp.setConfig(t, 1, "pulled-model")

	agent.tick(context.Background())
	assert.Equal(t, 1, cp.heartbeats)
	// base config の runtime(localhost:9)は到達不可 → degraded を実測報告
	assert.Equal(t, "degraded", cp.lastHealth)
	require.Len(t, cp.usageRows, 1)
	assert.Equal(t, "base-model", cp.usageRows[0]["logical_model"])
	assert.Equal(t, float64(5), cp.usageRows[0]["input_tokens"])
	assert.NotNil(t, srv.config().FindModel("pulled-model"))

	// 送信後はドレイン済み
	assert.Empty(t, srv.DrainUsage())
}

func TestAgentUsageRestoredOnFailure(t *testing.T) {
	cp := newMockControlPlane(t)
	agent, srv, _ := newAgentFixture(t, cp)
	require.NoError(t, agent.Register(context.Background()))

	in := int64(3)
	srv.usage.add("base-model", &in, nil, false)
	cp.Server.Close() // 制御プレーン断

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	agent.uploadUsage(ctx)
	// 失敗時はカウンタへ戻る(BD §13.3: 本文なしメタデータのみローカル保持)
	rows := srv.DrainUsage()
	require.Len(t, rows, 1)
	assert.Equal(t, int64(3), rows[0].InputTokens)
}
