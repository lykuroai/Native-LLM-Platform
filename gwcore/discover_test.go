package gwcore

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiscoverHosts(t *testing.T) {
	cases := []struct {
		name    string
		cidr    string
		wantLen int
		wantErr bool
	}{
		{"empty is localhost only", "", 1, false},
		{"/30 expands", "192.168.1.0/30", 4, false},
		{"/24 expands", "10.0.0.0/24", 256, false},
		{"too large is rejected", "10.0.0.0/16", 0, true},
		{"invalid cidr", "nope", 0, true},
		{"ipv6 unsupported", "fd00::/120", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hosts, err := DiscoverHosts(tc.cidr)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, hosts, tc.wantLen)
		})
	}
}

func TestProbeRuntime(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Write([]byte(`{"models":[{"name":"llama3:8b"},{"name":"qwen3:4b"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ollama.Close()

	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"object":"list","data":[{"id":"meta-llama/Llama-3-8B"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer openai.Close()

	// LLM Runtime ではない普通の HTTP サーバー(200 を返すが形が違う)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<html>hello</html>`))
	}))
	defer web.Close()

	client := ollama.Client()
	ctx := context.Background()

	cand, ok := probeRuntime(ctx, client, ollama.URL, "vllm")
	require.True(t, ok)
	require.Equal(t, "ollama", cand.Runtime) // /api/tags 応答が優先され機種確定
	require.Equal(t, []string{"llama3:8b", "qwen3:4b"}, cand.Models)

	cand, ok = probeRuntime(ctx, client, openai.URL, "vllm")
	require.True(t, ok)
	require.Equal(t, "vllm", cand.Runtime) // OpenAI 互換のみ → ポート由来の推定
	require.Equal(t, []string{"meta-llama/Llama-3-8B"}, cand.Models)

	_, ok = probeRuntime(ctx, client, web.URL, "vllm")
	require.False(t, ok) // JSON 形不一致は Runtime と誤認しない

	_, ok = probeRuntime(ctx, client, "http://127.0.0.1:1", "vllm")
	require.False(t, ok) // 到達不能
}

func TestConfiguredEndpoints(t *testing.T) {
	cfg := &Config{
		SchemaVersion: "1",
		Gateway:       GatewaySection{ID: "gw"},
		Models: []ModelDef{{
			LogicalName: "chat", Runtime: "vllm",
			Endpoint: "http://10.0.0.5:8000/", PhysicalModel: "m",
			Fallback: []ModelTarget{{Runtime: "ollama", Endpoint: "http://10.0.0.6:11434", PhysicalModel: "m2"}},
		}},
	}
	require.NoError(t, cfg.Validate())
	eps := ConfiguredEndpoints(cfg)
	require.True(t, eps["http://10.0.0.5:8000"]) // 末尾スラッシュは正規化
	require.True(t, eps["http://10.0.0.6:11434"])
	require.False(t, eps["http://10.0.0.7:8000"])
}

func TestAdminDiscoverAdopt(t *testing.T) {
	env := newAdminEnv(t)

	// 必須フィールド欠落
	rec := env.do(t, http.MethodPost, "/api/discover/adopt", env.token, `{"logical_name":"x"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// 正常取込 → メモリ・ファイル両方へ反映、runtime 未指定は openai_compatible
	rec = env.do(t, http.MethodPost, "/api/discover/adopt", env.token,
		`{"logical_name":"local-llama","endpoint":"http://127.0.0.1:11434/","physical_model":"llama3:8b","runtime":"ollama"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	def := env.srv.config().FindModel("local-llama")
	require.NotNil(t, def)
	require.Equal(t, "http://127.0.0.1:11434", def.Endpoint) // 末尾スラッシュ除去
	require.Equal(t, "ollama", def.Runtime)

	// 重複 logical_name は設定検証で拒否(Fail Closed)
	rec = env.do(t, http.MethodPost, "/api/discover/adopt", env.token,
		`{"logical_name":"local-llama","endpoint":"http://127.0.0.1:8000","physical_model":"m"}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)

	// 不正 runtime 種別も拒否
	rec = env.do(t, http.MethodPost, "/api/discover/adopt", env.token,
		`{"logical_name":"bad","endpoint":"http://127.0.0.1:8000","physical_model":"m","runtime":"magic"}`)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestAdminDiscoverCIDRLimit(t *testing.T) {
	env := newAdminEnv(t)
	rec := env.do(t, http.MethodGet, "/api/discover?cidr=10.0.0.0/8", env.token, "")
	require.Equal(t, http.StatusBadRequest, rec.Code)
}
