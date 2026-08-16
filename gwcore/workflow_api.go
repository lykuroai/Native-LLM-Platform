package gwcore

// workflow_api.go — Workflow Orchestrator のデータプレーン API
// (LYK-NLP-MRCI-002 §5.1)。POST /v1/workflows/{alias}/runs、Run 照会、
// SSE Event(Last-Event-ID 再開)、Cancel、および /v1/chat/completions の
// model=flow:{alias} 経路を提供する。
//
// 認可は 2 層(MRCI-002 §8): 実行系は Virtual Key(allowed_models に
// "flow:{alias}")、Run の照会・Cancel は実行したキー自身のみ。
// Node/Deployment ID・中間本文はデータプレーンへ返さない。

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lykuroai/Native-LLM-Platform/platform/workflow"
)

// syncWaitCap is the sync-mode gateway limit (MRCI-001 §11.8: 60秒)。
const syncWaitCap = 60 * time.Second

// SetWorkflowService installs (or clears) the workflow service。
func (s *Server) SetWorkflowService(ws *workflow.Service) {
	s.mu.Lock()
	s.workflows = ws
	s.mu.Unlock()
}

// workflowService returns the active service, or nil when disabled。
func (s *Server) workflowService() *workflow.Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.cfg.PlatformEnabled() || s.platform == nil {
		return nil
	}
	return s.workflows
}

// workflowHTTPStatus maps workflow error codes to HTTP (MRCI-002 §5.3)。
func workflowHTTPStatus(code string) int {
	switch code {
	case workflow.CodeNotFound:
		return http.StatusNotFound
	case workflow.CodeInputValidation, workflow.CodeTemplateRender:
		return http.StatusBadRequest
	case workflow.CodeNotPublished, workflow.CodeNoEligible, workflow.CodeBudgetExceeded:
		return http.StatusUnprocessableEntity
	case workflow.CodeIdemConflict, workflow.CodeRevConflict:
		return http.StatusConflict
	case workflow.CodeCapacity:
		return http.StatusTooManyRequests
	case workflow.CodeStepTimeout:
		return http.StatusGatewayTimeout
	case workflow.CodeInvalidResponse:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) writeWorkflowError(w http.ResponseWriter, r *http.Request, rerr *workflow.RunError) {
	if rerr.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(rerr.RetryAfter))
	}
	writeAPIError(w, workflowHTTPStatus(rerr.Code), rerr.Code, rerr.Message, requestID(r))
}

// startRunRequest is the POST /v1/workflows/{alias}/runs body (§11.5 縮退版)。
type startRunRequest struct {
	FlowVersion  int               `json:"flow_version,omitempty"`
	Inputs       map[string]string `json:"inputs"`
	ResponseMode string            `json:"response_mode,omitempty"`
	TimeoutMS    int               `json:"timeout_ms,omitempty"`
	Metadata     map[string]any    `json:"metadata,omitempty"` // 受理するが保存しない(W1)
}

func (s *Server) handleWorkflowStart(w http.ResponseWriter, r *http.Request, key *VirtualKeyDef) {
	reqID := requestID(r)
	svc := s.workflowService()
	if svc == nil {
		writeAPIError(w, http.StatusNotFound, "invalid_request", "workflow support is not enabled", reqID)
		return
	}
	alias := chi.URLParam(r, "alias")
	if !key.AllowsModel("flow:" + alias) {
		s.auditDenied(r, key, http.StatusForbidden, "policy_denied")
		writeAPIError(w, http.StatusForbidden, "policy_denied",
			"this flow is not allowed for this key", reqID)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, s.config().Gateway.MaxRequestBytes+1))
	if err != nil || int64(len(body)) > s.config().Gateway.MaxRequestBytes {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "request body unreadable or too large", reqID)
		return
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	var req startRunRequest
	if err := dec.Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid run request: "+err.Error(), reqID)
		return
	}
	mode := req.ResponseMode
	if mode == "" {
		mode = "sync"
	}
	if mode != "sync" && mode != "stream" {
		// async は W1 対象外(MRCI-002 §10-2)— 受理して無視しない
		writeAPIError(w, http.StatusBadRequest, "invalid_request",
			"response_mode must be sync or stream", reqID)
		return
	}

	run, rerr := svc.StartRun(workflow.StartParams{
		Alias:        alias,
		FlowVersion:  req.FlowVersion,
		Inputs:       req.Inputs,
		ResponseMode: mode,
		KeyID:        key.ID,
		RequestID:    reqID,
		DataClass:    r.Header.Get("X-Data-Class"),
		IdemKey:      r.Header.Get("Idempotency-Key"),
	})
	if rerr != nil {
		s.auditWorkflowDenied(r, key, alias, rerr)
		s.writeWorkflowError(w, r, rerr)
		return
	}
	if run.RequestID == reqID {
		// 新規 Run のみ監査・メトリクスを終了時に記録(Idempotency 再利用は除く)
		go s.watchWorkflowRun(svc, run, alias, key.ID, r.Header.Get("X-Data-Class"), r.URL.Path)
	}

	if mode == "stream" {
		s.streamWorkflowRun(w, r, svc, run, 0)
		return
	}

	wait := syncWaitCap
	if req.TimeoutMS > 0 && time.Duration(req.TimeoutMS)*time.Millisecond < wait {
		wait = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	select {
	case <-run.Done():
		s.writeRunResult(w, svc, run.ID, http.StatusOK)
	case <-time.After(wait):
		// sync 上限超過: 202 + run_id(GET / SSE で追跡)。Run は継続する
		s.writeRunResult(w, svc, run.ID, http.StatusAccepted)
	case <-r.Context().Done():
		// クライアント切断 — Run は継続、監査は watch 側で記録
	}
}

// writeRunResult renders the §11.7 run result。最終 Output は COMPLETED かつ
// メモリ保持中のみ含む。
func (s *Server) writeRunResult(w http.ResponseWriter, svc *workflow.Service, runID string, status int) {
	snap, err := svc.Runs().Snapshot(runID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, workflow.CodeNotFound, "run not found", "")
		return
	}
	out := map[string]any{
		"run_id":       snap.ID,
		"flow_id":      snap.FlowID,
		"alias":        snap.Alias,
		"flow_version": snap.FlowVersion,
		"status":       snap.Status,
		"usage": map[string]int64{
			"input_tokens":  snap.InputTokens,
			"output_tokens": snap.OutputTokens,
			"total_tokens":  snap.InputTokens + snap.OutputTokens,
		},
		"created_at": snap.CreatedAt,
	}
	if snap.ErrorCode != "" {
		out["error_code"] = snap.ErrorCode
	}
	if snap.CompletedAt != nil {
		out["completed_at"] = snap.CompletedAt
		out["duration_ms"] = snap.CompletedAt.Sub(snap.CreatedAt).Milliseconds()
	}
	if text, ok := svc.Runs().Output(snap.ID); ok {
		out["output"] = map[string]string{"text": text}
	}
	writeJSON(w, status, out)
}

func (s *Server) handleWorkflowRunGet(w http.ResponseWriter, r *http.Request, key *VirtualKeyDef) {
	svc, runID, ok := s.ownRun(w, r, key)
	if !ok {
		return
	}
	s.writeRunResult(w, svc, runID, http.StatusOK)
}

func (s *Server) handleWorkflowRunSteps(w http.ResponseWriter, r *http.Request, key *VirtualKeyDef) {
	svc, runID, ok := s.ownRun(w, r, key)
	if !ok {
		return
	}
	snap, err := svc.Runs().Snapshot(runID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, workflow.CodeNotFound, "run not found", requestID(r))
		return
	}
	type decision struct {
		Attempt      int    `json:"attempt"`
		LogicalModel string `json:"logical_model"`
		PoolID       string `json:"pool_id,omitempty"`
		FallbackUsed bool   `json:"fallback_used"`
		ErrorCode    string `json:"error_code,omitempty"`
	}
	type stepView struct {
		StepID       string     `json:"step_id"`
		Status       string     `json:"status"`
		Attempts     int        `json:"attempts"`
		InputTokens  int64      `json:"input_tokens"`
		OutputTokens int64      `json:"output_tokens"`
		ErrorCode    string     `json:"error_code,omitempty"`
		StartedAt    *time.Time `json:"started_at,omitempty"`
		CompletedAt  *time.Time `json:"completed_at,omitempty"`
		Decisions    []decision `json:"route_decisions,omitempty"`
	}
	steps := make([]stepView, 0, len(snap.Steps))
	for _, st := range snap.Steps {
		sv := stepView{StepID: st.StepID, Status: st.Status, Attempts: st.Attempts,
			InputTokens: st.InputTokens, OutputTokens: st.OutputTokens,
			ErrorCode: st.ErrorCode, StartedAt: st.StartedAt, CompletedAt: st.CompletedAt}
		for _, d := range st.Decisions {
			// Node/Deployment ID はデータプレーンへ返さない(§9.2)
			sv.Decisions = append(sv.Decisions, decision{Attempt: d.Attempt,
				LogicalModel: d.LogicalModel, PoolID: d.PoolID,
				FallbackUsed: d.FallbackUsed, ErrorCode: d.ErrorCode})
		}
		steps = append(steps, sv)
	}
	writeJSON(w, http.StatusOK, map[string]any{"run_id": snap.ID, "steps": steps})
}

func (s *Server) handleWorkflowRunEvents(w http.ResponseWriter, r *http.Request, key *VirtualKeyDef) {
	svc, runID, ok := s.ownRun(w, r, key)
	if !ok {
		return
	}
	var lastSeq int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		lastSeq, _ = strconv.ParseInt(v, 10, 64)
	} else if v := r.URL.Query().Get("last_event_id"); v != "" {
		lastSeq, _ = strconv.ParseInt(v, 10, 64)
	}
	run, err := svc.Runs().Get(runID)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, workflow.CodeNotFound, "run not found", requestID(r))
		return
	}
	s.streamWorkflowRun(w, r, svc, run, lastSeq)
}

// streamWorkflowRun serves the SSE event stream。POST(response_mode=stream)
// の応答としても、GET /events の再接続でも使う。終了イベント後、COMPLETED
// なら最終 Output を workflow.output イベントとして応答ストリームへ載せる
// (応答経路であり Event 保存ではない — ディスクへは書かれない)。
func (s *Server) streamWorkflowRun(w http.ResponseWriter, r *http.Request, svc *workflow.Service, run *workflow.Run, lastSeq int64) {
	replay, live, unsub, err := svc.Runs().Subscribe(run.ID, lastSeq)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, workflow.CodeNotFound, "run not found", requestID(r))
		return
	}
	defer unsub()
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	writeEvent := func(ev workflow.Event) bool {
		data, jerr := json.Marshal(map[string]any{"run_id": run.ID, "type": ev.Type,
			"schema_version": ev.SchemaVersion, "data": ev.Data, "at": ev.At})
		if jerr != nil {
			return false
		}
		if _, werr := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, data); werr != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	finish := func(terminalType string) {
		if terminalType == "workflow.completed" {
			if text, ok := svc.Runs().Output(run.ID); ok {
				data, _ := json.Marshal(map[string]any{"run_id": run.ID, "text": text})
				fmt.Fprintf(w, "event: workflow.output\ndata: %s\n\n", data)
			}
		}
		fmt.Fprint(w, "event: done\ndata: {}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}

	terminal := map[string]bool{"workflow.completed": true, "workflow.failed": true,
		"workflow.cancelled": true}
	for _, ev := range replay {
		if !writeEvent(ev) {
			return
		}
		if terminal[ev.Type] {
			finish(ev.Type)
			return
		}
	}
	for {
		select {
		case ev := <-live:
			if !writeEvent(ev) {
				return
			}
			if terminal[ev.Type] {
				finish(ev.Type)
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) handleWorkflowRunCancel(w http.ResponseWriter, r *http.Request, key *VirtualKeyDef) {
	svc, runID, ok := s.ownRun(w, r, key)
	if !ok {
		return
	}
	if err := svc.CancelRun(runID); err != nil {
		writeAPIError(w, http.StatusNotFound, workflow.CodeNotFound, "run not found", requestID(r))
		return
	}
	snap, _ := svc.Runs().Snapshot(runID)
	status := workflow.RunCancelling
	if snap != nil {
		status = snap.Status
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": runID, "status": status})
}

// ownRun resolves the service + run and enforces per-key ownership。
// 他キーの Run は存在有無を漏らさない(404)。
func (s *Server) ownRun(w http.ResponseWriter, r *http.Request, key *VirtualKeyDef) (*workflow.Service, string, bool) {
	reqID := requestID(r)
	svc := s.workflowService()
	if svc == nil {
		writeAPIError(w, http.StatusNotFound, "invalid_request", "workflow support is not enabled", reqID)
		return nil, "", false
	}
	runID := chi.URLParam(r, "run_id")
	snap, err := svc.Runs().Snapshot(runID)
	if err != nil || snap.KeyID != key.ID {
		writeAPIError(w, http.StatusNotFound, workflow.CodeNotFound, "run not found", reqID)
		return nil, "", false
	}
	return svc, runID, true
}

// watchWorkflowRun records audit・usage・metrics at run completion
// (client 切断でも Run は走り切るため handler ではなくここで確定する)。
func (s *Server) watchWorkflowRun(svc *workflow.Service, run *workflow.Run, alias, keyID, dataClass, endpoint string) {
	<-run.Done()
	snap, err := svc.Runs().Snapshot(run.ID)
	if err != nil {
		return
	}
	status := http.StatusOK
	result := "success"
	switch snap.Status {
	case workflow.RunFailed:
		status, result = http.StatusInternalServerError, snap.ErrorCode
	case workflow.RunTimedOut:
		status, result = http.StatusGatewayTimeout, snap.ErrorCode
	case workflow.RunCancelled:
		result = "cancelled"
	}
	fallbackUsed := false
	for _, st := range snap.Steps {
		for _, d := range st.Decisions {
			if d.FallbackUsed {
				fallbackUsed = true
			}
		}
	}
	in, out := snap.InputTokens, snap.OutputTokens
	rec := &AuditRecord{
		Timestamp:    time.Now().UTC(),
		RequestID:    snap.RequestID,
		GatewayID:    s.config().Gateway.ID,
		VirtualKeyID: keyID,
		Endpoint:     endpoint,
		LogicalModel: "flow:" + alias,
		DataClass:    dataClass,
		Routing:      "workflow",
		Status:       status,
		Result:       result,
		InputTokens:  &in,
		OutputTokens: &out,
		LatencyMS:    time.Since(snap.CreatedAt).Milliseconds(),
	}
	if err := s.audit.Record(rec); err != nil {
		slog.Error("workflow audit record failed", "error", err)
	}
	s.usage.add("flow:"+alias, &in, &out, snap.Status != workflow.RunCompleted)
	s.metrics.recordWorkflowRun(snap.Status, fallbackUsed)
}

// auditWorkflowDenied records a rejected run request。
func (s *Server) auditWorkflowDenied(r *http.Request, key *VirtualKeyDef, alias string, rerr *workflow.RunError) {
	rec := &AuditRecord{
		Timestamp:    time.Now().UTC(),
		RequestID:    requestID(r),
		GatewayID:    s.config().Gateway.ID,
		VirtualKeyID: key.ID,
		Endpoint:     r.URL.Path,
		LogicalModel: "flow:" + alias,
		DataClass:    r.Header.Get("X-Data-Class"),
		Routing:      "workflow",
		Status:       workflowHTTPStatus(rerr.Code),
		Result:       rerr.Code,
	}
	if err := s.audit.Record(rec); err != nil {
		slog.Error("workflow audit record failed", "error", err)
	}
}

// --- OpenAI 互換 flow:{alias} 経路(§11.10 縮退版) ---

// workflowChat executes a published flow behind /v1/chat/completions。
// W1 は「必須 Input が 1 つの Flow」のみ対応 — 最後の user message を
// その Input へ写像する。複数 Input の Flow は Native Workflow API のみ。
func (s *Server) workflowChat(w http.ResponseWriter, r *http.Request, key *VirtualKeyDef, logical string, payload map[string]json.RawMessage, streaming bool, started time.Time) {
	reqID := requestID(r)
	svc := s.workflowService()
	if svc == nil || !svc.AliasEnabled() {
		writeAPIError(w, http.StatusNotFound, "model_unavailable", "unknown model", reqID)
		return
	}
	alias := strings.TrimPrefix(logical, "flow:")

	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if raw, ok := payload["messages"]; ok {
		if err := json.Unmarshal(raw, &msgs); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid_request", "messages must be an array", reqID)
			return
		}
	}
	var userText string
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			if json.Unmarshal(msgs[i].Content, &userText) != nil {
				writeAPIError(w, http.StatusBadRequest, "invalid_request",
					"structured message content is not supported for flow models", reqID)
				return
			}
			break
		}
	}
	if userText == "" {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "a user message is required", reqID)
		return
	}
	inputName, rerr := svc.SingleInputName(alias)
	if rerr != nil {
		s.auditWorkflowDenied(r, key, alias, rerr)
		s.writeWorkflowError(w, r, rerr)
		return
	}

	run, rerr := svc.StartRun(workflow.StartParams{
		Alias:        alias,
		Inputs:       map[string]string{inputName: userText},
		ResponseMode: "sync",
		KeyID:        key.ID,
		RequestID:    reqID,
		DataClass:    r.Header.Get("X-Data-Class"),
		IdemKey:      r.Header.Get("Idempotency-Key"),
	})
	if rerr != nil {
		s.auditWorkflowDenied(r, key, alias, rerr)
		s.writeWorkflowError(w, r, rerr)
		return
	}
	if run.RequestID == reqID {
		go s.watchWorkflowRun(svc, run, alias, key.ID, r.Header.Get("X-Data-Class"), r.URL.Path)
	}

	select {
	case <-run.Done():
	case <-time.After(syncWaitCap):
		writeAPIError(w, http.StatusGatewayTimeout, "inference_timeout",
			"workflow did not finish within the chat completion window; use the workflow API", reqID)
		return
	case <-r.Context().Done():
		return
	}
	snap, err := svc.Runs().Snapshot(run.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "server_error", "run lost", reqID)
		return
	}
	if snap.Status != workflow.RunCompleted {
		code := snap.ErrorCode
		if code == "" {
			code = workflow.CodeInternal
		}
		writeAPIError(w, workflowHTTPStatus(code), code, "workflow run "+strings.ToLower(snap.Status), reqID)
		return
	}
	text, _ := svc.Runs().Output(run.ID)
	usage := map[string]int64{
		"prompt_tokens":     snap.InputTokens,
		"completion_tokens": snap.OutputTokens,
		"total_tokens":      snap.InputTokens + snap.OutputTokens,
	}
	if streaming {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		s.metrics.streamStarted(time.Since(started))
		defer s.metrics.streamEnded()
		chunk := func(delta map[string]any, finish any) {
			data, _ := json.Marshal(map[string]any{
				"id": snap.ID, "object": "chat.completion.chunk",
				"created": snap.CreatedAt.Unix(), "model": logical,
				"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
			})
			fmt.Fprintf(w, "data: %s\n\n", data)
			if flusher != nil {
				flusher.Flush()
			}
		}
		chunk(map[string]any{"role": "assistant", "content": text}, nil)
		chunk(map[string]any{}, "stop")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": snap.ID, "object": "chat.completion",
		"created": snap.CreatedAt.Unix(), "model": logical,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": usage,
	})
}
