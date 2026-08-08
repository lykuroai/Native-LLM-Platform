// Package enginecontract freezes the Platform↔Engine boundary
// (platform-engine-v1、LYK-NIE-SD-001 v1.0 §9 の RPC 名・フィールド名どおり)。
//
// Native Engine は実装中のため、本 contract は仕様書を正本として凍結し、
// 実 Engine release 後に As-Built 差分を versioned adapter で吸収する
// (Phase 0 報告 §2)。transport(mTLS gRPC 推奨)の binding は Engine 側
// release と同時に決定する。
//
// 呼出主体の分離(§8.2 / AT-G03): Control API クライアントは Model Manager
// のみが保持し、Orchestrator には Data API のみを注入する。Gateway service
// はどちらも保持しない。
package enginecontract

import "context"

// DataAPIVersion / ControlAPIVersion are the frozen contract versions.
const (
	DataAPIVersion    = "platform-engine-data-v1"
	ControlAPIVersion = "platform-engine-control-v1"
)

// Message is one normalized chat message (§9.4 input.messages)。
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// GenerateRequest is the Data API inference request (§9.4)。
// prompt と messages は排他(§9.5)。
type GenerateRequest struct {
	RequestID       string     `json:"request_id"`
	TraceID         string     `json:"trace_id,omitempty"`
	TenantScope     string     `json:"tenant_scope,omitempty"`  // hash 値(§9.4)
	ProjectScope    string     `json:"project_scope,omitempty"` // hash 値
	ModelInstanceID string     `json:"model_instance_id"`
	Input           Input      `json:"input"`
	Generation      Generation `json:"generation"`
	Scheduling      Scheduling `json:"scheduling"`
	Cache           Cache      `json:"cache"`
}

type Input struct {
	Messages []Message `json:"messages,omitempty"`
	Prompt   string    `json:"prompt,omitempty"`
}

type Generation struct {
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	TopK            *int     `json:"top_k,omitempty"`
	Seed            *int64   `json:"seed,omitempty"`
	Stop            []string `json:"stop,omitempty"`
}

type Scheduling struct {
	Priority       int   `json:"priority,omitempty"`
	DeadlineUnixMS int64 `json:"deadline_unix_ms,omitempty"`
}

type Cache struct {
	ReuseHandle      string `json:"reuse_handle,omitempty"`
	AllowPrefixCache bool   `json:"allow_prefix_cache,omitempty"`
}

// UsageInfo は §9.6 usage。
type UsageInfo struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// TimingInfo は §9.6 timing。
type TimingInfo struct {
	QueueMS    int64 `json:"queue_ms"`
	TokenizeMS int64 `json:"tokenize_ms"`
	PrefillMS  int64 `json:"prefill_ms"`
	DecodeMS   int64 `json:"decode_ms"`
	TotalMS    int64 `json:"total_ms"`
}

// GenerateResponse is the Data API result (§9.6)。
type GenerateResponse struct {
	RequestID       string     `json:"request_id"`
	ModelInstanceID string     `json:"model_instance_id"`
	OutputText      string     `json:"output_text"`
	FinishReason    string     `json:"finish_reason"`
	Usage           UsageInfo  `json:"usage"`
	Timing          TimingInfo `json:"timing"`
	Cache           Cache      `json:"cache"`
}

// Streaming event types (§9.7)。
const (
	EventStarted   = "response.started"
	EventTextDelta = "response.output_text.delta"
	EventUsage     = "response.usage"
	EventCompleted = "response.completed"
	EventError     = "response.error"
)

// StreamEvent is one ordered streaming event (§9.7: request_id・monotonic
// sequence・timestamp を持つ)。
type StreamEvent struct {
	Type        string            `json:"type"`
	RequestID   string            `json:"request_id"`
	Sequence    int64             `json:"sequence"`
	TimestampMS int64             `json:"timestamp_ms"`
	Delta       string            `json:"delta,omitempty"`
	Usage       *UsageInfo        `json:"usage,omitempty"`
	Final       *GenerateResponse `json:"final,omitempty"`
	Err         *EngineError      `json:"error,omitempty"`
}

// EngineError is the §21.1 error schema(本文・path・credential を含めない)。
type EngineError struct {
	RequestID string `json:"request_id,omitempty"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Component string `json:"component,omitempty"`
}

func (e *EngineError) Error() string { return e.Code + ": " + e.Message }

// Engine error codes (§21.2、17種)。
const (
	CodeInvalidRequest             = "invalid_request"
	CodeAuthenticationFailed       = "authentication_failed"
	CodeModelNotLoaded             = "model_not_loaded"
	CodeUnsupportedModel           = "unsupported_model"
	CodeArtifactVerificationFailed = "artifact_verification_failed"
	CodeContextLengthExceeded      = "context_length_exceeded"
	CodeResourceExhausted          = "resource_exhausted"
	CodeCapacityExhausted          = "capacity_exhausted"
	CodeDeadlineRejected           = "deadline_rejected"
	CodeDeadlineExceeded           = "deadline_exceeded"
	CodeRequestCancelled           = "request_cancelled"
	CodeEngineDraining             = "engine_draining"
	CodeGPUOOM                     = "gpu_oom"
	CodeGPUUnhealthy               = "gpu_unhealthy"
	CodeInferenceFailed            = "inference_failed"
	CodeStreamConsumerSlow         = "stream_consumer_slow"
	CodeInternalError              = "internal_error"
)

// CapacityInfo is the §25.2 GetCapacity result。
type CapacityInfo struct {
	TotalVRAMMB              int64  `json:"total_vram_mb"`
	FreeVRAMMB               int64  `json:"free_vram_mb"`
	ReservedVRAMMB           int64  `json:"reserved_vram_mb"`
	ActiveSequences          int    `json:"active_sequences"`
	MaxSequences             int    `json:"max_sequences"`
	QueuedRequests           int    `json:"queued_requests"`
	MaxQueue                 int    `json:"max_queue"`
	KVUsedMB                 int64  `json:"kv_used_mb"`
	KVFreeMB                 int64  `json:"kv_free_mb"`
	LoadedModel              string `json:"loaded_model"`
	ContextLimit             int    `json:"context_limit"`
	EstimatedAdmissionTokens int64  `json:"estimated_admission_tokens"`
	Health                   string `json:"health"`
	Throttled                bool   `json:"throttled"`
}

// ModelStatus is the Control API model state (§20.2 の語彙)。
type ModelStatus struct {
	ModelInstanceID string `json:"model_instance_id"`
	State           string `json:"state"` // registered..unloaded (§20.2)
	ArtifactID      string `json:"artifact_id,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
}

// ManifestInfo is content-free manifest metadata (§10.2 抜粋)。
type ManifestInfo struct {
	SchemaVersion     string   `json:"schema_version"`
	ArtifactID        string   `json:"artifact_id"`
	ModelFamily       string   `json:"model_family"`
	Architecture      string   `json:"architecture"`
	ModelVersion      string   `json:"model_version"`
	WeightFormat      string   `json:"weight_format"`
	Precision         string   `json:"precision"`
	MaxContextTokens  int      `json:"max_context_tokens"`
	LicenseReviewID   string   `json:"license_review_id"`
	CertifiedProfiles []string `json:"certified_profiles"`
}

// LoadModelRequest instructs the Engine to load a signed artifact (§9.3)。
// artifact path は外部入力から直接使用しない(§12.4)— 承認済み deployment
// 設定からのみ渡す。
type LoadModelRequest struct {
	ModelInstanceID string `json:"model_instance_id"`
	ArtifactPath    string `json:"artifact_path"`
	ArtifactDigest  string `json:"artifact_digest"`
}

// EngineCapabilities is the GetCapabilities result (§9.2)。
type EngineCapabilities struct {
	EngineVersion     string   `json:"engine_version"`
	DataAPIVersion    string   `json:"data_api_version"`
	ControlAPIVersion string   `json:"control_api_version"`
	LoadedModels      []string `json:"loaded_models"`
	Streaming         bool     `json:"streaming"`
	CountTokens       bool     `json:"count_tokens"`
}

// DataAPI is the inference-path operation set (§9.2)。
// 呼出主体は Inference Orchestrator のみ(§8.5)。
type DataAPI interface {
	Generate(ctx context.Context, req *GenerateRequest) (*GenerateResponse, *EngineError)
	// GenerateStream sends ordered events to emit until completion or error.
	// emit が false を返したら consumer 切断として中止する。
	GenerateStream(ctx context.Context, req *GenerateRequest, emit func(StreamEvent) bool) *EngineError
	Cancel(ctx context.Context, requestID string) *EngineError
	CountTokens(ctx context.Context, modelInstanceID string, input Input) (int64, *EngineError)
	GetCapabilities(ctx context.Context) (*EngineCapabilities, *EngineError)
}

// ControlAPI is the lifecycle operation set (§9.3)。
// 呼出主体は Model Manager のみ(§8.2 / AT-G03)。Gateway service identity へ
// Control scope を付与しない。
type ControlAPI interface {
	LoadModel(ctx context.Context, req *LoadModelRequest) *EngineError
	UnloadModel(ctx context.Context, modelInstanceID string) *EngineError
	Drain(ctx context.Context) *EngineError
	Resume(ctx context.Context) *EngineError
	GetModelStatus(ctx context.Context, modelInstanceID string) (*ModelStatus, *EngineError)
	GetCapacity(ctx context.Context) (*CapacityInfo, *EngineError)
	GetManifest(ctx context.Context, modelInstanceID string) (*ManifestInfo, *EngineError)
}

// PlatformCode normalizes an engine error code to the frozen platform code
// (Phase 0 報告 §4.3 の凍結表)。request_cancelled は呼出側でクライアント
// 切断として扱い、この関数へ渡さない。
func PlatformCode(engineCode string) string {
	switch engineCode {
	case CodeInvalidRequest, CodeContextLengthExceeded:
		return "platform_invalid_request"
	case CodeModelNotLoaded, CodeUnsupportedModel, CodeArtifactVerificationFailed,
		CodeGPUUnhealthy, CodeEngineDraining:
		return "model_not_available"
	case CodeResourceExhausted, CodeCapacityExhausted, CodeGPUOOM:
		return "capacity_exhausted"
	case CodeDeadlineRejected, CodeDeadlineExceeded:
		return "inference_timeout"
	default:
		// authentication_failed / internal_error / inference_failed /
		// stream_consumer_slow / 未知 code
		return "internal_contract_error"
	}
}
