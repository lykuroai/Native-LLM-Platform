package gwcore

import (
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/lykuroai/Native-LLM-Platform/token"
)

//go:embed adminui/index.html
var adminIndexHTML []byte

// AdminTokenPrefix marks local admin-UI credentials。Virtual Key(lkpgw_)・
// agent token(lkpga_)と接頭辞を分け、取り違えを検証段階で弾く。
const AdminTokenPrefix = "lkpadm_"

// AdminTokenHashFile is the DataDir file holding the admin token hash.
const AdminTokenHashFile = "admin-token.hash"

// GenerateAdminToken returns (plaintext, sha256hex)。原文は発行時のみ表示し、
// DataDir にはハッシュだけを保存する(Virtual Key と同じ方針)。
func GenerateAdminToken() (plain, hash string, err error) {
	seg, err := token.RandomTokenSegment(48)
	if err != nil {
		return "", "", fmt.Errorf("generate admin token: %w", err)
	}
	plain = AdminTokenPrefix + seg
	return plain, token.HashToken(plain), nil
}

// AdminOptions configures the embedded admin UI listener。
type AdminOptions struct {
	// TokenHash is the sha256 hex of the admin token(必須。空なら全要求拒否)。
	TokenHash string
	// ConfigPath is the gateway.yaml the UI persists edits to。
	ConfigPath string
	// AuditPath is the audit JSONL destination(空 = stdout、閲覧不可)。
	AuditPath string
	// Connected reports whether a control plane is configured。true の場合、
	// ローカル編集は次回の署名済み世代フェッチで上書きされうる(UI で警告)。
	Connected bool
}

// adminAPI serves the embedded management UI and its JSON API。
// 設計方針: 管理UIは特権面なので物理endpoint等も表示する(データ面の
// /v1/models が logical のみ返すのとは意図的に異なる)。本文の Zero-Retention
// は管理面でも不変 — prompt/response を読む・返す経路は持たない。
type adminAPI struct {
	srv  *Server
	opts AdminOptions
	// mu serializes read-modify-write cycles on the config(キー追加と設定
	// 編集の競合防止。SetConfig 自体は atomic だが世代の逐次性を保証する)。
	mu sync.Mutex
}

// NewAdminHandler builds the admin UI handler(管理listener専用。データ面の
// Router には決して mount しない)。
func NewAdminHandler(srv *Server, opts AdminOptions) http.Handler {
	a := &adminAPI{srv: srv, opts: opts}
	r := chi.NewRouter()

	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(adminIndexHTML)
	})

	r.Get("/api/overview", a.withAuth(a.handleOverview))
	r.Get("/api/config", a.withAuth(a.handleConfigGet))
	r.Put("/api/config", a.withAuth(a.handleConfigPut))
	r.Get("/api/keys", a.withAuth(a.handleKeysList))
	r.Post("/api/keys", a.withAuth(a.handleKeyCreate))
	r.Patch("/api/keys/{id}", a.withAuth(a.handleKeyPatch))
	r.Delete("/api/keys/{id}", a.withAuth(a.handleKeyDelete))
	r.Get("/api/discover", a.withAuth(a.handleDiscover))
	r.Post("/api/discover/adopt", a.withAuth(a.handleDiscoverAdopt))
	r.Get("/api/audit", a.withAuth(a.handleAuditTail))
	r.Get("/api/metrics", a.withAuth(func(w http.ResponseWriter, r *http.Request) {
		a.srv.Metrics().ServeHTTP(w, r)
	}))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusNotFound, "invalid_request", "unknown endpoint", requestID(r))
	})
	return r
}

// withAuth validates the Bearer admin token(constant-time 比較)。
func (a *adminAPI) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := ensureRequestID(r)
		w.Header().Set("X-Request-ID", reqID)
		tok := token.BearerToken(r.Header.Get("Authorization"))
		if a.opts.TokenHash == "" || !strings.HasPrefix(tok, AdminTokenPrefix) ||
			subtle.ConstantTimeCompare([]byte(token.HashToken(tok)), []byte(a.opts.TokenHash)) != 1 {
			a.srv.metrics.recordDenied(r.URL.Path, http.StatusUnauthorized, "authentication_failed")
			writeAPIError(w, http.StatusUnauthorized, "authentication_failed",
				"invalid or missing admin token", reqID)
			return
		}
		next(w, r)
	}
}

func (a *adminAPI) handleOverview(w http.ResponseWriter, r *http.Request) {
	cfg := a.srv.config()
	type modelStatus struct {
		LogicalName   string `json:"logical_name"`
		Runtime       string `json:"runtime"`
		Endpoint      string `json:"endpoint"`
		PhysicalModel string `json:"physical_model"`
		Healthy       bool   `json:"healthy"`
	}
	endpointUp := map[string]bool{}
	models := make([]modelStatus, 0, len(cfg.Models))
	for _, m := range cfg.Models {
		up, checked := endpointUp[m.Endpoint]
		if !checked {
			up = a.srv.proxy.pingRuntime(r.Context(), m.Endpoint)
			endpointUp[m.Endpoint] = up
		}
		models = append(models, modelStatus{
			LogicalName:   m.LogicalName,
			Runtime:       m.Runtime,
			Endpoint:      m.Endpoint,
			PhysicalModel: m.PhysicalModel,
			Healthy:       up,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":          Version,
		"gateway_id":       cfg.Gateway.ID,
		"listen":           cfg.Gateway.Listen,
		"strict_local":     cfg.StrictLocal(),
		"platform_enabled": cfg.PlatformEnabled(),
		"connected":        a.opts.Connected,
		"config_path":      a.opts.ConfigPath,
		"audit_available":  a.opts.AuditPath != "",
		"key_count":        len(cfg.Auth.VirtualKeys),
		"models":           models,
	})
}

func (a *adminAPI) handleConfigGet(w http.ResponseWriter, _ *http.Request) {
	raw, err := yaml.Marshal(a.srv.config())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), "")
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Write(raw)
}

func (a *adminAPI) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "read body failed", requestID(r))
		return
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_request", err.Error(), requestID(r))
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.persistAndApply(cfg); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), requestID(r))
		return
	}
	a.auditAdmin(r, "admin_config_applied")
	writeJSON(w, http.StatusOK, map[string]any{"applied": true, "models": len(cfg.Models)})
}

func (a *adminAPI) handleKeysList(w http.ResponseWriter, _ *http.Request) {
	cfg := a.srv.config()
	type keyEntry struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		AllowedModels []string `json:"allowed_models"`
		RPMLimit      int      `json:"rpm_limit"`
		Disabled      bool     `json:"disabled"`
		ToolsAllowed  bool     `json:"tools_allowed"`
	}
	out := make([]keyEntry, 0, len(cfg.Auth.VirtualKeys))
	for i := range cfg.Auth.VirtualKeys {
		k := &cfg.Auth.VirtualKeys[i]
		out = append(out, keyEntry{
			ID: k.ID, Name: k.Name, AllowedModels: k.AllowedModels,
			RPMLimit: k.RPMLimit, Disabled: k.Disabled, ToolsAllowed: k.ToolsAllowed(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (a *adminAPI) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		AllowedModels []string `json:"allowed_models"`
		RPMLimit      int      `json:"rpm_limit"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body", requestID(r))
		return
	}
	if body.ID == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "id is required", requestID(r))
		return
	}
	plain, hash, err := GenerateVirtualKey()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), requestID(r))
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg, err := cloneConfig(a.srv.config())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), requestID(r))
		return
	}
	cfg.Auth.VirtualKeys = append(cfg.Auth.VirtualKeys, VirtualKeyDef{
		ID: body.ID, Name: body.Name, KeyHash: hash,
		AllowedModels: body.AllowedModels, RPMLimit: body.RPMLimit,
	})
	if err := cfg.Validate(); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_request", err.Error(), requestID(r))
		return
	}
	if err := a.persistAndApply(cfg); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), requestID(r))
		return
	}
	a.auditAdmin(r, "admin_key_created")
	// 原文は一度きりの応答。設定・ログにはハッシュのみ残る。
	writeJSON(w, http.StatusOK, map[string]any{"id": body.ID, "key": plain, "key_hash": hash})
}

func (a *adminAPI) handleKeyPatch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          *string   `json:"name"`
		Disabled      *bool     `json:"disabled"`
		RPMLimit      *int      `json:"rpm_limit"`
		AllowedModels *[]string `json:"allowed_models"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body", requestID(r))
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.mutateKey(w, r, chi.URLParam(r, "id"), "admin_key_updated", func(k *VirtualKeyDef) {
		if body.Name != nil {
			k.Name = *body.Name
		}
		if body.Disabled != nil {
			k.Disabled = *body.Disabled
		}
		if body.RPMLimit != nil {
			k.RPMLimit = *body.RPMLimit
		}
		if body.AllowedModels != nil {
			k.AllowedModels = *body.AllowedModels
		}
	})
}

func (a *adminAPI) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := chi.URLParam(r, "id")
	cfg, err := cloneConfig(a.srv.config())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), requestID(r))
		return
	}
	keys := cfg.Auth.VirtualKeys[:0]
	found := false
	for _, k := range cfg.Auth.VirtualKeys {
		if k.ID == id {
			found = true
			continue
		}
		keys = append(keys, k)
	}
	if !found {
		writeAPIError(w, http.StatusNotFound, "invalid_request", "unknown key id", requestID(r))
		return
	}
	cfg.Auth.VirtualKeys = keys
	if err := a.persistAndApply(cfg); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), requestID(r))
		return
	}
	a.auditAdmin(r, "admin_key_deleted")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// mutateKey applies fn to the key with the given id and persists(呼出側で
// a.mu を保持していること)。
func (a *adminAPI) mutateKey(w http.ResponseWriter, r *http.Request, id, auditResult string, fn func(*VirtualKeyDef)) {
	cfg, err := cloneConfig(a.srv.config())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), requestID(r))
		return
	}
	var target *VirtualKeyDef
	for i := range cfg.Auth.VirtualKeys {
		if cfg.Auth.VirtualKeys[i].ID == id {
			target = &cfg.Auth.VirtualKeys[i]
			break
		}
	}
	if target == nil {
		writeAPIError(w, http.StatusNotFound, "invalid_request", "unknown key id", requestID(r))
		return
	}
	fn(target)
	if err := cfg.Validate(); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_request", err.Error(), requestID(r))
		return
	}
	if err := a.persistAndApply(cfg); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), requestID(r))
		return
	}
	a.auditAdmin(r, auditResult)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "disabled": target.Disabled})
}

// handleDiscover scans for local runtimes(read-only。取込は adopt が担う)。
// cidr 未指定はローカルホストの既知ポートのみ。CIDR は管理者の明示指定が
// 必須で、上限 /22(広域スキャンをさせない)。
func (a *adminAPI) handleDiscover(w http.ResponseWriter, r *http.Request) {
	hosts, err := DiscoverHosts(r.URL.Query().Get("cidr"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error(), requestID(r))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	candidates := DiscoverRuntimes(ctx, hosts, ConfiguredEndpoints(a.srv.config()))
	a.auditAdmin(r, "admin_discover_scanned")
	writeJSON(w, http.StatusOK, map[string]any{
		"hosts_scanned": len(hosts),
		"candidates":    candidates,
	})
}

// handleDiscoverAdopt promotes one discovered candidate into the config
// (承認フローの実体。ここを通らない限り発見された Runtime へは接続しない)。
func (a *adminAPI) handleDiscoverAdopt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LogicalName   string `json:"logical_name"`
		Runtime       string `json:"runtime"`
		Endpoint      string `json:"endpoint"`
		PhysicalModel string `json:"physical_model"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body", requestID(r))
		return
	}
	if body.LogicalName == "" || body.Endpoint == "" || body.PhysicalModel == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request",
			"logical_name, endpoint and physical_model are required", requestID(r))
		return
	}
	if body.Runtime == "" {
		body.Runtime = "openai_compatible"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cfg, err := cloneConfig(a.srv.config())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), requestID(r))
		return
	}
	cfg.Models = append(cfg.Models, ModelDef{
		LogicalName:   body.LogicalName,
		Runtime:       body.Runtime,
		Endpoint:      strings.TrimRight(body.Endpoint, "/"),
		PhysicalModel: body.PhysicalModel,
	})
	if err := cfg.Validate(); err != nil {
		writeAPIError(w, http.StatusUnprocessableEntity, "invalid_request", err.Error(), requestID(r))
		return
	}
	if err := a.persistAndApply(cfg); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), requestID(r))
		return
	}
	a.auditAdmin(r, "admin_model_adopted")
	writeJSON(w, http.StatusOK, map[string]any{
		"adopted": body.LogicalName, "endpoint": body.Endpoint,
	})
}

// handleAuditTail returns the last N audit records(監査JSONLの読み戻し。
// 記録は no-content なので本文が混ざることはない)。
func (a *adminAPI) handleAuditTail(w http.ResponseWriter, r *http.Request) {
	if a.opts.AuditPath == "" {
		writeJSON(w, http.StatusOK, map[string]any{"available": false, "records": []any{}})
		return
	}
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	records, err := tailJSONL(a.opts.AuditPath, limit)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", err.Error(), requestID(r))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "records": records})
}

// persistAndApply writes cfg to ConfigPath atomically, then hot-reloads。
// 書込失敗時は適用しない(ファイルとメモリの世代乖離を作らない)。
func (a *adminAPI) persistAndApply(cfg *Config) error {
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	header := []byte("# managed by private-gateway admin UI (comments are not preserved)\n")
	tmp := a.opts.ConfigPath + ".tmp"
	if err := os.WriteFile(tmp, append(header, raw...), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, a.opts.ConfigPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("replace config: %w", err)
	}
	a.srv.SetConfig(cfg)
	return nil
}

// auditAdmin records a management action(操作証跡。本文なしは通常監査と同じ)。
func (a *adminAPI) auditAdmin(r *http.Request, result string) {
	rec := &AuditRecord{
		Timestamp: time.Now().UTC(),
		RequestID: requestID(r),
		GatewayID: a.srv.config().Gateway.ID,
		Endpoint:  r.URL.Path,
		Routing:   "admin",
		Status:    http.StatusOK,
		Result:    result,
	}
	if err := a.srv.audit.Record(rec); err != nil {
		slog.Error("admin audit record failed", "error", err)
	}
}

// cloneConfig deep-copies via YAML round trip(検証・既定値適用込み)。
func cloneConfig(c *Config) (*Config, error) {
	raw, err := yaml.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("clone config: %w", err)
	}
	return ParseConfig(raw)
}

// tailJSONL returns up to limit trailing JSON lines of a JSONL file。
// 末尾 512KiB のみ読む(監査ログの肥大でメモリを使わない)。
func tailJSONL(path string, limit int) ([]json.RawMessage, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		if os.IsNotExist(err) {
			return []json.RawMessage{}, nil
		}
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat audit log: %w", err)
	}
	const maxRead = 512 << 10
	offset := int64(0)
	if st.Size() > maxRead {
		offset = st.Size() - maxRead
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek audit log: %w", err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read audit log: %w", err)
	}
	if offset > 0 {
		// 途中から読んだ場合、先頭の欠けた行を捨てる
		if idx := bytes.IndexByte(data, '\n'); idx >= 0 {
			data = data[idx+1:]
		}
	}
	lines := bytes.Split(data, []byte("\n"))
	records := make([]json.RawMessage, 0, len(lines))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || !json.Valid(line) {
			continue
		}
		records = append(records, json.RawMessage(line))
	}
	if len(records) > limit {
		records = records[len(records)-limit:]
	}
	return records, nil
}
