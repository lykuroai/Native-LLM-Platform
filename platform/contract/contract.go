// Package contract defines the versioned Gateway↔Platform boundary
// (LYK-NLP-SD-001 v2.0 §5.3, gateway-platform-v1).
//
// 第一形態は in-process binding(既存 Gateway は単一プロセス・内部APIを持たない
// ため)。interface は transport 非依存に定義し、プロセス分離時に mTLS gRPC へ
// 昇格する(Phase 0 報告 §4.1)。Gateway は認証・Policy 判定を完了してから
// Request を渡し、Platform は Gateway の認証判断を再実装しない(§5.2)。
package contract

import (
	"context"
	"encoding/json"
	"time"
)

// Version is the frozen contract identifier (§5.3).
const Version = "gateway-platform-v1"

// PolicyContext carries the Gateway-resolved policy decision (§5.2)。
// Virtual Key 原文・JWT 原文は含めない。
type PolicyContext struct {
	DataClass     string   // 宣言された data class(空 = 未宣言)
	RoutingMode   string   // "" | "local-only" | "hybrid"
	AllowedModels []string // 空 = 全 logical model 許可(キー由来)
	ToolsAllowed  bool
}

// ConversationContext identifies an opt-in managed conversation (§10)。
type ConversationContext struct {
	ID              string
	Version         int64  // optimistic lock(0 = 未指定)
	RetentionPolicy string // client希望。tenant policy が優先(§13.1)
}

// Request is the normalized inference request (§5.3 contract必須項目)。
// NormalizedInput は OpenAI 互換 body のフィールド集合(gwcore の透過原則を
// 維持し再構造化しない)。model フィールドは LogicalModel として解決済み。
type Request struct {
	ContractVersion string
	RequestID       string
	TraceID         string
	TenantScope     string // gateway_id 由来の内部識別子(顧客環境=単一tenant)
	ProjectScope    string
	ActorScope      string // virtual_key_id
	Policy          PolicyContext
	LogicalModel    string
	// PoolID optionally narrows candidate deployments to a named pool
	// (platform.pools[]、LYK-NLP-MRCI-002 §7.2)。空 = 全 deployment。
	// v0.9.0 での追加(後方互換: ゼロ値で従来動作)。
	PoolID          string
	Endpoint        string // 上流パス(/v1/chat/completions 等)
	NormalizedInput map[string]json.RawMessage
	Stream          bool
	Deadline        time.Time
	Conversation    *ConversationContext
	IdempotencyKey  string // 保存を伴う操作のみ(§5.3)
}

// Usage is content-free token accounting (§1.4)。
type Usage struct {
	InputTokens  *int64
	OutputTokens *int64
}

// Response is a non-streaming result (§5.2: PlatformからGatewayへ返す情報)。
type Response struct {
	StatusCode          int
	ContentType         string
	Body                []byte
	Usage               Usage
	DeploymentID        string // 非秘密ID
	RouteType           string // "native" | "connector"(秘密endpoint名は返さない §1.4)
	ConversationVersion int64  // conversation 利用時の新 version
}

// StreamResult summarizes a completed stream (usage sniff 相当)。
type StreamResult struct {
	Usage               Usage
	DeploymentID        string
	RouteType           string
	ConversationVersion int64
	// ClientGone reports the client disconnected mid-stream(監査用)。
	ClientGone bool
}

// StreamWriter is the transparent SSE sink。Platform は上流の生バイトを
// そのまま書く(§0.3: レスポンスは生バイトで透過)。
type StreamWriter interface {
	// Start is called exactly once before the first chunk with the upstream
	// status code and content type(SSE 開始後は HTTP status を変更できない)。
	Start(statusCode int, contentType string)
	// Chunk writes one line/chunk. 返り値 false はクライアント切断。
	Chunk(p []byte) bool
	Flush()
}

// Error is the stable Platform error (§13.1.1 で凍結した code 集合)。
type Error struct {
	Code       string // platform_invalid_request | conversation_conflict | ...
	Message    string
	RetryAfter int // 秒。0 = 提示なし
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Frozen platform error codes (§13.1.1)。
const (
	ErrInvalidRequest       = "platform_invalid_request"
	ErrConversationConflict = "conversation_conflict"
	ErrModelNotAllowed      = "model_not_allowed"
	ErrModelNotAvailable    = "model_not_available"
	ErrCapacityExhausted    = "capacity_exhausted"
	ErrInferenceTimeout     = "inference_timeout"
	ErrPlatformNotReady     = "platform_not_ready"
	ErrInternalContract     = "internal_contract_error"
)

// HTTPStatus maps a frozen platform code to the Gateway HTTP status (§13.1.1)。
func HTTPStatus(code string) int {
	switch code {
	case ErrInvalidRequest:
		return 400
	case ErrConversationConflict:
		return 409
	case ErrModelNotAllowed:
		return 403
	case ErrModelNotAvailable, ErrPlatformNotReady:
		return 503
	case ErrCapacityExhausted:
		return 429
	case ErrInferenceTimeout:
		return 504
	default:
		return 502 // internal_contract_error ほか未知は 502(§13.1.1)
	}
}

// GatewayErrorCode maps a platform code to the gwcore外部エラーcode
// (Phase 0 報告 §4.2 の凍結表)。
func GatewayErrorCode(code string) string {
	switch code {
	case ErrInvalidRequest:
		return "invalid_request"
	case ErrConversationConflict:
		return "conversation_conflict"
	case ErrModelNotAllowed:
		return "policy_denied"
	case ErrModelNotAvailable, ErrPlatformNotReady:
		return "model_unavailable"
	case ErrCapacityExhausted:
		return "rate_limit_exceeded"
	case ErrInferenceTimeout:
		return "inference_timeout"
	default:
		return "server_error"
	}
}

// ModelInfo is a non-secret logical model entry for /v1/models(物理情報非開示)。
type ModelInfo struct {
	LogicalName string
	Ready       bool
}

// RouteStatus reports deployment readiness for one logical model (§5.3)。
type RouteStatus struct {
	LogicalModel string
	Ready        bool
	Deployments  []DeploymentStatus
}

// DeploymentStatus is a non-secret deployment state view。
type DeploymentStatus struct {
	ID        string
	RouteType string // native | connector
	Status    string // available | unavailable | draining | suspended
}

// Capabilities describes what the Platform build supports (§5.3)。
type Capabilities struct {
	ContractVersion   string
	Streaming         bool
	Conversation      bool // Conversation Memory 有効
	NativeEngine      bool // 実 Engine 接続済み
	DistributedPool   bool
	TaskDecomposition bool
}

// Backend is the gateway-platform-v1 operation set (§5.3)。
// ListModels は /v1/models 提供のための拡張(非秘密 logical 名のみ)。
type Backend interface {
	GetPlatformCapabilities(ctx context.Context) Capabilities
	Execute(ctx context.Context, req *Request) (*Response, *Error)
	ExecuteStream(ctx context.Context, req *Request, w StreamWriter) (*StreamResult, *Error)
	Cancel(ctx context.Context, requestID string)
	CountTokens(ctx context.Context, req *Request) (int64, *Error)
	GetRouteStatus(ctx context.Context, logicalModel string) (*RouteStatus, *Error)
	ListModels(ctx context.Context) []ModelInfo
}
