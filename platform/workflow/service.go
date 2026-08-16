// Workflow Service — W1 Sequential Runner と Flow 管理の中核
// (MRCI-001 §6.2/§8、MRCI-002 §4)。
//
// Step 実行は Executor(既存 orchestrator)へ委譲する。本 package は
// Runtime protocol・候補選択・failover を実装しない(§2.3 重複実装禁止)。
package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/lykuroai/Native-LLM-Platform/platform/contract"
	"github.com/lykuroai/Native-LLM-Platform/platform/platformcfg"
)

// Workflow error codes(MRCI-002 §5.3。gwcore が HTTP へ写像する)。
const (
	CodeNotFound        = "workflow_not_found"
	CodeNotPublished    = "workflow_not_published"
	CodeInputValidation = "workflow_input_validation_error"
	CodeTemplateRender  = "template_render_error"
	CodeNoEligible      = "no_eligible_runtime"
	CodeBudgetExceeded  = "budget_exceeded"
	CodeIdemConflict    = "idempotency_conflict"
	CodeRevConflict     = "workflow_version_conflict"
	CodeCapacity        = "workflow_capacity_exhausted"
	CodeStepTimeout     = "step_timeout"
	CodeInvalidResponse = "runtime_invalid_response"
	CodeCancelled       = "cancelled"
	CodeInternal        = "workflow_internal_error"
)

// RunError is the gateway-facing workflow error。
type RunError struct {
	Code       string
	Message    string
	RetryAfter int
}

func (e *RunError) Error() string { return e.Code + ": " + e.Message }

// Executor is the slice of contract.Backend the runner needs。
type Executor interface {
	Execute(ctx context.Context, req *contract.Request) (*contract.Response, *contract.Error)
	Cancel(ctx context.Context, requestID string)
}

// Service coordinates flow storage and run execution。
type Service struct {
	wcfg     platformcfg.Workflows
	flows    *FlowStore
	runs     *RunStore
	exec     Executor
	resolver Resolver
	sem      chan struct{}
	stop     context.CancelFunc
}

// Open loads stores and starts the retention sweeper。
func Open(cfg *platformcfg.Config, resolver Resolver, exec Executor) (*Service, error) {
	fs, err := OpenFlowStore(cfg.Workflows.DataDir, cfg.Workflows.MaxFlows)
	if err != nil {
		return nil, err
	}
	rs, err := OpenRunStore(cfg.Workflows.DataDir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{wcfg: cfg.Workflows, flows: fs, runs: rs, exec: exec, resolver: resolver,
		sem: make(chan struct{}, cfg.Workflows.MaxConcurrentRuns), stop: cancel}
	go s.sweepLoop(ctx)
	return s, nil
}

// Close stops background work(実行中 Run は run context で個別管理)。
func (s *Service) Close() { s.stop() }

func (s *Service) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runs.Sweep(s.wcfg.RunRetentionDays, s.wcfg.EventRetentionDays)
		}
	}
}

// Flows exposes the flow store(admin 経路用)。
func (s *Service) Flows() *FlowStore { return s.flows }

// Runs exposes the run store(照会・SSE 用)。
func (s *Service) Runs() *RunStore { return s.runs }

// AliasEnabled reports whether flow:{alias} chat routing is on。
func (s *Service) AliasEnabled() bool { return s.wcfg.AliasEnabled() }

// SingleInputName resolves the one input a chat message can map to
// (MRCI-001 §11.10: 複数 Input で写像不能な Flow は Native API のみ)。
func (s *Service) SingleInputName(alias string) (string, *RunError) {
	flowID, latest, err := s.flows.ResolveAlias(alias)
	if err != nil {
		return "", &RunError{Code: CodeNotFound, Message: "no published flow for alias " + alias}
	}
	fv, err := s.flows.GetVersion(flowID, latest)
	if err != nil {
		return "", &RunError{Code: CodeNotPublished, Message: "flow version unavailable"}
	}
	def, perr := ParseDefinition(fv.Definition)
	if perr != nil || def.InputsSchema == nil {
		return "", &RunError{Code: CodeInternal, Message: "published definition is unreadable"}
	}
	if len(def.InputsSchema.Required) == 1 {
		return def.InputsSchema.Required[0], nil
	}
	if len(def.InputsSchema.Required) == 0 && len(def.InputsSchema.Properties) == 1 {
		for name := range def.InputsSchema.Properties {
			return name, nil
		}
	}
	return "", &RunError{Code: CodeInputValidation,
		Message: "this flow requires multiple inputs; use POST /v1/workflows/" + alias + "/runs"}
}

// ValidateDefinition parses + validates a raw definition against the catalog。
func (s *Service) ValidateDefinition(raw json.RawMessage) (*Definition, []error) {
	def, err := ParseDefinition(raw)
	if err != nil {
		return nil, []error{err}
	}
	if errs := def.Validate(s.resolver); len(errs) > 0 {
		return nil, errs
	}
	return def, nil
}

// CreateFlow registers a new draft(構文のみ必須 — 完全な Validation は
// publish 時に強制。alias は作成時から一意)。
func (s *Service) CreateFlow(raw json.RawMessage) (*FlowMeta, []error) {
	def, err := ParseDefinition(raw)
	if err != nil {
		return nil, []error{err}
	}
	if !aliasPattern.MatchString(def.Alias) {
		return nil, []error{fmt.Errorf("alias %q is invalid", def.Alias)}
	}
	meta, cerr := s.flows.Create(raw, def.Alias, def.Name)
	if cerr != nil {
		return nil, []error{cerr}
	}
	return meta, nil
}

// UpdateFlow replaces the draft(revision は If-Match 相当)。
func (s *Service) UpdateFlow(flowID string, expectRevision int, raw json.RawMessage) (*Draft, []error) {
	def, err := ParseDefinition(raw)
	if err != nil {
		return nil, []error{err}
	}
	meta, gerr := s.flows.Get(flowID)
	if gerr != nil {
		return nil, []error{gerr}
	}
	if def.Alias != meta.Alias {
		// alias は Tenant 内一意の公開識別子 — Draft 更新での変更は不可
		return nil, []error{fmt.Errorf("alias cannot be changed after creation")}
	}
	d, uerr := s.flows.UpdateDraft(flowID, expectRevision, raw)
	if uerr != nil {
		return nil, []error{uerr}
	}
	return d, nil
}

// PublishFlow validates the draft fully and freezes it as the next version。
func (s *Service) PublishFlow(flowID string, expectRevision int) (*FlowVersion, []error) {
	d, err := s.flows.GetDraft(flowID)
	if err != nil {
		return nil, []error{err}
	}
	if d.Revision != expectRevision {
		return nil, []error{ErrRevisionConflict}
	}
	if _, errs := s.ValidateDefinition(d.Definition); len(errs) > 0 {
		return nil, errs
	}
	fv, perr := s.flows.Publish(flowID, expectRevision)
	if perr != nil {
		return nil, []error{perr}
	}
	return fv, nil
}

// StartParams describes a run request。
type StartParams struct {
	Alias        string
	FlowVersion  int // 0 = 最新公開 Version
	Inputs       map[string]string
	ResponseMode string // sync | stream
	KeyID        string
	RequestID    string
	DataClass    string
	IdemKey      string
}

// StartRun fixes the flow version, creates the run and starts execution in a
// goroutine。呼出側は Run.Done() / Subscribe で完了・進行を待つ。
func (s *Service) StartRun(p StartParams) (*Run, *RunError) {
	flowID, latest, err := s.flows.ResolveAlias(p.Alias)
	if err != nil {
		return nil, &RunError{Code: CodeNotFound, Message: "no published flow for alias " + p.Alias}
	}
	version := p.FlowVersion
	if version == 0 {
		version = latest
	}
	fv, err := s.flows.GetVersion(flowID, version)
	if err != nil {
		return nil, &RunError{Code: CodeNotPublished, Message: fmt.Sprintf("version %d is not published", version)}
	}
	def, perr := ParseDefinition(fv.Definition)
	if perr != nil {
		return nil, &RunError{Code: CodeInternal, Message: "published definition is unreadable"}
	}
	if err := def.ValidateInputs(p.Inputs); err != nil {
		return nil, &RunError{Code: CodeInputValidation, Message: err.Error()}
	}

	// Idempotency (§15.1): 同一 Key + 同一 payload → 既存 Run
	var idemHash, payloadSum string
	if p.IdemKey != "" {
		idemHash = hashParts(p.KeyID, p.IdemKey)
		payloadSum = hashParts(p.Alias, fmt.Sprint(version), canonicalInputs(p.Inputs), p.ResponseMode)
		if runID, conflict := s.runs.IdemCheck(idemHash, payloadSum); conflict {
			return nil, &RunError{Code: CodeIdemConflict, Message: "idempotency key was used with a different payload"}
		} else if runID != "" {
			r, gerr := s.runs.Get(runID)
			if gerr == nil {
				return r, nil
			}
		}
	}

	select {
	case s.sem <- struct{}{}:
	default:
		return nil, &RunError{Code: CodeCapacity, Message: "workflow run queue is full", RetryAfter: 10}
	}

	run := &Run{
		FlowID: flowID, Alias: p.Alias, FlowVersion: version, FlowChecksum: fv.Checksum,
		ResponseMode: p.ResponseMode, DataClass: p.DataClass, KeyID: p.KeyID,
		RequestID: p.RequestID,
		Steps:     make([]StepState, len(def.Steps)),
	}
	for i, st := range def.Steps {
		run.Steps[i] = StepState{StepID: st.StepID, Status: StepPending}
	}
	if err := s.runs.Create(run); err != nil {
		<-s.sem
		return nil, &RunError{Code: CodeInternal, Message: "failed to persist run"}
	}
	if idemHash != "" {
		s.runs.IdemRecord(idemHash, payloadSum, run.ID)
	}
	s.runs.Emit(run, "workflow.queued", map[string]any{"run_id": run.ID, "flow_version": version})

	go s.execute(run, def, p.Inputs)
	return run, nil
}

// CancelRun requests cancellation(冪等)。
func (s *Service) CancelRun(runID string) error {
	r, err := s.runs.Get(runID)
	if err != nil {
		return err
	}
	var reqID string
	var cancel func()
	already := false
	s.runs.Update(r, func(r *Run) {
		if Terminal(r.Status) {
			already = true
			return
		}
		r.Status = RunCancelling
		reqID = r.curReqID
		cancel = r.cancelFn
	})
	if already {
		return nil // 冪等: 終了済み Run への Cancel は no-op
	}
	if cancel != nil {
		cancel()
	}
	if reqID != "" {
		s.exec.Cancel(context.Background(), reqID)
	}
	s.runs.Emit(r, "workflow.cancelling", map[string]any{"run_id": r.ID})
	return nil
}

func hashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func canonicalInputs(in map[string]string) string {
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	for _, k := range keys {
		b = append(b, k...)
		b = append(b, 0)
		b = append(b, in[k]...)
		b = append(b, 0)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- runner ---

// retryable reports whether a contract error may be retried on another
// attempt。タイムアウトは二重推論防止のため再試行しない(既存原則)。
func retryable(code string) bool {
	switch code {
	case contract.ErrCapacityExhausted, contract.ErrModelNotAvailable, contract.ErrPlatformNotReady:
		return true
	}
	return false
}

type target struct {
	poolID   string
	fallback bool
}

func (s *Service) execute(run *Run, def *Definition, inputs map[string]string) {
	defer func() { <-s.sem }()

	timeoutMS := def.Limits.TimeoutMS
	if timeoutMS <= 0 {
		timeoutMS = DefaultTimeoutMS
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()

	now := time.Now().UTC()
	s.runs.Update(run, func(r *Run) {
		r.Status = RunRunning
		r.StartedAt = &now
		r.cancelFn = cancel
	})
	s.runs.Emit(run, "workflow.started", map[string]any{"run_id": run.ID})

	vars := map[string]string{"run.id": run.ID}
	if def.InputsSchema != nil {
		// 宣言済みで未指定の任意 input は空文字として展開する
		for name := range def.InputsSchema.Properties {
			vars["inputs."+name] = ""
		}
	}
	for k, v := range inputs {
		vars["inputs."+k] = v
	}

	var totalIn, totalOut int64
	for i := range def.Steps {
		step := &def.Steps[i]

		if def.Limits.MaxTotalTokens > 0 && totalIn+totalOut >= def.Limits.MaxTotalTokens {
			s.failRun(run, i, CodeBudgetExceeded, "max_total_tokens budget exhausted")
			return
		}
		if err := ctx.Err(); err != nil {
			s.finishInterrupted(run, i, err)
			return
		}

		text, rerr := RenderTemplate(step.InputMapping["text"], vars)
		if rerr != nil {
			s.failRun(run, i, CodeTemplateRender, rerr.Error())
			return
		}
		if len(text) > MaxTemplateOutBytes {
			s.failRun(run, i, CodeTemplateRender, "rendered input exceeds size limit")
			return
		}

		stepStart := time.Now().UTC()
		s.runs.Update(run, func(r *Run) {
			r.Steps[i].Status = StepRunning
			r.Steps[i].StartedAt = &stepStart
		})
		s.runs.Emit(run, "step.started", map[string]any{"step_id": step.StepID})

		output, code, msg := s.runStep(ctx, run, def, i, step, text)
		if code != "" {
			if ctx.Err() != nil {
				s.finishInterrupted(run, i, ctx.Err())
				return
			}
			s.runs.Update(run, func(r *Run) {
				st := &r.Steps[i]
				st.Status = StepFailed
				if code == CodeStepTimeout {
					st.Status = StepTimedOut
				}
				st.ErrorCode = code
				done := time.Now().UTC()
				st.CompletedAt = &done
			})
			s.runs.Emit(run, "step.failed", map[string]any{"step_id": step.StepID, "error_code": code})
			s.failRun(run, i+1, code, msg)
			return
		}

		vars["steps."+step.StepID+".output"] = output
		var stIn, stOut int64
		s.runs.Update(run, func(r *Run) {
			st := &r.Steps[i]
			st.Status = StepCompleted
			done := time.Now().UTC()
			st.CompletedAt = &done
			stIn, stOut = st.InputTokens, st.OutputTokens
		})
		totalIn += stIn
		totalOut += stOut
		s.runs.Emit(run, "step.completed", map[string]any{
			"step_id": step.StepID, "attempts": run.Steps[i].Attempts,
			"input_tokens": stIn, "output_tokens": stOut,
			"duration_ms": time.Since(stepStart).Milliseconds()})
	}

	final, rerr := RenderTemplate(def.OutputMapping["text"], vars)
	if rerr != nil {
		s.failRun(run, len(def.Steps), CodeTemplateRender, rerr.Error())
		return
	}
	done := time.Now().UTC()
	s.runs.Update(run, func(r *Run) {
		r.Status = RunCompleted
		r.CompletedAt = &done
		r.InputTokens = totalIn
		r.OutputTokens = totalOut
		r.output = final
	})
	s.runs.Emit(run, "workflow.completed", map[string]any{
		"run_id": run.ID, "input_tokens": totalIn, "output_tokens": totalOut,
		"duration_ms": done.Sub(run.CreatedAt).Milliseconds()})
	// 最終 Output は Event に載せない(§10.5 本文非含有)— 応答は
	// Run Store のメモリ(Output())経由でのみ返す。
	close(run.done)
}

// runStep executes one step across primary + approved fallback pools。
// 戻り値: (output, errorCode, message)。errorCode=="" が成功。
func (s *Service) runStep(ctx context.Context, run *Run, def *Definition, idx int, step *Step, text string) (string, string, string) {
	maxAttempts := DefaultMaxAttempts
	backoff := 500 * time.Millisecond
	if step.RetryPolicy != nil {
		maxAttempts = step.RetryPolicy.MaxAttempts
		if step.RetryPolicy.BackoffMS > 0 {
			backoff = time.Duration(step.RetryPolicy.BackoffMS) * time.Millisecond
		}
	}
	targets := []target{{poolID: step.RuntimeTarget.PoolID}}
	if step.FallbackPolicy != nil {
		for _, pid := range step.FallbackPolicy.AllowedPoolIDs {
			targets = append(targets, target{poolID: pid, fallback: true})
		}
	}

	attempt := 0
	var lastCode, lastMsg string
	for _, tg := range targets {
		if tg.fallback {
			s.runs.Emit(run, "fallback.selected", map[string]any{
				"step_id": step.StepID, "pool_id": tg.poolID})
		}
		for a := 0; a < maxAttempts; a++ {
			if err := ctx.Err(); err != nil {
				return "", CodeCancelled, "run aborted"
			}
			attempt++
			if attempt > 1 {
				s.runs.Update(run, func(r *Run) { r.Steps[idx].Status = StepRetrying })
				s.runs.Emit(run, "step.retrying", map[string]any{"step_id": step.StepID, "attempt": attempt})
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return "", CodeCancelled, "run aborted"
				}
			}
			out, usage, dep, routeType, cerr := s.attempt(ctx, run, step, text, attempt, tg)
			dec := RouteDecision{Attempt: attempt, LogicalModel: step.RuntimeTarget.LogicalModel,
				PoolID: tg.poolID, DeploymentID: dep, RouteType: routeType, FallbackUsed: tg.fallback}
			if cerr != nil {
				dec.ErrorCode = cerr.Code
			}
			s.runs.Update(run, func(r *Run) {
				st := &r.Steps[idx]
				st.Attempts = attempt
				st.Decisions = append(st.Decisions, dec)
				if cerr == nil {
					if usage.InputTokens != nil {
						st.InputTokens += *usage.InputTokens
					}
					if usage.OutputTokens != nil {
						st.OutputTokens += *usage.OutputTokens
					}
				}
			})
			if cerr == nil {
				return out, "", ""
			}
			switch {
			case cerr.Code == contract.ErrInferenceTimeout:
				// 推論開始済みの可能性 — 別 attempt・別 pool へ切替えない
				return "", CodeStepTimeout, "step timed out"
			case retryable(cerr.Code):
				lastCode, lastMsg = CodeNoEligible, cerr.Message
				continue
			default:
				code := CodeInvalidResponse
				if cerr.Code == contract.ErrInvalidRequest {
					code = CodeInputValidation
				}
				return "", code, cerr.Message
			}
		}
	}
	if lastCode == "" {
		lastCode, lastMsg = CodeNoEligible, "no eligible runtime"
	}
	return "", lastCode, lastMsg
}

// attempt issues one contract request(1 Step Attempt = 1 既存実行記録)。
func (s *Service) attempt(ctx context.Context, run *Run, step *Step, text string, attempt int, tg target) (string, contract.Usage, string, string, *contract.Error) {
	reqID := fmt.Sprintf("%s-%s-a%d", run.ID, step.StepID, attempt)
	s.runs.Update(run, func(r *Run) { r.curReqID = reqID })

	msgs := []map[string]string{}
	if step.SystemPrompt != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": step.SystemPrompt})
	}
	msgs = append(msgs, map[string]string{"role": "user", "content": text})
	body := map[string]json.RawMessage{}
	rawMsgs, err := json.Marshal(msgs)
	if err != nil {
		return "", contract.Usage{}, "", "", &contract.Error{Code: contract.ErrInternalContract, Message: "failed to build messages"}
	}
	body["messages"] = rawMsgs
	if g := step.Generation; g != nil {
		if g.Temperature != nil {
			body["temperature"], _ = json.Marshal(*g.Temperature)
		}
		if g.TopP != nil {
			body["top_p"], _ = json.Marshal(*g.TopP)
		}
		if g.MaxOutputTokens > 0 {
			body["max_tokens"], _ = json.Marshal(g.MaxOutputTokens)
		}
	}

	stepCtx := ctx
	if step.TimeoutMS > 0 {
		var cancel context.CancelFunc
		stepCtx, cancel = context.WithTimeout(ctx, time.Duration(step.TimeoutMS)*time.Millisecond)
		defer cancel()
	}
	req := &contract.Request{
		ContractVersion: contract.Version,
		RequestID:       reqID,
		TraceID:         run.RequestID,
		ActorScope:      "workflow:" + run.Alias,
		Policy:          contract.PolicyContext{DataClass: run.DataClass},
		LogicalModel:    step.RuntimeTarget.LogicalModel,
		PoolID:          tg.poolID,
		Endpoint:        "/v1/chat/completions",
		NormalizedInput: body,
	}
	if step.TimeoutMS > 0 {
		req.Deadline = time.Now().Add(time.Duration(step.TimeoutMS) * time.Millisecond)
	}
	resp, cerr := s.exec.Execute(stepCtx, req)
	if cerr != nil {
		return "", contract.Usage{}, "", "", cerr
	}
	if resp.StatusCode >= 400 {
		return "", resp.Usage, resp.DeploymentID, resp.RouteType,
			&contract.Error{Code: contract.ErrInternalContract, Message: fmt.Sprintf("upstream returned status %d", resp.StatusCode)}
	}
	var probe struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(resp.Body, &probe) != nil || len(probe.Choices) == 0 {
		return "", resp.Usage, resp.DeploymentID, resp.RouteType,
			&contract.Error{Code: contract.ErrInternalContract, Message: "upstream response is not a chat completion"}
	}
	return probe.Choices[0].Message.Content, resp.Usage, resp.DeploymentID, resp.RouteType, nil
}

// failRun finalizes a failed run(nextStep 以降を SKIPPED へ)。
func (s *Service) failRun(run *Run, nextStep int, code, msg string) {
	done := time.Now().UTC()
	s.runs.Update(run, func(r *Run) {
		r.Status = RunFailed
		r.ErrorCode = code
		r.CompletedAt = &done
		for j := nextStep; j < len(r.Steps); j++ {
			if r.Steps[j].Status == StepPending {
				r.Steps[j].Status = StepSkipped
			}
		}
		var in, out int64
		for _, st := range r.Steps {
			in += st.InputTokens
			out += st.OutputTokens
		}
		r.InputTokens, r.OutputTokens = in, out
	})
	s.runs.Emit(run, "workflow.failed", map[string]any{"run_id": run.ID, "error_code": code, "message": msg})
	close(run.done)
}

// finishInterrupted maps context cancellation to CANCELLED / TIMED_OUT。
func (s *Service) finishInterrupted(run *Run, stepIdx int, cause error) {
	status := RunTimedOut
	code := CodeStepTimeout
	evt := "workflow.failed"
	if run.Status == RunCancelling || cause == context.Canceled {
		status = RunCancelled
		code = CodeCancelled
		evt = "workflow.cancelled"
	}
	done := time.Now().UTC()
	s.runs.Update(run, func(r *Run) {
		r.Status = status
		r.ErrorCode = code
		r.CompletedAt = &done
		for j := stepIdx; j < len(r.Steps); j++ {
			switch r.Steps[j].Status {
			case StepPending:
				r.Steps[j].Status = StepSkipped
			case StepRunning, StepRetrying:
				r.Steps[j].Status = StepCancelled
			}
		}
	})
	s.runs.Emit(run, evt, map[string]any{"run_id": run.ID, "error_code": code})
	close(run.done)
}
