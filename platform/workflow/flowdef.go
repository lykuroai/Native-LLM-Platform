// Package workflow implements the W1 Workflow Orchestrator
// (LYK-NLP-MRCI-001 v1.1 を LYK-NLP-MRCI-002 で本リポジトリへ縮退適用)。
//
// 責務: Flow 定義の検証・ファイル永続化・Sequential 連鎖実行・Handoff・
// Retry/Fallback・Run/Event 管理。Step の実行は contract 経由で既存
// orchestrator へ委譲し、Runtime 選択・failover・pool を再実装しない(§2.3)。
package workflow

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// W1 の Flow 定義(MRCI-001 §7)。未知フィールドは Parse 時に拒否する
// (W2/W3 機能・output_schema 等を「受理して無視」しない — Fail Closed)。
type Definition struct {
	Name             string            `json:"name"`
	Alias            string            `json:"alias"`
	Description      string            `json:"description,omitempty"`
	ExecutionMode    string            `json:"execution_mode"`              // SEQUENTIAL のみ
	DataClass        string            `json:"data_class,omitempty"`        // 空 = 未宣言
	ConversationMode string            `json:"conversation_mode,omitempty"` // STATELESS のみ
	InputsSchema     *InputsSchema     `json:"inputs_schema"`
	Steps            []Step            `json:"steps"`
	OutputMapping    map[string]string `json:"output_mapping"`
	Limits           Limits            `json:"limits,omitempty"`
}

// InputsSchema is the W1 subset of JSON Schema(object + string properties)。
type InputsSchema struct {
	Type                 string               `json:"type"` // object のみ
	Required             []string             `json:"required,omitempty"`
	Properties           map[string]InputProp `json:"properties"`
	AdditionalProperties *bool                `json:"additionalProperties,omitempty"`
}

type InputProp struct {
	Type      string `json:"type"` // string のみ
	MaxLength int    `json:"maxLength,omitempty"`
}

type Step struct {
	StepID         string            `json:"step_id"`
	Name           string            `json:"name,omitempty"`
	Type           string            `json:"type"` // LLM のみ(W1)
	DependsOn      []string          `json:"depends_on,omitempty"`
	RuntimeTarget  RuntimeTarget     `json:"runtime_target"`
	SystemPrompt   string            `json:"system_prompt,omitempty"`
	InputMapping   map[string]string `json:"input_mapping"`
	Generation     *Generation       `json:"generation,omitempty"`
	TimeoutMS      int               `json:"timeout_ms,omitempty"`
	RetryPolicy    *RetryPolicy      `json:"retry_policy,omitempty"`
	FallbackPolicy *FallbackPolicy   `json:"fallback_policy,omitempty"`
	OnError        string            `json:"on_error,omitempty"` // STOP のみ(W1)
}

type RuntimeTarget struct {
	LogicalModel string `json:"logical_model"`
	PoolID       string `json:"pool_id,omitempty"`
}

type Generation struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
}

type RetryPolicy struct {
	MaxAttempts         int  `json:"max_attempts"`
	PreferDifferentNode bool `json:"prefer_different_node,omitempty"`
	BackoffMS           int  `json:"backoff_ms,omitempty"`
}

type FallbackPolicy struct {
	AllowedPoolIDs          []string `json:"allowed_pool_ids"`
	AllowDegradedCapability bool     `json:"allow_degraded_capability,omitempty"`
}

type Limits struct {
	TimeoutMS      int   `json:"timeout_ms,omitempty"`       // 既定 180000
	MaxTotalTokens int64 `json:"max_total_tokens,omitempty"` // 0 = 無制限
	MaxSteps       int   `json:"max_steps,omitempty"`        // 上限 20(W1)
	MaxParallelism int   `json:"max_parallelism,omitempty"`  // W1 は 1 のみ
}

// W1 caps(MRCI-002 §3 の Scale 縮退値)。
const (
	MaxStepsW1          = 20
	MaxAttemptsCap      = 5
	MaxInputLenDefault  = 100000
	DefaultTimeoutMS    = 180000
	DefaultMaxAttempts  = 1
	MaxTemplateOutBytes = 4 << 20 // 展開後 Input の上限 4MiB
)

var stepIDPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var aliasPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ParseDefinition decodes JSON strictly(未知フィールド拒否)。
func ParseDefinition(raw []byte) (*Definition, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var def Definition
	if err := dec.Decode(&def); err != nil {
		return nil, fmt.Errorf("flow definition: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("flow definition: trailing data after JSON object")
	}
	return &def, nil
}

// Resolver answers catalog questions during validation(modelmanager /
// platformcfg への依存を interface で切る)。
type Resolver interface {
	ModelExists(logicalName string) bool
	PoolExists(poolID string) bool
}

// Validate enforces MRCI-001 §7.6 の規則(W1 縮退版)。エラーは全件返す。
func (d *Definition) Validate(r Resolver) []error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if d.Name == "" {
		fail("name is required")
	}
	if !aliasPattern.MatchString(d.Alias) {
		fail("alias %q is invalid (lowercase letters, digits, hyphen, max 64)", d.Alias)
	}
	if d.ExecutionMode != "SEQUENTIAL" {
		fail("execution_mode must be SEQUENTIAL in W1")
	}
	switch d.ConversationMode {
	case "", "STATELESS":
	default:
		fail("conversation_mode %q is not supported in W1 (STATELESS only)", d.ConversationMode)
	}
	if d.Limits.MaxParallelism > 1 {
		fail("limits.max_parallelism must be 1 in W1")
	}
	maxSteps := d.Limits.MaxSteps
	if maxSteps <= 0 || maxSteps > MaxStepsW1 {
		maxSteps = MaxStepsW1
	}
	if len(d.Steps) == 0 {
		fail("at least one step is required")
	}
	if len(d.Steps) > maxSteps {
		fail("flow has %d steps, limit is %d", len(d.Steps), maxSteps)
	}

	// inputs schema(W1 subset)
	inputNames := map[string]bool{}
	if d.InputsSchema == nil {
		fail("inputs_schema is required")
	} else {
		if d.InputsSchema.Type != "object" {
			fail("inputs_schema.type must be \"object\"")
		}
		if len(d.InputsSchema.Properties) == 0 {
			fail("inputs_schema.properties must define at least one input")
		}
		for name, p := range d.InputsSchema.Properties {
			if p.Type != "string" {
				fail("inputs_schema.properties.%s.type must be \"string\" in W1", name)
			}
			inputNames[name] = true
		}
		for _, req := range d.InputsSchema.Required {
			if !inputNames[req] {
				fail("inputs_schema.required references unknown property %q", req)
			}
		}
	}

	// steps
	seen := map[string]int{}
	for i, s := range d.Steps {
		prefix := fmt.Sprintf("steps[%d]", i)
		if !stepIDPattern.MatchString(s.StepID) {
			fail("%s.step_id %q is invalid (lowercase, digits, underscore, max 64)", prefix, s.StepID)
			continue
		}
		if _, dup := seen[s.StepID]; dup {
			fail("%s.step_id %q is not unique", prefix, s.StepID)
			continue
		}
		if s.Type != "LLM" {
			fail("%s.type %q is not supported in W1 (LLM only)", prefix, s.Type)
		}
		switch s.OnError {
		case "", "STOP":
		default:
			fail("%s.on_error %q is not supported in W1 (STOP only)", prefix, s.OnError)
		}
		if s.RuntimeTarget.LogicalModel == "" {
			fail("%s.runtime_target.logical_model is required", prefix)
		} else if r != nil && !r.ModelExists(s.RuntimeTarget.LogicalModel) {
			fail("%s.runtime_target.logical_model %q is not a known approved model", prefix, s.RuntimeTarget.LogicalModel)
		}
		if s.RuntimeTarget.PoolID != "" && r != nil && !r.PoolExists(s.RuntimeTarget.PoolID) {
			fail("%s.runtime_target.pool_id %q is not a defined pool", prefix, s.RuntimeTarget.PoolID)
		}
		if s.FallbackPolicy != nil {
			for _, pid := range s.FallbackPolicy.AllowedPoolIDs {
				if r != nil && !r.PoolExists(pid) {
					fail("%s.fallback_policy.allowed_pool_ids: pool %q is not defined", prefix, pid)
				}
			}
		}
		if s.RetryPolicy != nil && (s.RetryPolicy.MaxAttempts < 1 || s.RetryPolicy.MaxAttempts > MaxAttemptsCap) {
			fail("%s.retry_policy.max_attempts must be 1..%d", prefix, MaxAttemptsCap)
		}
		// 依存: W1 は「先に定義された step のみ参照可」= 循環は構文上不可能
		for _, dep := range s.DependsOn {
			if _, ok := seen[dep]; !ok {
				fail("%s.depends_on references %q which is not an earlier step", prefix, dep)
			}
		}
		// input_mapping: W1 は "text" キー必須
		if _, ok := s.InputMapping["text"]; !ok {
			fail("%s.input_mapping.text is required in W1", prefix)
		}
		for key, tpl := range s.InputMapping {
			if key != "text" {
				fail("%s.input_mapping key %q is not supported in W1 (text only)", prefix, key)
				continue
			}
			errs = append(errs, checkRefs(tpl, fmt.Sprintf("%s.input_mapping.%s", prefix, key), inputNames, seen, s.DependsOn)...)
		}
		seen[s.StepID] = i
	}

	// output_mapping: W1 は "text" キー必須、最終出力は全 step を参照可
	if _, ok := d.OutputMapping["text"]; !ok {
		fail("output_mapping.text is required in W1")
	}
	allDeps := make([]string, 0, len(seen))
	for id := range seen {
		allDeps = append(allDeps, id)
	}
	for key, tpl := range d.OutputMapping {
		if key != "text" {
			fail("output_mapping key %q is not supported in W1 (text only)", key)
			continue
		}
		errs = append(errs, checkRefs(tpl, "output_mapping."+key, inputNames, seen, allDeps)...)
	}
	return errs
}

// checkRefs validates template syntax and that every reference is an allowed
// variable: inputs.{declared}, steps.{dependency}.output, run.id。
func checkRefs(tpl, where string, inputs map[string]bool, steps map[string]int, deps []string) []error {
	var errs []error
	if err := CheckTemplateSyntax(tpl); err != nil {
		return []error{fmt.Errorf("%s: %v", where, err)}
	}
	depSet := map[string]bool{}
	for _, d := range deps {
		depSet[d] = true
	}
	for _, ref := range TemplateRefs(tpl) {
		parts := strings.Split(ref, ".")
		switch {
		case len(parts) == 2 && parts[0] == "inputs":
			if !inputs[parts[1]] {
				errs = append(errs, fmt.Errorf("%s: reference to undeclared input %q", where, parts[1]))
			}
		case len(parts) == 3 && parts[0] == "steps" && parts[2] == "output":
			if _, known := steps[parts[1]]; !known {
				errs = append(errs, fmt.Errorf("%s: reference to unknown step %q", where, parts[1]))
			} else if !depSet[parts[1]] {
				// 依存関係外の step output は参照不可(MRCI-001 §7.6)
				errs = append(errs, fmt.Errorf("%s: step %q is not in depends_on", where, parts[1]))
			}
		case ref == "run.id":
		default:
			errs = append(errs, fmt.Errorf("%s: reference %q is not an allowed variable", where, ref))
		}
	}
	return errs
}

// ValidateInputs checks run inputs against the schema and returns them
// (required・型・maxLength・additionalProperties)。
func (d *Definition) ValidateInputs(inputs map[string]string) error {
	if d.InputsSchema == nil {
		return fmt.Errorf("flow has no inputs_schema")
	}
	for _, req := range d.InputsSchema.Required {
		if _, ok := inputs[req]; !ok {
			return fmt.Errorf("missing required input %q", req)
		}
	}
	for name, v := range inputs {
		p, ok := d.InputsSchema.Properties[name]
		if !ok {
			return fmt.Errorf("unexpected input %q", name)
		}
		maxLen := p.MaxLength
		if maxLen <= 0 {
			maxLen = MaxInputLenDefault
		}
		if len(v) > maxLen {
			return fmt.Errorf("input %q exceeds maxLength %d", name, maxLen)
		}
	}
	return nil
}
