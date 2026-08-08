package gwcore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lykuroai/Native-LLM-Platform/platform/contract"
)

// Version is the gateway software version (パッケージの software_version と対応)。
var Version = "0.1.0"

// Server is the customer-side gateway HTTP service (BD §9)。
// 設定は Agent の hot reload(SetConfig)で入れ替わるため atomic に保持する。
type Server struct {
	mu    sync.RWMutex
	cfg   *Config
	auth  *Authenticator
	proxy *proxyCore
	audit *AuditLogger
	usage *usageCounter
	// platform is the gateway-platform-v1 backend(LYK-NLP-SD-001 v2.0
	// Phase 1 Adapter)。nil または config の platform.enabled=false なら
	// 既存 proxyRequest 経路を維持する(rollback 経路)。
	platform contract.Backend
	// onConfigApplied is invoked after SetConfig(platform backend の再構築
	// 用 hook。main が設定する)。
	onConfigApplied func(*Config)
	// metrics is the always-on in-process registry(ADD §17.1)。endpoint の
	// 公開は main 側の LYKURO_METRICS_ENABLED でゲートする。
	metrics *Metrics
}

// Metrics returns the registry (metrics endpoint / agent hook 用)。
func (s *Server) Metrics() *Metrics { return s.metrics }

func NewServer(cfg *Config, audit *AuditLogger) *Server {
	return &Server{
		cfg:     cfg,
		auth:    NewAuthenticator(cfg),
		proxy:   newProxyCore(),
		audit:   audit,
		usage:   newUsageCounter(),
		metrics: newMetrics(),
	}
}

// config returns the current configuration snapshot.
func (s *Server) config() *Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// SetConfig atomically applies a new validated configuration (BD §13.2)。
func (s *Server) SetConfig(cfg *Config) {
	auth := NewAuthenticator(cfg)
	s.mu.Lock()
	s.cfg = cfg
	s.auth = auth
	hook := s.onConfigApplied
	s.mu.Unlock()
	if hook != nil {
		hook(cfg)
	}
}

// SetPlatformBackend installs (or clears) the platform backend。
func (s *Server) SetPlatformBackend(b contract.Backend) {
	s.mu.Lock()
	s.platform = b
	s.mu.Unlock()
}

// OnConfigApplied registers the config-change hook(platform 再構築用)。
func (s *Server) OnConfigApplied(fn func(*Config)) {
	s.mu.Lock()
	s.onConfigApplied = fn
	s.mu.Unlock()
}

// platformBackend returns the active backend if the platform route is on。
func (s *Server) platformBackend() contract.Backend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cfg.PlatformEnabled() && s.platform != nil {
		return s.platform
	}
	return nil
}

func (s *Server) authenticator() *Authenticator {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.auth
}

// RuntimeHealthy reports whether at least one runtime endpoint is reachable
// (Agent の heartbeat health_status 用、/readyz と同じ判定)。
func (s *Server) RuntimeHealthy(ctx context.Context) bool {
	endpoints := map[string]bool{}
	for _, m := range s.config().Models {
		for _, t := range m.Targets() {
			endpoints[t.Endpoint] = true
		}
	}
	for ep := range endpoints {
		if s.proxy.pingRuntime(ctx, ep) {
			return true
		}
	}
	return false
}

// Router builds the HTTP handler.
func (s *Server) Router() http.Handler {
	r := chi.NewRouter()

	// 運用エンドポイント(BD §9.1: healthz は認証不要・最小情報)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", s.handleReady)
	r.Get("/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": Version})
	})

	// OpenAI互換API(認証必須)
	r.Get("/v1/models", s.withAuth(s.handleModels))
	r.Post("/v1/chat/completions", s.withAuth(s.handleChat))
	r.Post("/v1/embeddings", s.withAuth(s.handleEmbeddings))
	// Phase 7: Responses API / rerank(透過。Runtime 側の対応が前提)
	r.Post("/v1/responses", s.withAuth(func(w http.ResponseWriter, r *http.Request, k *VirtualKeyDef) {
		s.routeInference(w, r, k, "/v1/responses")
	}))
	r.Post("/v1/rerank", s.withAuth(func(w http.ResponseWriter, r *http.Request, k *VirtualKeyDef) {
		s.routeInference(w, r, k, "/v1/rerank")
	}))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(w, http.StatusNotFound, "invalid_request", "unknown endpoint", requestID(r))
	})
	return r
}

// handleReady checks config and runtime reachability (BD §17.3)。
// 認証不要のため内部 endpoint URL は返さない — logical model 名のみ
// (物理情報の非開示は /v1/models と同じ方針、BD §9.5)。
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	cfg := s.config()
	// endpoint 単位で1回だけ ping し、結果を logical model へ写像する
	endpointUp := map[string]bool{}
	models := map[string]bool{}
	ready := false
	for _, m := range cfg.Models {
		healthy := false
		for _, t := range m.Targets() {
			up, checked := endpointUp[t.Endpoint]
			if !checked {
				up = s.proxy.pingRuntime(r.Context(), t.Endpoint)
				endpointUp[t.Endpoint] = up
			}
			if up {
				healthy = true
			}
		}
		models[m.LogicalName] = healthy
		if healthy {
			ready = true
		}
	}
	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"ready":  ready,
		"models": models,
	})
}

// withAuth performs steps 1-4 of the request flow (BD §12.4)。
func (s *Server) withAuth(next func(http.ResponseWriter, *http.Request, *VirtualKeyDef)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := ensureRequestID(r)
		w.Header().Set("X-Request-ID", reqID)

		key, ok := s.authenticator().Authenticate(r.Header.Get("Authorization"))
		if !ok {
			s.auditDenied(r, nil, http.StatusUnauthorized, "authentication_failed")
			writeAPIError(w, http.StatusUnauthorized, "authentication_failed",
				"invalid or missing virtual key", reqID)
			return
		}
		if !s.authenticator().CheckRate(key) {
			s.auditDenied(r, key, http.StatusTooManyRequests, "rate_limit_exceeded")
			w.Header().Set("Retry-After", "60")
			writeAPIError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
				"requests per minute limit exceeded", reqID)
			return
		}
		// データ分類・経路ポリシー(BD §11): 宣言された class がこの Gateway で
		// 許可されていなければ拒否。hybrid 要求は MVP(Strict Local)では常に拒否。
		if dc := r.Header.Get("X-Data-Class"); dc != "" && !s.config().DataClassAllowed(dc) {
			s.auditDenied(r, key, http.StatusForbidden, "policy_denied")
			writeAPIError(w, http.StatusForbidden, "policy_denied",
				"declared data class is not allowed on this gateway", reqID)
			return
		}
		if rm := r.Header.Get("X-Routing-Mode"); rm == "hybrid" && s.config().StrictLocal() {
			s.auditDenied(r, key, http.StatusForbidden, "policy_denied")
			writeAPIError(w, http.StatusForbidden, "policy_denied",
				"hybrid routing is disabled (strict local mode)", reqID)
			return
		}
		next(w, r, key)
	}
}

// handleModels lists logical models only (BD §9.5: 物理モデル名を返さない)。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request, key *VirtualKeyDef) {
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	out := []modelEntry{}
	if backend := s.platformBackend(); backend != nil {
		for _, m := range backend.ListModels(r.Context()) {
			if key.AllowsModel(m.LogicalName) {
				out = append(out, modelEntry{ID: m.LogicalName, Object: "model", OwnedBy: "organization"})
			}
		}
	} else {
		for _, m := range s.config().Models {
			if key.AllowsModel(m.LogicalName) {
				out = append(out, modelEntry{ID: m.LogicalName, Object: "model", OwnedBy: "organization"})
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, key *VirtualKeyDef) {
	s.routeInference(w, r, key, "/v1/chat/completions")
}

func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request, key *VirtualKeyDef) {
	s.routeInference(w, r, key, "/v1/embeddings")
}

// routeInference selects the platform adapter or the legacy transparent
// proxy(feature flag、Phase 1: flag OFF で既存 route 完全維持)。
func (s *Server) routeInference(w http.ResponseWriter, r *http.Request, key *VirtualKeyDef, upstreamPath string) {
	if backend := s.platformBackend(); backend != nil {
		s.platformRequest(w, r, key, upstreamPath, backend)
		return
	}
	s.proxyRequest(w, r, key, upstreamPath)
}

func (s *Server) auditDenied(r *http.Request, key *VirtualKeyDef, status int, result string) {
	rec := &AuditRecord{
		Timestamp: time.Now().UTC(),
		RequestID: requestID(r),
		GatewayID: s.config().Gateway.ID,
		Endpoint:  r.URL.Path,
		DataClass: r.Header.Get("X-Data-Class"),
		Routing:   "local-only",
		Status:    status,
		Result:    result,
	}
	if key != nil {
		rec.VirtualKeyID = key.ID
	}
	if err := s.audit.Record(rec); err != nil {
		slog.Error("audit record failed", "error", err)
	}
	s.metrics.recordDenied(r.URL.Path, status, result)
}

// ---- helpers ----

func ensureRequestID(r *http.Request) string {
	id := r.Header.Get("X-Request-ID")
	if id == "" || len(id) > 128 {
		id = newRequestID()
	}
	r.Header.Set("X-Request-ID", id)
	return id
}

func requestID(r *http.Request) string {
	return r.Header.Get("X-Request-ID")
}

func newRequestID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "req_unknown"
	}
	return "req_" + hex.EncodeToString(buf)
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("write response failed", "error", err)
	}
}

// writeAPIError emits the OpenAI-compatible error shape (BD §9.4)。
func writeAPIError(w http.ResponseWriter, status int, code, message, reqID string) {
	typ := "invalid_request_error"
	switch code {
	case "authentication_failed":
		typ = "authentication_error"
	case "policy_denied":
		typ = "policy_error"
	case "rate_limit_exceeded":
		typ = "rate_limit_error"
	case "model_unavailable", "inference_timeout", "config_not_ready":
		typ = "service_error"
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message":    message,
			"type":       typ,
			"code":       code,
			"request_id": reqID,
		},
	})
}
