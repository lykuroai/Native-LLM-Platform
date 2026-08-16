package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeResolver struct {
	models map[string]bool
	pools  map[string]bool
}

func (f fakeResolver) ModelExists(n string) bool { return f.models[n] }
func (f fakeResolver) PoolExists(p string) bool  { return f.pools[p] }

var testResolver = fakeResolver{
	models: map[string]bool{"model-a": true, "model-b": true},
	pools:  map[string]bool{"pool-1": true, "pool-safe": true},
}

// validDef is the MRCI-001 §7.3 追加質問型 Flow の縮退版。
func validDef(t *testing.T, mutate func(m map[string]any)) json.RawMessage {
	t.Helper()
	base := `{
	  "name": "Two Runtime Follow-up Chain",
	  "alias": "two-runtime-follow-up",
	  "execution_mode": "SEQUENTIAL",
	  "inputs_schema": {
	    "type": "object",
	    "required": ["question1", "question2"],
	    "properties": {
	      "question1": {"type": "string", "maxLength": 1000},
	      "question2": {"type": "string"}
	    }
	  },
	  "steps": [
	    {
	      "step_id": "runtime_a",
	      "type": "LLM",
	      "runtime_target": {"logical_model": "model-a"},
	      "system_prompt": "answer",
	      "input_mapping": {"text": "{{inputs.question1}}"},
	      "retry_policy": {"max_attempts": 2, "backoff_ms": 1}
	    },
	    {
	      "step_id": "runtime_b",
	      "type": "LLM",
	      "depends_on": ["runtime_a"],
	      "runtime_target": {"logical_model": "model-b", "pool_id": "pool-1"},
	      "input_mapping": {"text": "{{inputs.question1}}\n{{steps.runtime_a.output}}\n{{inputs.question2}}"},
	      "fallback_policy": {"allowed_pool_ids": ["pool-safe"]}
	    }
	  ],
	  "output_mapping": {"text": "{{steps.runtime_b.output}}"}
	}`
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(base), &m))
	if mutate != nil {
		mutate(m)
	}
	raw, err := json.Marshal(m)
	require.NoError(t, err)
	return raw
}

func step(m map[string]any, i int) map[string]any {
	return m["steps"].([]any)[i].(map[string]any)
}

func TestParseDefinitionRejectsUnknownFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"top-level unknown", func(m map[string]any) { m["network_zone"] = "z1" }},
		{"step output_schema (W2)", func(m map[string]any) {
			step(m, 0)["output_schema"] = map[string]any{"type": "object"}
		}},
		{"step condition (W2)", func(m map[string]any) { step(m, 0)["condition"] = "x" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDefinition(validDef(t, tc.mutate))
			require.Error(t, err)
		})
	}
}

func TestValidateOK(t *testing.T) {
	def, err := ParseDefinition(validDef(t, nil))
	require.NoError(t, err)
	require.Empty(t, def.Validate(testResolver))
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(m map[string]any)
		wantSub string
	}{
		{"bad execution mode", func(m map[string]any) { m["execution_mode"] = "PARALLEL" }, "SEQUENTIAL"},
		{"conversation mode", func(m map[string]any) { m["conversation_mode"] = "CONVERSATION" }, "not supported"},
		{"bad alias", func(m map[string]any) { m["alias"] = "Bad Alias!" }, "alias"},
		{"duplicate step id", func(m map[string]any) { step(m, 1)["step_id"] = "runtime_a" }, "not unique"},
		{"bad step id", func(m map[string]any) { step(m, 0)["step_id"] = "Runtime-A" }, "step_id"},
		{"unknown model", func(m map[string]any) {
			step(m, 0)["runtime_target"] = map[string]any{"logical_model": "nope"}
		}, "not a known approved model"},
		{"unknown pool", func(m map[string]any) {
			step(m, 1)["runtime_target"] = map[string]any{"logical_model": "model-b", "pool_id": "nope"}
		}, "not a defined pool"},
		{"unknown fallback pool", func(m map[string]any) {
			step(m, 1)["fallback_policy"] = map[string]any{"allowed_pool_ids": []any{"nope"}}
		}, "not defined"},
		{"forward dependency", func(m map[string]any) {
			step(m, 0)["depends_on"] = []any{"runtime_b"}
		}, "not an earlier step"},
		{"ref outside deps", func(m map[string]any) {
			step(m, 1)["depends_on"] = []any{}
			step(m, 1)["input_mapping"] = map[string]any{"text": "{{steps.runtime_a.output}}"}
		}, "not in depends_on"},
		{"undeclared input", func(m map[string]any) {
			step(m, 0)["input_mapping"] = map[string]any{"text": "{{inputs.nope}}"}
		}, "undeclared input"},
		{"forbidden variable", func(m map[string]any) {
			step(m, 0)["input_mapping"] = map[string]any{"text": "{{env.SECRET}}"}
		}, "not an allowed variable"},
		{"unclosed template", func(m map[string]any) {
			step(m, 0)["input_mapping"] = map[string]any{"text": "{{inputs.question1"}
		}, "malformed"},
		{"missing text mapping", func(m map[string]any) {
			step(m, 0)["input_mapping"] = map[string]any{}
		}, "input_mapping.text is required"},
		{"missing output mapping", func(m map[string]any) { m["output_mapping"] = map[string]any{} }, "output_mapping.text"},
		{"retry too high", func(m map[string]any) {
			step(m, 0)["retry_policy"] = map[string]any{"max_attempts": 99}
		}, "max_attempts"},
		{"on_error CONTINUE (W2)", func(m map[string]any) { step(m, 0)["on_error"] = "CONTINUE" }, "on_error"},
		{"non-string input prop", func(m map[string]any) {
			m["inputs_schema"].(map[string]any)["properties"].(map[string]any)["question1"] = map[string]any{"type": "integer"}
		}, "must be \"string\""},
		{"parallelism", func(m map[string]any) { m["limits"] = map[string]any{"max_parallelism": 2} }, "max_parallelism"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def, err := ParseDefinition(validDef(t, tc.mutate))
			require.NoError(t, err)
			errs := def.Validate(testResolver)
			require.NotEmpty(t, errs)
			var all []string
			for _, e := range errs {
				all = append(all, e.Error())
			}
			require.Contains(t, strings.Join(all, "; "), tc.wantSub)
		})
	}
}

func TestValidateStepCountLimit(t *testing.T) {
	def, err := ParseDefinition(validDef(t, func(m map[string]any) {
		steps := m["steps"].([]any)
		for i := 0; i < MaxStepsW1; i++ {
			s := map[string]any{
				"step_id": fmt.Sprintf("extra_%d", i), "type": "LLM",
				"runtime_target": map[string]any{"logical_model": "model-a"},
				"input_mapping":  map[string]any{"text": "{{inputs.question1}}"},
			}
			steps = append(steps, s)
		}
		m["steps"] = steps
	}))
	require.NoError(t, err)
	errs := def.Validate(testResolver)
	require.NotEmpty(t, errs)
}

func TestValidateInputs(t *testing.T) {
	def, err := ParseDefinition(validDef(t, nil))
	require.NoError(t, err)

	require.NoError(t, def.ValidateInputs(map[string]string{"question1": "a", "question2": "b"}))
	require.Error(t, def.ValidateInputs(map[string]string{"question1": "a"}), "missing required")
	require.Error(t, def.ValidateInputs(map[string]string{"question1": "a", "question2": "b", "extra": "x"}))
	require.Error(t, def.ValidateInputs(map[string]string{"question1": strings.Repeat("x", 1001), "question2": "b"}))
}
