package gwcore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// タイムアウト既定(CLAUDE.md 外部API方針と同じ): 接続10秒・全体15分。
const (
	connectTimeout = 10 * time.Second
	overallTimeout = 15 * time.Minute
	pingTimeout    = 2 * time.Second
)

type proxyCore struct {
	client *http.Client
}

func newProxyCore() *proxyCore {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: connectTimeout}).DialContext,
		ResponseHeaderTimeout: overallTimeout,
		MaxIdleConnsPerHost:   16,
	}
	return &proxyCore{client: &http.Client{Transport: transport, Timeout: overallTimeout}}
}

// pingRuntime checks basic reachability of a runtime endpoint.
func (p *proxyCore) pingRuntime(ctx context.Context, endpoint string) bool {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/models", nil)
	if err != nil {
		return false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// 認証エラーでも到達はしている
	return resp.StatusCode < 500
}

// proxyRequest implements steps 5-11 of the request flow (BD §12.4):
// モデル解決 → model 書換 → 透過転送(SSE含む) → usage sniff → 監査。
func (s *Server) proxyRequest(w http.ResponseWriter, r *http.Request, key *VirtualKeyDef, upstreamPath string) {
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

	// model と stream だけを解釈し、他フィールドは触らない
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

	// モデル解決(未知は明示 404)→ キーのモデル許可(BD §8.2)
	def := cfg.FindModel(logical)
	if def == nil {
		s.auditProxy(r, key, logical, "", "local", http.StatusNotFound, "model_not_found", nil, nil, started)
		writeAPIError(w, http.StatusNotFound, "invalid_request",
			"unknown model "+logical, reqID)
		return
	}
	if !key.AllowsModel(logical) {
		s.auditProxy(r, key, logical, "", "local", http.StatusForbidden, "policy_denied", nil, nil, started)
		writeAPIError(w, http.StatusForbidden, "policy_denied",
			"the requested model is not allowed for this key", reqID)
		return
	}
	// ツール統制(Phase 7): キーが tools を許可していなければ拒否。
	// 旧OpenAI形式(functions/function_call)も抜け穴にしない。
	hasTools := false
	for _, f := range []string{"tools", "tool_choice", "functions", "function_call"} {
		if _, ok := payload[f]; ok {
			hasTools = true
			break
		}
	}
	if hasTools && !key.ToolsAllowed() {
		s.auditProxy(r, key, logical, "", "local", http.StatusForbidden, "policy_denied", nil, nil, started)
		writeAPIError(w, http.StatusForbidden, "policy_denied",
			"tool use is not allowed for this key", reqID)
		return
	}

	streaming := false
	if raw, ok := payload["stream"]; ok {
		_ = json.Unmarshal(raw, &streaming)
	}

	// 試行順: ローカル候補(BD §12.3 fallback group) → 適格時のみ承認cloud
	// (Phase 7 Hybrid、ポリシー最優先: local-only ヘッダ・未宣言class・
	// confidential/restricted は cloud へ出さない — CloudEligible 参照)。
	type attempt struct {
		endpoint string
		model    string
		auth     string
		routing  string
	}
	var attempts []attempt
	for _, t := range def.Targets() {
		attempts = append(attempts, attempt{endpoint: t.Endpoint, model: t.PhysicalModel, routing: "local"})
	}
	if cfg.CloudEligible(def, r.Header.Get("X-Data-Class"), r.Header.Get("X-Routing-Mode")) {
		if cred, kerr := os.ReadFile(cfg.Hybrid.APIKeyFile); kerr != nil {
			slog.Warn("hybrid cloud key unavailable, staying local", "error", kerr)
		} else {
			attempts = append(attempts, attempt{
				endpoint: cfg.Hybrid.Endpoint,
				model:    def.CloudFallback.Model,
				auth:     strings.TrimSpace(string(cred)),
				routing:  "hybrid-cloud",
			})
		}
	}

	var resp *http.Response
	var used attempt
	var lastErr error
	for i, at := range attempts {
		// model の書換(これ以外の body 改変はしない)
		physRaw, merr := json.Marshal(at.model)
		if merr != nil {
			continue
		}
		payload["model"] = physRaw
		upstreamBody, merr := json.Marshal(payload)
		if merr != nil {
			writeAPIError(w, http.StatusInternalServerError, "server_error", "failed to rebuild request", reqID)
			return
		}
		upReq, rerr := http.NewRequestWithContext(r.Context(), http.MethodPost,
			at.endpoint+upstreamPath, bytes.NewReader(upstreamBody))
		if rerr != nil {
			writeAPIError(w, http.StatusInternalServerError, "server_error", "failed to build upstream request", reqID)
			return
		}
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("X-Request-ID", reqID)
		if at.auth != "" {
			upReq.Header.Set("Authorization", "Bearer "+at.auth)
		}
		if accept := r.Header.Get("Accept"); accept != "" {
			upReq.Header.Set("Accept", accept)
		}
		got, derr := s.proxy.client.Do(upReq)
		if derr != nil {
			lastErr = derr
			if isTimeoutErr(derr) {
				// タイムアウトは推論が開始済みの可能性がある。次候補へ再送すると
				// 推論の二重実行(cloud なら二重課金)になるため failover せず
				// 504 で返す。切替対象は到達不可(dial 失敗等)のみ。
				break
			}
			continue // 到達不可 → 次候補(BD §21)
		}
		if got.StatusCode == http.StatusServiceUnavailable && i < len(attempts)-1 {
			got.Body.Close()
			lastErr = fmt.Errorf("upstream returned 503")
			continue // Runtime未ロード → 次候補。最終候補の503は透過
		}
		resp = got
		used = at
		break
	}
	if resp == nil {
		status, code, msg := upstreamFailure(lastErr)
		s.auditProxy(r, key, logical, def.PhysicalModel, "local", status, code, nil, nil, started)
		writeAPIError(w, status, code, msg, reqID)
		return
	}
	defer resp.Body.Close()

	// レスポンス透過(SSE含む)。usage は覗き見のみ。
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)

	var inTok, outTok *int64
	bodyError := false
	if streaming && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		// TTFT は上流応答ヘッダ到達(=先頭バイト直前)を近似とする
		s.metrics.streamStarted(time.Since(started))
		inTok, outTok = streamAndSniff(w, resp.Body)
		s.metrics.streamEnded()
	} else {
		respBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			// 上流が本文送出中に切断(status 送信済みのため 200+空bodyになる)。
			// 監査を success にしない — 運用が SDK 側エラーと突き合わせられる
			// ように明示コードで記録する。
			bodyError = true
			slog.Error("upstream body read failed",
				"request_id", reqID, "physical_model", used.model, "error", readErr)
		} else {
			if _, err := w.Write(respBody); err != nil {
				slog.Debug("client write failed", "error", err)
			}
			inTok, outTok = sniffUsage(respBody)
		}
	}

	result := "success"
	switch {
	case bodyError:
		result = "upstream_body_error"
	case resp.StatusCode >= 400:
		result = fmt.Sprintf("upstream_%d", resp.StatusCode)
	}
	s.usage.add(logical, inTok, outTok, bodyError || resp.StatusCode >= 400)
	s.auditProxy(r, key, logical, used.model, used.routing, resp.StatusCode, result, inTok, outTok, started)
}

// isTimeoutErr reports whether the transport error is a timeout
// (context deadline 含む)。failover 可否の判定に使う。
func isTimeoutErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// upstreamFailure maps transport errors (BD §21: 到達不可時のみ Gateway が
// エラーを生成、それ以外は上流を透過)。
func upstreamFailure(err error) (int, string, string) {
	if err == nil {
		return http.StatusServiceUnavailable, "model_unavailable", "no runtime candidate available"
	}
	if isTimeoutErr(err) {
		return http.StatusGatewayTimeout, "inference_timeout", "inference did not complete in time"
	}
	return http.StatusServiceUnavailable, "model_unavailable",
		"runtime is unreachable: " + err.Error()
}

// streamAndSniff copies SSE to the client while scanning data chunks for a
// usage object (本文は保持しない)。
func streamAndSniff(w http.ResponseWriter, body io.Reader) (inTok, outTok *int64) {
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if _, err := w.Write(append(line, '\n')); err != nil {
			return inTok, outTok // クライアント切断
		}
		if flusher != nil && len(line) == 0 {
			flusher.Flush() // イベント境界で送出
		}
		if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok && bytes.Contains(data, []byte(`"usage"`)) {
			if i, o := sniffUsage(data); i != nil || o != nil {
				inTok, outTok = i, o
			}
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	return inTok, outTok
}

// sniffUsage extracts usage token counts from an OpenAI-compatible payload.
// Chat Completions 形式(prompt_tokens/completion_tokens)と Responses API 形式
// (input_tokens/output_tokens)の両方に対応する。
func sniffUsage(data []byte) (inTok, outTok *int64) {
	var probe struct {
		Usage *usageProbe `json:"usage"`
		// Responses API のSSEは response.completed イベントで
		// {"response":{...,"usage":{...}}} の形を取る。
		Response *struct {
			Usage *usageProbe `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, nil
	}
	u := probe.Usage
	if u == nil && probe.Response != nil {
		u = probe.Response.Usage
	}
	if u == nil {
		return nil, nil
	}
	in := u.PromptTokens + u.InputTokens
	out := u.CompletionTokens + u.OutputTokens
	return &in, &out
}

type usageProbe struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
}

func (s *Server) auditProxy(r *http.Request, key *VirtualKeyDef, logical, physical, routing string, status int, result string, inTok, outTok *int64, started time.Time) {
	rec := &AuditRecord{
		Timestamp:     time.Now().UTC(),
		RequestID:     requestID(r),
		GatewayID:     s.config().Gateway.ID,
		VirtualKeyID:  key.ID,
		Endpoint:      r.URL.Path,
		LogicalModel:  logical,
		PhysicalModel: physical,
		DataClass:     r.Header.Get("X-Data-Class"),
		Routing:       routing,
		Status:        status,
		Result:        result,
		InputTokens:   inTok,
		OutputTokens:  outTok,
		LatencyMS:     time.Since(started).Milliseconds(),
	}
	if err := s.audit.Record(rec); err != nil {
		slog.Error("audit record failed", "error", err)
	}
	s.metrics.recordRequest(r.URL.Path, status, result, logical, inTok, outTok, time.Since(started))
}
