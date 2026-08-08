// Package gwcore is the customer-side Private LLM Gateway runtime
// (docs/20260806/ LYK-PLG-BD-001 §7-§12, Phase 4 MVP).
//
// MVP は「配布された gateway.yaml 駆動の stateless プロキシ」: Virtual Key
// (ハッシュ)・モデル表・ポリシーは設定ファイルが権威で、DB を持たない。
// 設定の署名検証・世代管理は Phase 5 の Control Agent が担う。
package gwcore

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/lykuroai/Native-LLM-Platform/platform/platformcfg"
)

// Config is the customer-side gateway configuration (BD §13.4 の実体).
// secret 値は置かない — Virtual Key はハッシュのみ保持する。
type Config struct {
	SchemaVersion string         `yaml:"schema_version"`
	Gateway       GatewaySection `yaml:"gateway"`
	Auth          AuthSection    `yaml:"auth"`
	Models        []ModelDef     `yaml:"models"`
	Audit         AuditSection   `yaml:"audit"`
	Hybrid        HybridSection  `yaml:"hybrid"`
	// Platform is the optional Native LLM Platform section
	// (LYK-NLP-SD-001 v2.0)。nil または enabled:false で既存 route を
	// 完全維持する(feature flag / rollback 経路、Phase 1)。
	Platform *platformcfg.Config `yaml:"platform"`
}

// PlatformEnabled reports whether the platform route is active。
func (c *Config) PlatformEnabled() bool {
	return c.Platform != nil && c.Platform.Enabled
}

type GatewaySection struct {
	ID                 string   `yaml:"id"`
	Listen             string   `yaml:"listen"`
	StrictLocalMode    *bool    `yaml:"strict_local_mode"`
	AllowedDataClasses []string `yaml:"allowed_data_classes"`
	// MaxRequestBytes limits request bodies (既定 10MiB)。
	MaxRequestBytes int64 `yaml:"max_request_bytes"`
}

type AuthSection struct {
	VirtualKeys []VirtualKeyDef `yaml:"virtual_keys"`
}

// VirtualKeyDef is one gateway-local virtual key (BD §10.4)。
// KeyHash は SHA-256 hex。原文は設定に置かない。
type VirtualKeyDef struct {
	ID            string   `yaml:"id"`
	Name          string   `yaml:"name"`
	KeyHash       string   `yaml:"key_hash"`
	AllowedModels []string `yaml:"allowed_models"` // 空 = 全 logical model 可
	RPMLimit      int      `yaml:"rpm_limit"`      // 0 = 無制限
	Disabled      bool     `yaml:"disabled"`
	// AllowTools controls function calling / tool use (Phase 7 統制)。
	// 未指定 = 許可。false なら tools / tool_choice 付き要求を拒否。
	AllowTools *bool `yaml:"allow_tools"`
}

// ToolsAllowed reports whether the key may send tool definitions.
func (k *VirtualKeyDef) ToolsAllowed() bool {
	return k.AllowTools == nil || *k.AllowTools
}

// ModelDef maps a Logical Model to physical runtime deployments (BD §12.1)。
// Fallback は同一 logical model のローカル候補群(BD §12.3 fallback group、
// §21: Runtime未ロード時に許可済みLocal候補へ切替)。
type ModelDef struct {
	LogicalName   string `yaml:"logical_name"`
	Runtime       string `yaml:"runtime"`  // vllm | ollama | tgi | openai_compatible
	Endpoint      string `yaml:"endpoint"` // OpenAI互換APIのベースURL(パス /v1 を含めない)
	PhysicalModel string `yaml:"physical_model"`
	// Fallback candidates (順に試行)。
	Fallback []ModelTarget `yaml:"fallback"`
	// CloudFallback routes to the approved cloud when ALL local candidates
	// fail (Phase 7 Hybrid、hybrid.enabled 必須)。
	CloudFallback *CloudFallback `yaml:"cloud_fallback"`
}

// ModelTarget is one runtime deployment candidate.
type ModelTarget struct {
	Runtime       string `yaml:"runtime"`
	Endpoint      string `yaml:"endpoint"`
	PhysicalModel string `yaml:"physical_model"`
}

// Targets returns primary + fallback candidates in priority order.
func (m *ModelDef) Targets() []ModelTarget {
	out := make([]ModelTarget, 0, 1+len(m.Fallback))
	out = append(out, ModelTarget{Runtime: m.Runtime, Endpoint: m.Endpoint, PhysicalModel: m.PhysicalModel})
	return append(out, m.Fallback...)
}

// CloudFallback names the model on the approved cloud endpoint.
type CloudFallback struct {
	Model string `yaml:"model"` // 例 deepseek/deepseek-chat(Lykuro SaaS 複合キー)
}

// HybridSection enables the approved-cloud escape hatch (BD §5.1 Hybrid、
// feature flag + 個別承認前提)。strict_local_mode を明示 OFF にしない限り
// 有効化できない(検証で強制)。
type HybridSection struct {
	Enabled  bool   `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"` // 承認済みcloud(通常 Lykuro SaaS)のベースURL
	// APIKeyFile holds the cloud credential (設定本文へ鍵を書かない)。
	APIKeyFile string `yaml:"api_key_file"`
	// AllowedDataClasses may only contain public / internal
	// (confidential/restricted は常に Local のみ、BD §11.1)。
	AllowedDataClasses []string `yaml:"allowed_data_classes"`
}

type AuditSection struct {
	// Path is the JSONL audit log destination. 空 = stdout。
	Path string `yaml:"path"`
}

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// StrictLocal reports the effective strict-local flag (既定 ON、BD §0.3)。
func (c *Config) StrictLocal() bool {
	return c.Gateway.StrictLocalMode == nil || *c.Gateway.StrictLocalMode
}

// LoadConfig reads and validates the gateway configuration.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return ParseConfig(raw)
}

// ParseConfig parses and validates configuration bytes.
func ParseConfig(raw []byte) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate は Fail Closed 前提で設定を検査する(BD §0.3: 不明は拒否)。
func (c *Config) Validate() error {
	if c.SchemaVersion != "1" {
		return fmt.Errorf("config: unsupported schema_version %q", c.SchemaVersion)
	}
	if c.Gateway.ID == "" {
		return fmt.Errorf("config: gateway.id is required")
	}
	if c.Gateway.Listen == "" {
		c.Gateway.Listen = ":8443"
	}
	if c.Gateway.MaxRequestBytes <= 0 {
		c.Gateway.MaxRequestBytes = 10 << 20 // 10MiB
	}
	if len(c.Gateway.AllowedDataClasses) == 0 {
		// 未指定は最小権限(public のみ)
		c.Gateway.AllowedDataClasses = []string{"public"}
	}
	for _, dc := range c.Gateway.AllowedDataClasses {
		if !platformcfg.ValidDataClass(dc) {
			return fmt.Errorf("config: invalid allowed_data_classes entry %q", dc)
		}
	}
	if len(c.Models) == 0 && !c.PlatformEnabled() {
		// platform 有効時は logical model catalog が platform 節側にある
		return fmt.Errorf("config: at least one model is required")
	}
	validTarget := func(where string, t ModelTarget) error {
		if !validRuntimeAdapter(t.Runtime) {
			return fmt.Errorf("config: %s.runtime %q is invalid", where, t.Runtime)
		}
		u, err := url.Parse(t.Endpoint)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("config: %s.endpoint %q is not a valid URL", where, t.Endpoint)
		}
		if t.PhysicalModel == "" {
			return fmt.Errorf("config: %s.physical_model is required", where)
		}
		return nil
	}
	seen := map[string]bool{}
	for i, m := range c.Models {
		if m.LogicalName == "" {
			return fmt.Errorf("config: models[%d].logical_name is required", i)
		}
		if seen[m.LogicalName] {
			return fmt.Errorf("config: duplicate logical_name %q", m.LogicalName)
		}
		seen[m.LogicalName] = true
		if err := validTarget(fmt.Sprintf("models[%d]", i), ModelTarget{
			Runtime: m.Runtime, Endpoint: m.Endpoint, PhysicalModel: m.PhysicalModel}); err != nil {
			return err
		}
		for j, f := range m.Fallback {
			if err := validTarget(fmt.Sprintf("models[%d].fallback[%d]", i, j), f); err != nil {
				return err
			}
		}
		if m.CloudFallback != nil {
			if !c.Hybrid.Enabled {
				return fmt.Errorf("config: models[%d].cloud_fallback requires hybrid.enabled", i)
			}
			// Lykuro SaaS へは provider プレフィックスなしパスで送るため、
			// モデルは方式Bの複合キー(provider/model)であることを要求する。
			provider, mdl, ok := strings.Cut(m.CloudFallback.Model, "/")
			if !ok || provider == "" || mdl == "" {
				return fmt.Errorf("config: models[%d].cloud_fallback.model must be a composite key like provider/model, got %q",
					i, m.CloudFallback.Model)
			}
		}
	}

	// Hybrid(BD §5.1): 明示 opt-in + Strict Local との排他を Fail Closed で強制。
	if c.Hybrid.Enabled {
		if c.StrictLocal() {
			return fmt.Errorf("config: hybrid.enabled requires strict_local_mode to be explicitly false")
		}
		u, err := url.Parse(c.Hybrid.Endpoint)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("config: hybrid.endpoint %q is not a valid URL", c.Hybrid.Endpoint)
		}
		if c.Hybrid.APIKeyFile == "" {
			return fmt.Errorf("config: hybrid.api_key_file is required")
		}
		if len(c.Hybrid.AllowedDataClasses) == 0 {
			c.Hybrid.AllowedDataClasses = []string{"public"}
		}
		for _, dc := range c.Hybrid.AllowedDataClasses {
			// confidential/restricted は常に Local のみ(BD §11.1)。設定でも緩められない。
			if dc != "public" && dc != "internal" {
				return fmt.Errorf("config: hybrid.allowed_data_classes may only contain public/internal, got %q", dc)
			}
		}
	}
	// Platform 節(署名済み config に同乗、Fail Closed)
	if c.Platform != nil {
		if err := c.Platform.Validate(); err != nil {
			return err
		}
		if c.PlatformEnabled() {
			// platform catalog の logical model もキー許可検証の対象にする
			for _, m := range c.Platform.Models {
				seen[m.LogicalName] = true
			}
		}
	}

	keyIDs := map[string]bool{}
	for i, k := range c.Auth.VirtualKeys {
		if k.ID == "" {
			return fmt.Errorf("config: virtual_keys[%d].id is required", i)
		}
		if keyIDs[k.ID] {
			return fmt.Errorf("config: duplicate virtual key id %q", k.ID)
		}
		keyIDs[k.ID] = true
		if !sha256HexRe.MatchString(k.KeyHash) {
			return fmt.Errorf("config: virtual_keys[%d].key_hash must be sha256 hex", i)
		}
		for _, am := range k.AllowedModels {
			if !seen[am] {
				return fmt.Errorf("config: virtual_keys[%d] allows unknown model %q", i, am)
			}
		}
	}
	return nil
}

// FindModel resolves a logical model name.
func (c *Config) FindModel(logical string) *ModelDef {
	for i := range c.Models {
		if c.Models[i].LogicalName == logical {
			return &c.Models[i]
		}
	}
	return nil
}

// DataClassAllowed reports whether the declared class may be processed here.
func (c *Config) DataClassAllowed(class string) bool {
	for _, dc := range c.Gateway.AllowedDataClasses {
		if dc == class {
			return true
		}
	}
	return false
}

// CloudEligible reports whether a request may leave for the approved cloud
// (Phase 7 Hybrid)。ポリシー最優先(BD §8.2):
//   - hybrid有効 + 当該モデルに cloud_fallback があること
//   - X-Routing-Mode: local-only は常に優先
//   - X-Data-Class が明示宣言され、かつ hybrid 許可リスト内であること
//     (未宣言は分類不明 = Fail Closed で Local のみ)
func (c *Config) CloudEligible(def *ModelDef, dataClass, routingMode string) bool {
	if c.StrictLocal() || !c.Hybrid.Enabled || def.CloudFallback == nil {
		return false
	}
	if routingMode == "local-only" {
		return false
	}
	if dataClass == "" {
		return false
	}
	for _, dc := range c.Hybrid.AllowedDataClasses {
		if dc == dataClass {
			return true
		}
	}
	return false
}

// validRuntimeAdapter reports whether t is a known local runtime type
// (対応 Runtime の種別。追加時は README も更新すること)。
func validRuntimeAdapter(t string) bool {
	switch t {
	case "vllm", "ollama", "tgi", "openai_compatible":
		return true
	}
	return false
}
