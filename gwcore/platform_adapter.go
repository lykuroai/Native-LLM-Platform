package gwcore

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lykuroai/Native-LLM-Platform/platform/contract"
)

// platform_adapter.go is the Gateway Platform Adapter (LYK-NLP-SD-001 v2.0
// Phase 1)。認証・レート・data class 検査は withAuth(BD §12.4 手順1-4)で
// 完了済みの前提で、policy context を確定して gateway-platform-v1 の
// Request を組み立てる。Virtual Key 原文は渡さない(§5.2)。

// Conversation headers (§13.1)。
const (
	headerConversationID      = "X-Lykuro-Conversation-ID"
	headerConversationVersion = "X-Lykuro-Conversation-Version"
	headerRetentionPolicy     = "X-Lykuro-Retention-Policy"
)

// platformRequest bridges one inference request to the platform backend。
func (s *Server) platformRequest(w http.ResponseWriter, r *http.Request, key *VirtualKeyDef, upstreamPath string, backend contract.Backend) {
	reqID := requestID(r)
	started := time.Now()
	cfg := s.config()

	body, err := io.ReadAll(io.LimitReader(r.Body, cfg.Gateway.MaxRequestBytes+1))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "failed to read request body", reqID)
		return
	}
	if int64(len(body)) > cfg.Gateway.MaxRequestBytes {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "request body too large", reqID)
		return
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "request body must be JSON", reqID)
		return
	}
	var logical string
	if raw, ok := payload["model"]; ok {
		_ = json.Unmarshal(raw, &logical)
	}
	if logical == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "model is required", reqID)
		return
	}
	// キーのモデル許可・ツール統制は Gateway 側 Policy(BD §8.2 / Phase 7)。
	if !key.AllowsModel(logical) {
		s.auditProxy(r, key, logical, "", "platform", http.StatusForbidden, "policy_denied", nil, nil, started)
		writeAPIError(w, http.StatusForbidden, "policy_denied",
			"the requested model is not allowed for this key", reqID)
		return
	}
	hasTools := false
	for _, f := range []string{"tools", "tool_choice", "functions", "function_call"} {
		if _, ok := payload[f]; ok {
			hasTools = true
			break
		}
	}
	if hasTools && !key.ToolsAllowed() {
		s.auditProxy(r, key, logical, "", "platform", http.StatusForbidden, "policy_denied", nil, nil, started)
		writeAPIError(w, http.StatusForbidden, "policy_denied",
			"tool use is not allowed for this key", reqID)
		return
	}
	streaming := false
	if raw, ok := payload["stream"]; ok {
		_ = json.Unmarshal(raw, &streaming)
	}
	// model=flow:{alias} は Workflow Orchestrator へ(MRCI-002 §5.1。
	// 通常 model 名は platformcfg で flow: prefix を予約済み)
	if strings.HasPrefix(logical, "flow:") && upstreamPath == "/v1/chat/completions" {
		s.workflowChat(w, r, key, logical, payload, streaming, started)
		return
	}
	delete(payload, "model") // logical は contract フィールドで渡す

	preq := &contract.Request{
		ContractVersion: contract.Version,
		RequestID:       reqID,
		TraceID:         reqID,
		TenantScope:     cfg.Gateway.ID,
		ActorScope:      key.ID,
		Policy: contract.PolicyContext{
			DataClass:     r.Header.Get("X-Data-Class"),
			RoutingMode:   r.Header.Get("X-Routing-Mode"),
			AllowedModels: key.AllowedModels,
			ToolsAllowed:  key.ToolsAllowed(),
		},
		LogicalModel:    logical,
		Endpoint:        upstreamPath,
		NormalizedInput: payload,
		Stream:          streaming,
		IdempotencyKey:  r.Header.Get("Idempotency-Key"),
	}
	if convID := r.Header.Get(headerConversationID); convID != "" {
		cc := &contract.ConversationContext{
			ID:              convID,
			RetentionPolicy: r.Header.Get(headerRetentionPolicy),
		}
		if v := r.Header.Get(headerConversationVersion); v != "" {
			if n, perr := strconv.ParseInt(v, 10, 64); perr == nil {
				cc.Version = n
			}
		}
		preq.Conversation = cc
	}

	if streaming {
		sw := &httpStreamWriter{w: w, onStart: func() {
			s.metrics.streamStarted(time.Since(started))
		}}
		defer func() {
			if sw.started {
				s.metrics.streamEnded()
			}
		}()
		result, cerr := backend.ExecuteStream(r.Context(), preq, sw)
		if cerr != nil {
			s.writePlatformError(w, r, key, logical, cerr, sw.started, started)
			return
		}
		if result.ConversationVersion > 0 {
			// SSE 開始後は header を出せないため trailer 相当は返さない —
			// クライアントは次回 GET /api か非stream で version を得る
			_ = result.ConversationVersion
		}
		outcome := "success"
		if result.ClientGone {
			outcome = "client_disconnected"
		}
		s.usage.add(logical, result.Usage.InputTokens, result.Usage.OutputTokens, false)
		s.auditProxy(r, key, logical, result.DeploymentID, "platform-"+result.RouteType,
			http.StatusOK, outcome, result.Usage.InputTokens, result.Usage.OutputTokens, started)
		return
	}

	resp, cerr := backend.Execute(r.Context(), preq)
	if cerr != nil {
		s.writePlatformError(w, r, key, logical, cerr, false, started)
		return
	}
	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	if resp.ConversationVersion > 0 {
		w.Header().Set(headerConversationVersion, strconv.FormatInt(resp.ConversationVersion, 10))
	}
	w.WriteHeader(resp.StatusCode)
	if _, werr := w.Write(resp.Body); werr != nil {
		// クライアント切断 — 監査は実施
		_ = werr
	}
	result := "success"
	if resp.StatusCode >= 400 {
		result = "upstream_" + strconv.Itoa(resp.StatusCode)
	}
	s.usage.add(logical, resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.StatusCode >= 400)
	s.auditProxy(r, key, logical, resp.DeploymentID, "platform-"+resp.RouteType,
		resp.StatusCode, result, resp.Usage.InputTokens, resp.Usage.OutputTokens, started)
}

// writePlatformError maps a frozen platform error to the OpenAI-compatible
// error shape (Phase 0 報告 §4.2 の凍結表)。SSE 開始後は event で終了済みの
// ため何も書かない。
func (s *Server) writePlatformError(w http.ResponseWriter, r *http.Request, key *VirtualKeyDef, logical string, cerr *contract.Error, sseStarted bool, started time.Time) {
	status := contract.HTTPStatus(cerr.Code)
	code := contract.GatewayErrorCode(cerr.Code)
	s.auditProxy(r, key, logical, "", "platform", status, code, nil, nil, started)
	if sseStarted {
		return
	}
	if cerr.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(cerr.RetryAfter))
	}
	writeAPIError(w, status, code, cerr.Message, requestID(r))
}

// httpStreamWriter adapts http.ResponseWriter to contract.StreamWriter。
type httpStreamWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	started bool
	// onStart is invoked once at first byte(TTFT 計測用)。
	onStart func()
}

func (h *httpStreamWriter) Start(statusCode int, contentType string) {
	if h.started {
		return
	}
	h.started = true
	if h.onStart != nil {
		h.onStart()
	}
	if contentType != "" {
		h.w.Header().Set("Content-Type", contentType)
	}
	h.flusher, _ = h.w.(http.Flusher)
	h.w.WriteHeader(statusCode)
}

func (h *httpStreamWriter) Chunk(p []byte) bool {
	if !h.started {
		h.Start(http.StatusOK, "text/event-stream")
	}
	_, err := h.w.Write(p)
	return err == nil
}

func (h *httpStreamWriter) Flush() {
	if h.flusher != nil {
		h.flusher.Flush()
	}
}
