package gwcore

// admin_workflow.go — Workflow(Inference Flow)の管理プレーン API
// (MRCI-002 §5.2。admin listener 専用・admin-token 認証)。
//
// 管理面は特権面として Run の Route Decision(Deployment ID 含む)まで
// 返すが、Step Input/Output 本文を読む・返す経路は持たない(Zero-Retention
// は管理面でも不変 — admin.go の設計方針と同じ)。

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lykuroai/Native-LLM-Platform/platform/workflow"
)

// registerWorkflowRoutes mounts /api/workflows* on the admin router。
func (a *adminAPI) registerWorkflowRoutes(r chi.Router) {
	r.Get("/api/workflows", a.withAuth(a.handleFlowList))
	r.Post("/api/workflows", a.withAuth(a.handleFlowCreate))
	r.Get("/api/workflows/{id}", a.withAuth(a.handleFlowGet))
	r.Patch("/api/workflows/{id}", a.withAuth(a.handleFlowPatch))
	r.Post("/api/workflows/{id}/validate", a.withAuth(a.handleFlowValidate))
	r.Post("/api/workflows/{id}/publish", a.withAuth(a.handleFlowPublish))
	r.Get("/api/workflows/{id}/versions", a.withAuth(a.handleFlowVersions))
	r.Get("/api/workflows/{id}/versions/{version}", a.withAuth(a.handleFlowVersionGet))
	r.Post("/api/workflows/{id}/status", a.withAuth(a.handleFlowStatus))
	r.Get("/api/workflow-runs", a.withAuth(a.handleRunList))
	r.Get("/api/workflow-runs/{run_id}", a.withAuth(a.handleRunGet))
	r.Post("/api/workflow-runs/{run_id}/cancel", a.withAuth(a.handleRunCancel))
}

// adminWorkflowService returns the service or writes 503。
func (a *adminAPI) adminWorkflowService(w http.ResponseWriter, r *http.Request) *workflow.Service {
	svc := a.srv.workflowService()
	if svc == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "invalid_request",
			"workflows are not enabled (platform.workflows.enabled)", requestID(r))
	}
	return svc
}

// writeFlowStoreError maps store errors to HTTP。
func writeFlowStoreError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	code := "internal_error"
	switch {
	case errors.Is(err, workflow.ErrFlowNotFound), errors.Is(err, workflow.ErrVersionNotFound):
		status, code = http.StatusNotFound, workflow.CodeNotFound
	case errors.Is(err, workflow.ErrRevisionConflict):
		status, code = http.StatusConflict, workflow.CodeRevConflict
	case errors.Is(err, workflow.ErrAliasTaken), errors.Is(err, workflow.ErrFlowRetired):
		status, code = http.StatusConflict, "invalid_request"
	case errors.Is(err, workflow.ErrTooManyFlows):
		status, code = http.StatusUnprocessableEntity, "invalid_request"
	}
	writeAPIError(w, status, code, err.Error(), requestID(r))
}

func errStrings(errs []error) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e.Error())
	}
	return out
}

func (a *adminAPI) handleFlowList(w http.ResponseWriter, r *http.Request) {
	svc := a.adminWorkflowService(w, r)
	if svc == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flows": svc.Flows().List()})
}

func (a *adminAPI) handleFlowCreate(w http.ResponseWriter, r *http.Request) {
	svc := a.adminWorkflowService(w, r)
	if svc == nil {
		return
	}
	var body struct {
		Definition json.RawMessage `json:"definition"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || len(body.Definition) == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "definition is required", requestID(r))
		return
	}
	meta, errs := svc.CreateFlow(body.Definition)
	if len(errs) > 0 {
		if len(errs) == 1 {
			writeFlowStoreError(w, r, errs[0])
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"errors": errStrings(errs)})
		return
	}
	a.auditAdmin(r, "admin_workflow_created")
	writeJSON(w, http.StatusOK, meta)
}

func (a *adminAPI) handleFlowGet(w http.ResponseWriter, r *http.Request) {
	svc := a.adminWorkflowService(w, r)
	if svc == nil {
		return
	}
	id := chi.URLParam(r, "id")
	meta, err := svc.Flows().Get(id)
	if err != nil {
		writeFlowStoreError(w, r, err)
		return
	}
	draft, err := svc.Flows().GetDraft(id)
	if err != nil {
		writeFlowStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"meta": meta, "draft": draft})
}

func (a *adminAPI) handleFlowPatch(w http.ResponseWriter, r *http.Request) {
	svc := a.adminWorkflowService(w, r)
	if svc == nil {
		return
	}
	var body struct {
		Revision   int             `json:"revision"`
		Definition json.RawMessage `json:"definition"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil || len(body.Definition) == 0 {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "revision and definition are required", requestID(r))
		return
	}
	draft, errs := svc.UpdateFlow(chi.URLParam(r, "id"), body.Revision, body.Definition)
	if len(errs) > 0 {
		if len(errs) == 1 {
			writeFlowStoreError(w, r, errs[0])
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"errors": errStrings(errs)})
		return
	}
	a.auditAdmin(r, "admin_workflow_updated")
	writeJSON(w, http.StatusOK, draft)
}

func (a *adminAPI) handleFlowValidate(w http.ResponseWriter, r *http.Request) {
	svc := a.adminWorkflowService(w, r)
	if svc == nil {
		return
	}
	draft, err := svc.Flows().GetDraft(chi.URLParam(r, "id"))
	if err != nil {
		writeFlowStoreError(w, r, err)
		return
	}
	_, errs := svc.ValidateDefinition(draft.Definition)
	writeJSON(w, http.StatusOK, map[string]any{
		"valid":    len(errs) == 0,
		"revision": draft.Revision,
		"errors":   errStrings(errs),
	})
}

func (a *adminAPI) handleFlowPublish(w http.ResponseWriter, r *http.Request) {
	svc := a.adminWorkflowService(w, r)
	if svc == nil {
		return
	}
	var body struct {
		Revision int `json:"revision"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "revision is required", requestID(r))
		return
	}
	fv, errs := svc.PublishFlow(chi.URLParam(r, "id"), body.Revision)
	if len(errs) > 0 {
		if len(errs) == 1 {
			writeFlowStoreError(w, r, errs[0])
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"errors": errStrings(errs)})
		return
	}
	a.auditAdmin(r, "admin_workflow_published")
	writeJSON(w, http.StatusOK, map[string]any{
		"version": fv.Version, "checksum": fv.Checksum, "published_at": fv.PublishedAt,
	})
}

func (a *adminAPI) handleFlowVersions(w http.ResponseWriter, r *http.Request) {
	svc := a.adminWorkflowService(w, r)
	if svc == nil {
		return
	}
	versions, err := svc.Flows().Versions(chi.URLParam(r, "id"))
	if err != nil {
		writeFlowStoreError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

func (a *adminAPI) handleFlowVersionGet(w http.ResponseWriter, r *http.Request) {
	svc := a.adminWorkflowService(w, r)
	if svc == nil {
		return
	}
	ver, err := strconv.Atoi(chi.URLParam(r, "version"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "version must be an integer", requestID(r))
		return
	}
	fv, gerr := svc.Flows().GetVersion(chi.URLParam(r, "id"), ver)
	if gerr != nil {
		writeFlowStoreError(w, r, gerr)
		return
	}
	writeJSON(w, http.StatusOK, fv)
}

func (a *adminAPI) handleFlowStatus(w http.ResponseWriter, r *http.Request) {
	svc := a.adminWorkflowService(w, r)
	if svc == nil {
		return
	}
	var body struct {
		Status string `json:"status"` // published | suspended | retired
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "status is required", requestID(r))
		return
	}
	meta, err := svc.Flows().SetStatus(chi.URLParam(r, "id"), body.Status)
	if err != nil {
		writeFlowStoreError(w, r, err)
		return
	}
	a.auditAdmin(r, "admin_workflow_status_"+body.Status)
	writeJSON(w, http.StatusOK, meta)
}

func (a *adminAPI) handleRunList(w http.ResponseWriter, r *http.Request) {
	svc := a.adminWorkflowService(w, r)
	if svc == nil {
		return
	}
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	runs := svc.Runs().List(limit)
	type runRow struct {
		RunID       string     `json:"run_id"`
		Alias       string     `json:"alias"`
		FlowVersion int        `json:"flow_version"`
		Status      string     `json:"status"`
		KeyID       string     `json:"virtual_key_id,omitempty"`
		ErrorCode   string     `json:"error_code,omitempty"`
		InTokens    int64      `json:"input_tokens"`
		OutTokens   int64      `json:"output_tokens"`
		CreatedAt   time.Time  `json:"created_at"`
		CompletedAt *time.Time `json:"completed_at,omitempty"`
	}
	out := make([]runRow, 0, len(runs))
	for i := range runs {
		rr := &runs[i]
		out = append(out, runRow{RunID: rr.ID, Alias: rr.Alias, FlowVersion: rr.FlowVersion,
			Status: rr.Status, KeyID: rr.KeyID, ErrorCode: rr.ErrorCode,
			InTokens: rr.InputTokens, OutTokens: rr.OutputTokens,
			CreatedAt: rr.CreatedAt, CompletedAt: rr.CompletedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

// handleRunCancel cancels a run from the admin plane(冪等)。
func (a *adminAPI) handleRunCancel(w http.ResponseWriter, r *http.Request) {
	svc := a.adminWorkflowService(w, r)
	if svc == nil {
		return
	}
	runID := chi.URLParam(r, "run_id")
	if err := svc.CancelRun(runID); err != nil {
		writeAPIError(w, http.StatusNotFound, workflow.CodeNotFound, "run not found", requestID(r))
		return
	}
	a.auditAdmin(r, "admin_workflow_run_cancelled")
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": runID})
}

// handleRunGet returns the full run record(Route Decision の Deployment ID
// 含む特権ビュー。本文は含まれない — Run 構造自体が本文を持たない)。
func (a *adminAPI) handleRunGet(w http.ResponseWriter, r *http.Request) {
	svc := a.adminWorkflowService(w, r)
	if svc == nil {
		return
	}
	snap, err := svc.Runs().Snapshot(chi.URLParam(r, "run_id"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, workflow.CodeNotFound, "run not found", requestID(r))
		return
	}
	writeJSON(w, http.StatusOK, snap)
}
