package gwcore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type adminEnv struct {
	srv     *Server
	handler http.Handler
	token   string
	cfgPath string
	audit   string
}

func newAdminEnv(t *testing.T) *adminEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := &Config{
		SchemaVersion: "1",
		Gateway:       GatewaySection{ID: "gw-test"},
		Models: []ModelDef{{
			LogicalName: "chat", Runtime: "ollama",
			Endpoint: "http://127.0.0.1:1", PhysicalModel: "llama3",
		}},
	}
	require.NoError(t, cfg.Validate())

	cfgPath := filepath.Join(dir, "gateway.yaml")
	raw, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfgPath, raw, 0o600))

	auditPath := filepath.Join(dir, "audit.jsonl")
	audit, err := NewAuditLogger(auditPath)
	require.NoError(t, err)
	t.Cleanup(func() { audit.Close() })

	srv := NewServer(cfg, audit)
	plain, hash, err := GenerateAdminToken()
	require.NoError(t, err)

	return &adminEnv{
		srv:   srv,
		token: plain,
		handler: NewAdminHandler(srv, AdminOptions{
			TokenHash: hash, ConfigPath: cfgPath, AuditPath: auditPath,
		}),
		cfgPath: cfgPath,
		audit:   auditPath,
	}
}

func (e *adminEnv) do(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func TestAdminAuth(t *testing.T) {
	env := newAdminEnv(t)
	otherAdmin, _, err := GenerateAdminToken()
	require.NoError(t, err)
	vk, _, err := GenerateVirtualKey()
	require.NoError(t, err)

	cases := []struct {
		name   string
		token  string
		status int
	}{
		{"missing token", "", http.StatusUnauthorized},
		{"wrong admin token", otherAdmin, http.StatusUnauthorized},
		{"virtual key is not an admin token", vk, http.StatusUnauthorized},
		{"valid token", env.token, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(t, http.MethodGet, "/api/overview", tc.token, "")
			require.Equal(t, tc.status, rec.Code)
		})
	}
}

func TestAdminOverview(t *testing.T) {
	env := newAdminEnv(t)
	rec := env.do(t, http.MethodGet, "/api/overview", env.token, "")
	require.Equal(t, http.StatusOK, rec.Code)

	var ov struct {
		GatewayID   string `json:"gateway_id"`
		StrictLocal bool   `json:"strict_local"`
		KeyCount    int    `json:"key_count"`
		Models      []struct {
			LogicalName string `json:"logical_name"`
			Healthy     bool   `json:"healthy"`
		} `json:"models"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ov))
	require.Equal(t, "gw-test", ov.GatewayID)
	require.True(t, ov.StrictLocal)
	require.Zero(t, ov.KeyCount)
	require.Len(t, ov.Models, 1)
	require.Equal(t, "chat", ov.Models[0].LogicalName)
	require.False(t, ov.Models[0].Healthy) // endpoint は到達不能
}

func TestAdminKeyLifecycle(t *testing.T) {
	env := newAdminEnv(t)

	// 発行: 原文が一度だけ返り、即時に認証で使える
	rec := env.do(t, http.MethodPost, "/api/keys", env.token,
		`{"id":"vk-team-a","name":"team A","allowed_models":["chat"],"rpm_limit":10}`)
	require.Equal(t, http.StatusOK, rec.Code)
	var created struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.True(t, strings.HasPrefix(created.Key, VirtualKeyPrefix))

	key, ok := env.srv.authenticator().Authenticate("Bearer " + created.Key)
	require.True(t, ok)
	require.Equal(t, "vk-team-a", key.ID)

	// 永続化: 設定ファイルにハッシュのみが残る(原文は書かれない)
	raw, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)
	require.Contains(t, string(raw), "vk-team-a")
	require.NotContains(t, string(raw), created.Key)

	// 無効化 → 認証拒否
	rec = env.do(t, http.MethodPatch, "/api/keys/vk-team-a", env.token, `{"disabled":true}`)
	require.Equal(t, http.StatusOK, rec.Code)
	_, ok = env.srv.authenticator().Authenticate("Bearer " + created.Key)
	require.False(t, ok)

	// 削除 → 一覧から消え、再削除は 404
	rec = env.do(t, http.MethodDelete, "/api/keys/vk-team-a", env.token, "")
	require.Equal(t, http.StatusOK, rec.Code)
	rec = env.do(t, http.MethodDelete, "/api/keys/vk-team-a", env.token, "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Empty(t, env.srv.config().Auth.VirtualKeys)
}

func TestAdminKeyCreateValidation(t *testing.T) {
	env := newAdminEnv(t)
	cases := []struct {
		name string
		body string
	}{
		{"missing id", `{"name":"x"}`},
		{"invalid json", `{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := env.do(t, http.MethodPost, "/api/keys", env.token, tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
	// 未知モデルの許可は設定検証で拒否(Fail Closed)
	rec := env.do(t, http.MethodPost, "/api/keys", env.token,
		`{"id":"vk-x","allowed_models":["nope"]}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	require.Empty(t, env.srv.config().Auth.VirtualKeys)
}

func TestAdminConfigPut(t *testing.T) {
	env := newAdminEnv(t)

	// 不正な設定は反映されない
	rec := env.do(t, http.MethodPut, "/api/config", env.token, "schema_version: \"9\"\n")
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	require.Equal(t, "gw-test", env.srv.config().Gateway.ID)

	// 正常系: 取得 → 編集 → 反映 → メモリとファイルの両方が更新される
	rec = env.do(t, http.MethodGet, "/api/config", env.token, "")
	require.Equal(t, http.StatusOK, rec.Code)
	edited := strings.Replace(rec.Body.String(), "llama3", "llama3.1", 1)

	rec = env.do(t, http.MethodPut, "/api/config", env.token, edited)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "llama3.1", env.srv.config().Models[0].PhysicalModel)

	raw, err := os.ReadFile(env.cfgPath)
	require.NoError(t, err)
	require.Contains(t, string(raw), "llama3.1")
}

func TestAdminAuditTail(t *testing.T) {
	env := newAdminEnv(t)
	for i := 0; i < 3; i++ {
		require.NoError(t, env.srv.audit.Record(&AuditRecord{
			Timestamp: time.Now().UTC(), RequestID: "req_" + strings.Repeat("a", i+1),
			GatewayID: "gw-test", Endpoint: "/v1/chat/completions",
			Routing: "local-only", Status: 200, Result: "success",
		}))
	}
	rec := env.do(t, http.MethodGet, "/api/audit?limit=2", env.token, "")
	require.Equal(t, http.StatusOK, rec.Code)
	var out struct {
		Available bool `json:"available"`
		Records   []struct {
			RequestID     string `json:"request_id"`
			ContentLogged bool   `json:"content_logged"`
		} `json:"records"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.True(t, out.Available)
	require.Len(t, out.Records, 2)
	require.Equal(t, "req_aaa", out.Records[1].RequestID) // 末尾優先
	for _, r := range out.Records {
		require.False(t, r.ContentLogged) // Zero-Retention は管理面でも不変
	}
}

func TestAdminUIServed(t *testing.T) {
	env := newAdminEnv(t)
	// UI 本体は認証前に配布される(トークン入力画面のため)。API は全て認証必須。
	rec := env.do(t, http.MethodGet, "/", "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "管理画面")
}
