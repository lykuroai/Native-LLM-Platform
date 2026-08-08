// Package orchestrator implements the Platform Inference Orchestrator (§5)。
//
// Gateway(認証・Policy 済み)から contract.Request を受け、context 構築 →
// route 選択 → 実行 → 正規化応答を返す。admission は bounded(§19.6)。
// failover 規則は gwcore と同一: タイムアウトは推論二重実行防止のため
// 切替えず、到達不可・503 のみ次候補へ(§18)。
package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lykuroai/Native-LLM-Platform/platform/contract"
	"github.com/lykuroai/Native-LLM-Platform/platform/memory"
	"github.com/lykuroai/Native-LLM-Platform/platform/modelmanager"
	"github.com/lykuroai/Native-LLM-Platform/platform/platformcfg"
	"github.com/lykuroai/Native-LLM-Platform/platform/pool"
)

// Orchestrator routes requests across deployments。
type Orchestrator struct {
	cfg     *platformcfg.Config
	manager *modelmanager.Manager
	pool    *pool.Pool
	mem     *memory.Store // nil = stateless mode

	sem   chan struct{} // admission(max_concurrent)
	queue chan struct{} // bounded queue(max_queue)

	mu      sync.Mutex
	cancels map[string]context.CancelFunc // request_id -> cancel
}

// New wires the orchestrator。mem は managed mode のときのみ非 nil。
func New(cfg *platformcfg.Config, mgr *modelmanager.Manager, p *pool.Pool, mem *memory.Store) *Orchestrator {
	return &Orchestrator{
		cfg:     cfg,
		manager: mgr,
		pool:    p,
		mem:     mem,
		sem:     make(chan struct{}, cfg.Orchestrator.MaxConcurrent),
		queue:   make(chan struct{}, cfg.Orchestrator.MaxQueue),
		cancels: map[string]context.CancelFunc{},
	}
}

// admit enforces the bounded queue (§19.6: 超過は 429 + Retry-After)。
func (o *Orchestrator) admit(ctx context.Context) (func(), *contract.Error) {
	select {
	case o.queue <- struct{}{}:
	default:
		return nil, &contract.Error{Code: contract.ErrCapacityExhausted,
			Message: "request queue is full", RetryAfter: 5}
	}
	select {
	case o.sem <- struct{}{}:
		<-o.queue
		return func() { <-o.sem }, nil
	case <-ctx.Done():
		<-o.queue
		return nil, &contract.Error{Code: contract.ErrInferenceTimeout,
			Message: "cancelled while queued"}
	}
}

func (o *Orchestrator) deadline(req *contract.Request) time.Time {
	if !req.Deadline.IsZero() {
		return req.Deadline
	}
	return time.Now().Add(time.Duration(o.cfg.Orchestrator.DefaultDeadlineSeconds) * time.Second)
}

// registerCancel tracks the request for the Cancel operation。
func (o *Orchestrator) registerCancel(reqID string, cancel context.CancelFunc) func() {
	o.mu.Lock()
	o.cancels[reqID] = cancel
	o.mu.Unlock()
	return func() {
		o.mu.Lock()
		delete(o.cancels, reqID)
		o.mu.Unlock()
	}
}

// Cancel aborts an in-flight request (§5.3)。
func (o *Orchestrator) Cancel(requestID string) {
	o.mu.Lock()
	cancel, ok := o.cancels[requestID]
	o.mu.Unlock()
	if ok {
		cancel()
	}
}

// prepared bundles per-request routing state。
type prepared struct {
	candidates []modelmanager.Candidate
	ordered    []modelmanager.Candidate
	body       map[string]json.RawMessage
	conv       *memory.Conversation
	built      *memory.BuiltContext
	userMsgs   []memory.Message
	convKey    string
}

// prepare resolves candidates and builds conversation context。
func (o *Orchestrator) prepare(req *contract.Request) (*prepared, *contract.Error) {
	if req.ContractVersion != contract.Version {
		return nil, &contract.Error{Code: contract.ErrInternalContract,
			Message: fmt.Sprintf("unsupported contract version %q", req.ContractVersion)}
	}
	cands, cerr := o.manager.Resolve(req.LogicalModel, req.Policy)
	if cerr != nil {
		return nil, cerr
	}
	// chat 以外(embeddings/rerank/responses)は connector 経路のみ
	// (Engine MVP は chat+streaming のみ、§2.1)。
	if req.Endpoint != "/v1/chat/completions" {
		filtered := cands[:0]
		for _, c := range cands {
			if c.RouteType == "connector" {
				filtered = append(filtered, c)
			}
		}
		cands = filtered
		if len(cands) == 0 {
			return nil, &contract.Error{Code: contract.ErrModelNotAvailable,
				Message: "no deployment supports this endpoint"}
		}
	}

	p := &prepared{candidates: cands}
	// NormalizedInput の浅いコピー(model 書換で呼出元の map を汚さない)
	p.body = make(map[string]json.RawMessage, len(req.NormalizedInput)+1)
	for k, v := range req.NormalizedInput {
		p.body[k] = v
	}

	// Conversation(明示 opt-in の managed mode のみ、§10.2)
	if req.Conversation != nil && req.Conversation.ID != "" {
		if o.mem == nil {
			return nil, &contract.Error{Code: contract.ErrInvalidRequest,
				Message: "conversation memory is not enabled on this platform"}
		}
		conv, err := o.mem.GetOrCreate(req.Conversation.ID, req.Policy.DataClass, req.Conversation.Version)
		if err != nil {
			return nil, memErr(err)
		}
		p.conv = conv
		p.convKey = conv.ID

		var current []json.RawMessage
		if raw, ok := p.body["messages"]; ok {
			if err := json.Unmarshal(raw, &current); err != nil {
				return nil, &contract.Error{Code: contract.ErrInvalidRequest,
					Message: "messages must be an array"}
			}
		}
		ctxWindow := 0
		if len(cands) > 0 {
			ctxWindow = cands[0].ContextLength
		}
		p.built = memory.BuildContext(conv, current, ctxWindow, 0)
		merged, err := json.Marshal(p.built.Messages)
		if err != nil {
			return nil, &contract.Error{Code: contract.ErrInternalContract, Message: "context build failed"}
		}
		p.body["messages"] = merged
		// 保存対象の user メッセージ(今回 client が送った分)
		for _, raw := range current {
			var m struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}
			if json.Unmarshal(raw, &m) == nil && m.Role != "" {
				p.userMsgs = append(p.userMsgs, memory.Message{Role: m.Role, Content: m.Content})
			}
		}
	}

	ids := make([]string, len(cands))
	for i, c := range cands {
		ids[i] = c.DeploymentID
	}
	order := o.pool.Order(ids, p.convKey)
	byID := map[string]modelmanager.Candidate{}
	for _, c := range cands {
		byID[c.DeploymentID] = c
	}
	for _, id := range order {
		p.ordered = append(p.ordered, byID[id])
	}
	return p, nil
}

func memErr(err error) *contract.Error {
	switch err {
	case memory.ErrConflict:
		return &contract.Error{Code: contract.ErrConversationConflict, Message: "conversation version conflict"}
	case memory.ErrNotFound:
		return &contract.Error{Code: contract.ErrInvalidRequest, Message: "conversation not found"}
	default:
		// Conversation DB 停止時は managed conversation を拒否し、stateless を
		// 勝手に継続しない(§18)
		return &contract.Error{Code: contract.ErrPlatformNotReady, Message: "conversation store unavailable"}
	}
}

// commitTurn persists the turn after success and schedules summary work。
func (o *Orchestrator) commitTurn(p *prepared, req *contract.Request, modelID, assistantContent, status string) int64 {
	if p.conv == nil || o.mem == nil {
		return 0
	}
	ver, err := o.mem.AppendTurn(p.conv.ID, p.conv.Version, req.RequestID, modelID, p.userMsgs, assistantContent, status)
	if err != nil {
		slog.Error("conversation append failed", "request_id", req.RequestID, "error", err)
		return 0
	}
	if p.built != nil && p.built.NeedsSummary {
		o.scheduleSummary(p.conv.ID, p.built.SummaryFromSeq, p.built.SummaryToSeq, req)
	}
	return ver
}

// Execute runs a non-streaming request (contract.Backend の中核)。
func (o *Orchestrator) Execute(ctx context.Context, req *contract.Request) (*contract.Response, *contract.Error) {
	release, aerr := o.admit(ctx)
	if aerr != nil {
		return nil, aerr
	}
	defer release()

	p, perr := o.prepare(req)
	if perr != nil {
		return nil, perr
	}
	ctx, cancel := context.WithDeadline(ctx, o.deadline(req))
	defer cancel()
	unregister := o.registerCancel(req.RequestID, cancel)
	defer unregister()

	var lastErr *contract.Error
	for i, cand := range p.ordered {
		if !o.pool.Assignable(cand.DeploymentID) && i < len(p.ordered)-1 {
			continue
		}
		started := time.Now()
		done := o.pool.Acquire(cand.DeploymentID)
		resp, cerr := o.executeOn(ctx, cand, req, p)
		if cerr == nil {
			done(time.Since(started), resp.StatusCode < 500)
			o.pool.RecordSticky(p.convKey, cand.DeploymentID)
			resp.ConversationVersion = o.commitTurn(p, req, req.LogicalModel, assistantFromBody(resp.Body), "complete")
			return resp, nil
		}
		done(time.Since(started), false)
		lastErr = cerr
		if cerr.Code == contract.ErrInferenceTimeout {
			break // 推論開始済みの可能性 — 二重実行防止のため failover しない
		}
	}
	if lastErr == nil {
		lastErr = &contract.Error{Code: contract.ErrModelNotAvailable, Message: "no candidate available"}
	}
	return nil, lastErr
}

// executeOn dispatches to one candidate。
func (o *Orchestrator) executeOn(ctx context.Context, cand modelmanager.Candidate, req *contract.Request, p *prepared) (*contract.Response, *contract.Error) {
	if cand.RouteType == "native" {
		return o.nativeExecute(ctx, cand, req, p)
	}
	body, err := marshalWithModel(p.body, cand.PhysicalModel)
	if err != nil {
		return nil, &contract.Error{Code: contract.ErrInternalContract, Message: "failed to rebuild request"}
	}
	res, cerr := cand.Connector.Do(ctx, req.Endpoint, body, "application/json", req.RequestID)
	if cerr != nil {
		return nil, cerr
	}
	if res.StatusCode == 503 {
		// Runtime 未ロード → 呼出側で次候補へ(最終候補なら透過)
		return nil, &contract.Error{Code: contract.ErrModelNotAvailable, Message: "upstream returned 503", RetryAfter: 10}
	}
	return &contract.Response{
		StatusCode:   res.StatusCode,
		ContentType:  res.ContentType,
		Body:         res.Body,
		Usage:        res.Usage,
		DeploymentID: cand.DeploymentID,
		RouteType:    cand.RouteType,
	}, nil
}

// ExecuteStream runs a streaming request with transparent SSE passthrough。
func (o *Orchestrator) ExecuteStream(ctx context.Context, req *contract.Request, w contract.StreamWriter) (*contract.StreamResult, *contract.Error) {
	release, aerr := o.admit(ctx)
	if aerr != nil {
		return nil, aerr
	}
	defer release()

	p, perr := o.prepare(req)
	if perr != nil {
		return nil, perr
	}
	ctx, cancel := context.WithDeadline(ctx, o.deadline(req))
	defer cancel()
	unregister := o.registerCancel(req.RequestID, cancel)
	defer unregister()

	// SSE 開始検知(開始後は failover 不可)と、managed conversation 時の
	// assistant 応答蓄積を担うラッパ(本文は顧客環境内に留まる)
	acc := newAccumulatingWriter(w, p.conv != nil)
	sink := contract.StreamWriter(acc)

	var lastErr *contract.Error
	for i, cand := range p.ordered {
		if !o.pool.Assignable(cand.DeploymentID) && i < len(p.ordered)-1 {
			continue
		}
		started := time.Now()
		done := o.pool.Acquire(cand.DeploymentID)
		result, cerr := o.streamOn(ctx, cand, req, p, sink)
		if cerr == nil {
			done(time.Since(started), true)
			o.pool.RecordSticky(p.convKey, cand.DeploymentID)
			status := "complete"
			if result.ClientGone {
				status = "interrupted"
			}
			result.ConversationVersion = o.commitTurn(p, req, req.LogicalModel, acc.content(), status)
			result.DeploymentID = cand.DeploymentID
			result.RouteType = cand.RouteType
			return result, nil
		}
		done(time.Since(started), false)
		lastErr = cerr
		if acc.Started() {
			// SSE 開始後は候補切替も HTTP status 変更もできない(§13.1.1)
			break
		}
		if cerr.Code == contract.ErrInferenceTimeout {
			break
		}
	}
	if lastErr == nil {
		lastErr = &contract.Error{Code: contract.ErrModelNotAvailable, Message: "no candidate available"}
	}
	return nil, lastErr
}

func (o *Orchestrator) streamOn(ctx context.Context, cand modelmanager.Candidate, req *contract.Request, p *prepared, w contract.StreamWriter) (*contract.StreamResult, *contract.Error) {
	if cand.RouteType == "native" {
		return o.nativeStream(ctx, cand, req, p, w)
	}
	body, err := marshalWithModel(p.body, cand.PhysicalModel)
	if err != nil {
		return nil, &contract.Error{Code: contract.ErrInternalContract, Message: "failed to rebuild request"}
	}
	outcome, cerr := cand.Connector.DoStream(ctx, req.Endpoint, body, "text/event-stream", req.RequestID, w)
	if cerr != nil {
		return nil, cerr
	}
	return &contract.StreamResult{Usage: outcome.Usage, ClientGone: outcome.ClientGone}, nil
}

// CountTokens estimates input tokens for the routed model (§5.3)。
func (o *Orchestrator) CountTokens(ctx context.Context, req *contract.Request) (int64, *contract.Error) {
	cands, cerr := o.manager.Resolve(req.LogicalModel, req.Policy)
	if cerr != nil {
		return 0, cerr
	}
	for _, c := range cands {
		if c.RouteType == "native" && c.Engine != nil {
			input, ierr := engineInputFromBody(req.NormalizedInput)
			if ierr != nil {
				return 0, ierr
			}
			n, eerr := c.Engine.CountTokens(ctx, c.ModelInstanceID, input)
			if eerr == nil {
				return n, nil
			}
		}
	}
	// connector 経路は model tokenizer に非依存の概算(§10.5 と同じ方針)
	var total int64
	for _, v := range req.NormalizedInput {
		total += int64(len(v)) / 4
	}
	return total, nil
}

// marshalWithModel rebuilds the body with the physical model name
// (gwcore と同じく model 以外の body 改変はしない)。
func marshalWithModel(body map[string]json.RawMessage, physicalModel string) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(body))
	for k, v := range body {
		out[k] = v
	}
	raw, err := json.Marshal(physicalModel)
	if err != nil {
		return nil, err
	}
	out["model"] = raw
	return json.Marshal(out)
}

// assistantFromBody extracts choices[0].message.content(managed conversation
// の保存用。透過応答自体は改変しない)。
func assistantFromBody(body []byte) string {
	var probe struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &probe) != nil || len(probe.Choices) == 0 {
		return ""
	}
	return probe.Choices[0].Message.Content
}
