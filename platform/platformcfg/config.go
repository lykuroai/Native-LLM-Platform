// Package platformcfg defines the `platform:` section of the signed gateway
// configuration (LYK-NLP-SD-001 v2.0 §16.11 相当)。
//
// Platform の静的構成(model catalog・runtime endpoint・retention 等)は
// 既存 Gateway の署名済み config 配信(Ed25519+世代管理+LKG)に同乗する —
// 別 Agent・別配信路を作らない(§2.3)。approval workflow は Control Plane
// 側で完結し、顧客環境へは承認結果だけが配信される。
// secret 実値は置かない — credential はファイル参照のみ(§12.5)。
package platformcfg

import (
	"fmt"
	"net/url"
)

// Backend types (§14.3)。
const (
	BackendNativeEngine      = "native_engine"
	BackendExternalConnector = "external_connector"
)

// Approval states (§6.6)。顧客環境で routing 可能なのは approved のみ。
const (
	ApprovalApproved  = "approved"
	ApprovalSuspended = "suspended"
	ApprovalRetired   = "retired"
)

// Management modes (§6.5)。
const (
	ManageObserveOnly      = "observe_only"
	ManageInferenceOnly    = "inference_only"
	ManageLifecycleLimited = "lifecycle_limited"
	ManageNativeManaged    = "native_managed"
)

// Config is the platform: section root。
type Config struct {
	Enabled          bool              `yaml:"enabled"`
	Models           []ModelEntry      `yaml:"models"`
	RuntimeEndpoints []RuntimeEndpoint `yaml:"runtime_endpoints"`
	Engines          []EngineRef       `yaml:"engines"`
	Orchestrator     Orchestrator      `yaml:"orchestrator"`
	Memory           Memory            `yaml:"memory"`
	Pool             Pool              `yaml:"pool"`
}

// ModelEntry is one Logical Model in the distributed catalog (§6.3)。
type ModelEntry struct {
	LogicalName        string       `yaml:"logical_name"`
	DisplayName        string       `yaml:"display_name"`
	ApprovalStatus     string       `yaml:"approval_status"`      // approved | suspended | retired
	AllowedDataClasses []string     `yaml:"allowed_data_classes"` // 空 = gateway 設定に従う
	Capabilities       Capabilities `yaml:"capabilities"`
	Deployments        []Deployment `yaml:"deployments"`
}

type Capabilities struct {
	Chat          bool `yaml:"chat"`
	Streaming     bool `yaml:"streaming"`
	Embeddings    bool `yaml:"embeddings"`
	ToolCalling   bool `yaml:"tool_calling"`
	ContextLength int  `yaml:"context_length"`
}

// Deployment binds a logical model to one backend (§14.3)。
type Deployment struct {
	ID          string `yaml:"id"`
	BackendType string `yaml:"backend_type"` // native_engine | external_connector
	// external_connector 用
	RuntimeEndpointID string `yaml:"runtime_endpoint_id"`
	PhysicalModel     string `yaml:"physical_model"`
	// native_engine 用
	EngineID        string `yaml:"engine_id"`
	ModelInstanceID string `yaml:"model_instance_id"`
	ArtifactPath    string `yaml:"artifact_path"`
	ArtifactDigest  string `yaml:"artifact_digest"`
}

// RuntimeEndpoint is a customer-managed runtime (§6.4)。credential 原文は
// 保存しない — CredentialFile はファイル参照。
type RuntimeEndpoint struct {
	ID             string `yaml:"id"`
	DisplayName    string `yaml:"display_name"`
	ConnectorType  string `yaml:"connector_type"` // openai_compatible のみ(Phase 3)
	BaseURL        string `yaml:"base_url"`
	NetworkZone    string `yaml:"network_zone"`
	CredentialFile string `yaml:"credential_file"`
	ManagementMode string `yaml:"management_mode"`
	HealthPath     string `yaml:"health_path"` // 既定 /v1/models
}

// EngineRef declares a Native Engine binding (Native Edition のみ)。
// 実 Engine は実装中のため endpoint binding は将来項目 — mock: は contract
// テスト・PoC 用。
type EngineRef struct {
	ID       string `yaml:"id"`
	Endpoint string `yaml:"endpoint"` // "mock:" | (将来) mtls://host:port
}

type Orchestrator struct {
	MaxConcurrent          int `yaml:"max_concurrent"`           // 既定 64
	MaxQueue               int `yaml:"max_queue"`                // 既定 256
	DefaultDeadlineSeconds int `yaml:"default_deadline_seconds"` // 既定 120
}

// Memory は Conversation Memory の opt-in 設定(§10.2)。
type Memory struct {
	// Mode: stateless(既定・本文保存0日) | managed(明示 opt-in)
	Mode              string    `yaml:"mode"`
	DataDir           string    `yaml:"data_dir"`
	EncryptionKeyFile string    `yaml:"encryption_key_file"`
	Retention         Retention `yaml:"retention"`
	// SummaryModel is the logical model used for conversation summaries。
	// 空 = summary 生成なし(古い message は文脈から外れるのみ)。
	SummaryModel string `yaml:"summary_model"`
}

type Retention struct {
	MessageDays    int `yaml:"message_days"`     // 既定 30
	SummaryDays    int `yaml:"summary_days"`     // 既定 message_days と同じ
	KVCacheMinutes int `yaml:"kv_cache_minutes"` // 既定 15、上限 60(§11.1)
}

type Pool struct {
	// StickyTTLSeconds bounds sticky routing (§9.5)。0 = 既定 300。
	StickyTTLSeconds int `yaml:"sticky_ttl_seconds"`
	// HealthIntervalSeconds は node health probe 間隔。0 = 既定 30。
	HealthIntervalSeconds int `yaml:"health_interval_seconds"`
}

// Validate は Fail Closed 前提で platform 設定を検査する。
func (c *Config) Validate() error {
	if !c.Enabled {
		return nil // 無効時は内容を検査しない(旧 route 維持)
	}
	rteIDs := map[string]bool{}
	for i, r := range c.RuntimeEndpoints {
		if r.ID == "" {
			return fmt.Errorf("platform: runtime_endpoints[%d].id is required", i)
		}
		if rteIDs[r.ID] {
			return fmt.Errorf("platform: duplicate runtime_endpoint id %q", r.ID)
		}
		rteIDs[r.ID] = true
		if r.ConnectorType != "openai_compatible" {
			return fmt.Errorf("platform: runtime_endpoints[%d].connector_type %q is not supported", i, r.ConnectorType)
		}
		u, err := url.Parse(r.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("platform: runtime_endpoints[%d].base_url %q is not a valid http(s) URL", i, r.BaseURL)
		}
		switch r.ManagementMode {
		case "", ManageObserveOnly, ManageInferenceOnly, ManageLifecycleLimited:
		case ManageNativeManaged:
			return fmt.Errorf("platform: runtime_endpoints[%d]: native_managed is reserved for engines", i)
		default:
			return fmt.Errorf("platform: runtime_endpoints[%d].management_mode %q is invalid", i, r.ManagementMode)
		}
	}
	engIDs := map[string]bool{}
	for i, e := range c.Engines {
		if e.ID == "" {
			return fmt.Errorf("platform: engines[%d].id is required", i)
		}
		if engIDs[e.ID] {
			return fmt.Errorf("platform: duplicate engine id %q", e.ID)
		}
		engIDs[e.ID] = true
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("platform: at least one model is required when enabled")
	}
	names := map[string]bool{}
	for i, m := range c.Models {
		if m.LogicalName == "" {
			return fmt.Errorf("platform: models[%d].logical_name is required", i)
		}
		if names[m.LogicalName] {
			return fmt.Errorf("platform: duplicate logical_name %q", m.LogicalName)
		}
		names[m.LogicalName] = true
		switch m.ApprovalStatus {
		case ApprovalApproved, ApprovalSuspended, ApprovalRetired:
		default:
			// 未承認 model を配信 config に含めない(Fail Closed)
			return fmt.Errorf("platform: models[%d].approval_status %q is invalid (approved/suspended/retired)", i, m.ApprovalStatus)
		}
		for _, dc := range m.AllowedDataClasses {
			if !ValidDataClass(dc) {
				return fmt.Errorf("platform: models[%d] invalid data class %q", i, dc)
			}
		}
		if len(m.Deployments) == 0 {
			return fmt.Errorf("platform: models[%d] requires at least one deployment", i)
		}
		depIDs := map[string]bool{}
		for j, d := range m.Deployments {
			if d.ID == "" {
				return fmt.Errorf("platform: models[%d].deployments[%d].id is required", i, j)
			}
			if depIDs[d.ID] {
				return fmt.Errorf("platform: models[%d] duplicate deployment id %q", i, d.ID)
			}
			depIDs[d.ID] = true
			switch d.BackendType {
			case BackendExternalConnector:
				if !rteIDs[d.RuntimeEndpointID] {
					return fmt.Errorf("platform: models[%d].deployments[%d] references unknown runtime_endpoint %q", i, j, d.RuntimeEndpointID)
				}
				if d.PhysicalModel == "" {
					return fmt.Errorf("platform: models[%d].deployments[%d].physical_model is required", i, j)
				}
			case BackendNativeEngine:
				if !engIDs[d.EngineID] {
					return fmt.Errorf("platform: models[%d].deployments[%d] references unknown engine %q", i, j, d.EngineID)
				}
				if d.ModelInstanceID == "" || d.ArtifactDigest == "" {
					return fmt.Errorf("platform: models[%d].deployments[%d] requires model_instance_id and artifact_digest", i, j)
				}
			default:
				return fmt.Errorf("platform: models[%d].deployments[%d].backend_type %q is invalid", i, j, d.BackendType)
			}
		}
	}
	if c.Orchestrator.MaxConcurrent <= 0 {
		c.Orchestrator.MaxConcurrent = 64
	}
	if c.Orchestrator.MaxQueue <= 0 {
		c.Orchestrator.MaxQueue = 256
	}
	if c.Orchestrator.DefaultDeadlineSeconds <= 0 {
		c.Orchestrator.DefaultDeadlineSeconds = 120
	}
	switch c.Memory.Mode {
	case "", "stateless":
		c.Memory.Mode = "stateless"
	case "managed":
		if c.Memory.DataDir == "" {
			return fmt.Errorf("platform: memory.data_dir is required for managed mode")
		}
		if c.Memory.EncryptionKeyFile == "" {
			// 本文を平文で保存しない(§14.13)
			return fmt.Errorf("platform: memory.encryption_key_file is required for managed mode")
		}
		if c.Memory.Retention.MessageDays <= 0 {
			c.Memory.Retention.MessageDays = 30
		}
		if c.Memory.Retention.SummaryDays <= 0 {
			c.Memory.Retention.SummaryDays = c.Memory.Retention.MessageDays
		}
		if c.Memory.SummaryModel != "" && !names[c.Memory.SummaryModel] {
			return fmt.Errorf("platform: memory.summary_model %q is not a known model", c.Memory.SummaryModel)
		}
	default:
		return fmt.Errorf("platform: memory.mode %q is invalid (stateless/managed)", c.Memory.Mode)
	}
	if c.Memory.Retention.KVCacheMinutes <= 0 {
		c.Memory.Retention.KVCacheMinutes = 15
	}
	if c.Memory.Retention.KVCacheMinutes > 60 {
		return fmt.Errorf("platform: memory.retention.kv_cache_minutes must be <= 60")
	}
	if c.Pool.StickyTTLSeconds <= 0 {
		c.Pool.StickyTTLSeconds = 300
	}
	if c.Pool.HealthIntervalSeconds <= 0 {
		c.Pool.HealthIntervalSeconds = 30
	}
	return nil
}

// FindModel resolves a catalog entry by logical name.
func (c *Config) FindModel(logical string) *ModelEntry {
	for i := range c.Models {
		if c.Models[i].LogicalName == logical {
			return &c.Models[i]
		}
	}
	return nil
}

// FindEndpoint resolves a runtime endpoint by ID.
func (c *Config) FindEndpoint(id string) *RuntimeEndpoint {
	for i := range c.RuntimeEndpoints {
		if c.RuntimeEndpoints[i].ID == id {
			return &c.RuntimeEndpoints[i]
		}
	}
	return nil
}

// ValidDataClass reports whether c is a known data classification (BD §11.1)。
// 値集合の変更は配信 config の互換性に影響する(semver メジャー)。
func ValidDataClass(c string) bool {
	switch c {
	case "public", "internal", "confidential", "restricted":
		return true
	}
	return false
}
