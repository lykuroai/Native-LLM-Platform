package gwcore

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const validYAML = `
schema_version: "1"
gateway:
  id: gw-test
  listen: ":0"
  allowed_data_classes: [public, internal, confidential]
auth:
  virtual_keys:
    - id: vk-legal
      name: legal
      key_hash: aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      allowed_models: [company-llm-safe]
      rpm_limit: 60
models:
  - logical_name: company-llm-safe
    runtime: vllm
    endpoint: "http://localhost:8000"
    physical_model: qwen3-32b
  - logical_name: company-embed
    runtime: openai_compatible
    endpoint: "http://localhost:8001"
    physical_model: bge-m3
`

func TestParseConfigValid(t *testing.T) {
	c, err := ParseConfig([]byte(validYAML))
	require.NoError(t, err)
	assert.True(t, c.StrictLocal(), "strict local must default to ON")
	assert.Equal(t, int64(10<<20), c.Gateway.MaxRequestBytes)
	assert.NotNil(t, c.FindModel("company-llm-safe"))
	assert.Nil(t, c.FindModel("unknown"))
	assert.True(t, c.DataClassAllowed("confidential"))
	assert.False(t, c.DataClassAllowed("restricted"))
}

func TestParseConfigRejects(t *testing.T) {
	mutations := map[string]func(string) string{
		"bad schema": func(s string) string {
			return strings.Replace(s, `schema_version: "1"`, `schema_version: "2"`, 1)
		},
		"missing gateway id": func(s string) string {
			return strings.Replace(s, "id: gw-test", "id: \"\"", 1)
		},
		"bad runtime": func(s string) string {
			return strings.Replace(s, "runtime: vllm", "runtime: triton", 1)
		},
		"bad endpoint": func(s string) string {
			return strings.Replace(s, `endpoint: "http://localhost:8000"`, `endpoint: "not a url"`, 1)
		},
		"bad key hash": func(s string) string {
			return strings.Replace(s, "key_hash: aaaa", "key_hash: xyz", 1)
		},
		"unknown allowed model": func(s string) string {
			return strings.Replace(s, "allowed_models: [company-llm-safe]", "allowed_models: [ghost]", 1)
		},
		"duplicate logical name": func(s string) string {
			return strings.Replace(s, "logical_name: company-embed", "logical_name: company-llm-safe", 1)
		},
		"bad data class": func(s string) string {
			return strings.Replace(s, "[public, internal, confidential]", "[public, secret]", 1)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(mutate(validYAML)))
			assert.Error(t, err)
		})
	}
	t.Run("no models is valid (empty catalog serves 404)", func(t *testing.T) {
		yaml := `
schema_version: "1"
gateway:
  id: gw-x
`
		cfg, err := ParseConfig([]byte(yaml))
		assert.NoError(t, err)
		assert.Empty(t, cfg.Models)
	})
}

func TestGenerateVirtualKey(t *testing.T) {
	plain, hash, err := GenerateVirtualKey()
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(plain, VirtualKeyPrefix))
	assert.Equal(t, HashKey(plain), hash)
	assert.Len(t, hash, 64)

	plain2, _, err := GenerateVirtualKey()
	require.NoError(t, err)
	assert.NotEqual(t, plain, plain2)
}

func TestRateLimiter(t *testing.T) {
	rl := newRateLimiter()
	for i := 0; i < 3; i++ {
		assert.True(t, rl.allow("k", 3), "request %d within limit", i)
	}
	assert.False(t, rl.allow("k", 3), "4th request must be limited")
	// 別キーは独立
	assert.True(t, rl.allow("other", 3))
}
