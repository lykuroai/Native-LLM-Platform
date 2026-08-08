package enginecontract

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// MockEngine is an in-memory Data/Control API implementation for contract
// tests and pre-release integration(実 Engine は実装中。本 mock は仕様書
// §9/§20/§21 の contract 挙動のみを模し、推論品質は模さない)。
type MockEngine struct {
	mu       sync.Mutex
	state    string // §20.1: starting..stopped
	draining bool
	loaded   map[string]*ModelStatus // model_instance_id -> status
	// ApprovedDigests simulates artifact signature/digest verification(§10.3)。
	// 空でない場合、LoadModel は登録済み digest 以外を拒否する。
	ApprovedDigests map[string]bool
	Capacity        CapacityInfo
}

// NewMockEngine returns a ready mock engine with no loaded models.
func NewMockEngine() *MockEngine {
	return &MockEngine{
		state:  "idle",
		loaded: map[string]*ModelStatus{},
		Capacity: CapacityInfo{
			TotalVRAMMB: 24576, FreeVRAMMB: 24576,
			MaxSequences: 16, MaxQueue: 64, Health: "healthy",
		},
	}
}

var _ DataAPI = (*MockEngine)(nil)
var _ ControlAPI = (*MockEngine)(nil)

func (m *MockEngine) modelReady(id string) *EngineError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.draining {
		return &EngineError{Code: CodeEngineDraining, Message: "engine is draining", Retryable: true}
	}
	st, ok := m.loaded[id]
	if !ok || st.State != "ready" {
		return &EngineError{Code: CodeModelNotLoaded, Message: "model instance is not loaded", Retryable: true}
	}
	return nil
}

// Generate echoes a deterministic completion (contract 適合検証用)。
func (m *MockEngine) Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, *EngineError) {
	if req.RequestID == "" || req.ModelInstanceID == "" {
		return nil, &EngineError{Code: CodeInvalidRequest, Message: "request_id and model_instance_id are required"}
	}
	if req.Input.Prompt != "" && len(req.Input.Messages) > 0 {
		return nil, &EngineError{Code: CodeInvalidRequest, Message: "prompt and messages are exclusive"}
	}
	if err := m.modelReady(req.ModelInstanceID); err != nil {
		return nil, err
	}
	if dl := req.Scheduling.DeadlineUnixMS; dl > 0 && time.Now().UnixMilli() > dl {
		return nil, &EngineError{Code: CodeDeadlineExceeded, Message: "deadline exceeded", Retryable: false}
	}
	var lastUser string
	for _, msg := range req.Input.Messages {
		if msg.Role == "user" {
			lastUser = msg.Content
		}
	}
	if lastUser == "" {
		lastUser = req.Input.Prompt
	}
	out := "mock:" + req.ModelInstanceID + ":" + lastUser
	inTok := int64(len(req.Input.Prompt))
	for _, msg := range req.Input.Messages {
		inTok += int64(len(msg.Content)) / 4
	}
	outTok := int64(len(out)) / 4
	return &GenerateResponse{
		RequestID:       req.RequestID,
		ModelInstanceID: req.ModelInstanceID,
		OutputText:      out,
		FinishReason:    "stop",
		Usage:           UsageInfo{InputTokens: inTok, OutputTokens: outTok, TotalTokens: inTok + outTok},
		Timing:          TimingInfo{TotalMS: 1},
	}, nil
}

// GenerateStream emits started → delta(s) → usage → completed (§9.7 順序)。
func (m *MockEngine) GenerateStream(ctx context.Context, req *GenerateRequest, emit func(StreamEvent) bool) *EngineError {
	resp, err := m.Generate(ctx, req)
	if err != nil {
		return err
	}
	seq := int64(0)
	next := func(e StreamEvent) bool {
		seq++
		e.RequestID = req.RequestID
		e.Sequence = seq
		e.TimestampMS = time.Now().UnixMilli()
		return emit(e)
	}
	if !next(StreamEvent{Type: EventStarted}) {
		return &EngineError{Code: CodeRequestCancelled, Message: "consumer gone"}
	}
	for _, part := range strings.SplitAfter(resp.OutputText, ":") {
		if part == "" {
			continue
		}
		if ctx.Err() != nil {
			return &EngineError{Code: CodeRequestCancelled, Message: "cancelled"}
		}
		if !next(StreamEvent{Type: EventTextDelta, Delta: part}) {
			return &EngineError{Code: CodeRequestCancelled, Message: "consumer gone"}
		}
	}
	u := resp.Usage
	if !next(StreamEvent{Type: EventUsage, Usage: &u}) {
		return &EngineError{Code: CodeRequestCancelled, Message: "consumer gone"}
	}
	next(StreamEvent{Type: EventCompleted, Final: resp})
	return nil
}

func (m *MockEngine) Cancel(ctx context.Context, requestID string) *EngineError { return nil }

func (m *MockEngine) CountTokens(ctx context.Context, modelInstanceID string, input Input) (int64, *EngineError) {
	if err := m.modelReady(modelInstanceID); err != nil {
		return 0, err
	}
	n := int64(len(input.Prompt)) / 4
	for _, msg := range input.Messages {
		n += int64(len(msg.Content)) / 4
	}
	return n, nil
}

func (m *MockEngine) GetCapabilities(ctx context.Context) (*EngineCapabilities, *EngineError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	models := make([]string, 0, len(m.loaded))
	for id, st := range m.loaded {
		if st.State == "ready" {
			models = append(models, id)
		}
	}
	return &EngineCapabilities{
		EngineVersion:     "mock-0.0.0",
		DataAPIVersion:    DataAPIVersion,
		ControlAPIVersion: ControlAPIVersion,
		LoadedModels:      models,
		Streaming:         true,
		CountTokens:       true,
	}, nil
}

// LoadModel simulates §8.7 の検証つき load。
func (m *MockEngine) LoadModel(ctx context.Context, req *LoadModelRequest) *EngineError {
	if req.ModelInstanceID == "" || req.ArtifactDigest == "" {
		return &EngineError{Code: CodeInvalidRequest, Message: "model_instance_id and artifact_digest are required"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ApprovedDigests != nil && !m.ApprovedDigests[req.ArtifactDigest] {
		m.loaded[req.ModelInstanceID] = &ModelStatus{
			ModelInstanceID: req.ModelInstanceID, State: "failed",
			ErrorCode: CodeArtifactVerificationFailed,
		}
		return &EngineError{Code: CodeArtifactVerificationFailed, Message: "artifact digest is not approved"}
	}
	m.loaded[req.ModelInstanceID] = &ModelStatus{
		ModelInstanceID: req.ModelInstanceID, State: "ready", ArtifactID: req.ArtifactDigest,
	}
	m.state = "ready"
	return nil
}

func (m *MockEngine) UnloadModel(ctx context.Context, id string) *EngineError {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.loaded[id]; !ok {
		return &EngineError{Code: CodeModelNotLoaded, Message: "not loaded"}
	}
	delete(m.loaded, id)
	return nil
}

func (m *MockEngine) Drain(ctx context.Context) *EngineError {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.draining = true
	m.state = "draining"
	return nil
}

func (m *MockEngine) Resume(ctx context.Context) *EngineError {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.draining = false
	m.state = "ready"
	return nil
}

func (m *MockEngine) GetModelStatus(ctx context.Context, id string) (*ModelStatus, *EngineError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.loaded[id]
	if !ok {
		return &ModelStatus{ModelInstanceID: id, State: "unloaded"}, nil
	}
	cp := *st
	return &cp, nil
}

func (m *MockEngine) GetCapacity(ctx context.Context) (*CapacityInfo, *EngineError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.Capacity
	c.ActiveSequences = 0
	return &c, nil
}

func (m *MockEngine) GetManifest(ctx context.Context, id string) (*ManifestInfo, *EngineError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.loaded[id]
	if !ok {
		return nil, &EngineError{Code: CodeModelNotLoaded, Message: "not loaded"}
	}
	return &ManifestInfo{
		SchemaVersion: "1",
		ArtifactID:    st.ArtifactID,
		ModelFamily:   "mock",
		Architecture:  fmt.Sprintf("mock-%s", id),
		WeightFormat:  "safetensors",
	}, nil
}
