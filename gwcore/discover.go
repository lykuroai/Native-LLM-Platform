package gwcore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Runtime 自動検出(discovery)。ローカルホスト、または管理者が明示指定した
// CIDR 範囲の既知ポートを HTTP プローブし、稼働中の LLM Runtime を候補として
// 列挙する。ここでは**発見するだけ**で接続先には決して自動昇格しない —
// 候補は管理画面/CLI で提示し、管理者の取込操作(承認)を経て初めて
// gateway.yaml の models に入る(Fail Closed。connector の anti-SSRF 方針と
// 同じく、リクエスト由来の URL へ勝手に推論を送る経路を作らない)。

// discoverPorts are well-known local runtime ports(検出対象)。
var discoverPorts = []struct {
	Port    int
	Runtime string // /v1/models のみ応答した場合の推定 runtime
}{
	{11434, "ollama"},
	{8000, "vllm"},
	{8080, "tgi"},
	{3000, "tgi"},
	{1234, "openai_compatible"}, // LM Studio 等
}

const (
	discoverProbeTimeout = 800 * time.Millisecond
	discoverConcurrency  = 64
	// discoverMaxHosts caps CIDR expansion(/22 相当。広域スキャンは
	// IDS 検知・管理外ホスト接触の運用リスクがあるため拒否する)。
	discoverMaxHosts = 1024
)

// RuntimeCandidate is one discovered runtime endpoint.
type RuntimeCandidate struct {
	Endpoint string   `json:"endpoint"`
	Runtime  string   `json:"runtime"`
	Models   []string `json:"models"`
	// Configured is true when the endpoint already appears in the config
	// (models / fallback / platform.runtime_endpoints)。
	Configured bool `json:"configured"`
}

// DiscoverHosts expands the scan target list。cidr 空 = ローカルホストのみ。
func DiscoverHosts(cidr string) ([]string, error) {
	if cidr == "" {
		return []string{"127.0.0.1"}, nil
	}
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid cidr %q: %w", cidr, err)
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		return nil, fmt.Errorf("only IPv4 CIDR is supported")
	}
	if n := 1 << (bits - ones); n > discoverMaxHosts {
		return nil, fmt.Errorf("cidr %q is too large (max /22 = %d hosts)", cidr, discoverMaxHosts)
	}
	var hosts []string
	for ip := ipnet.IP.Mask(ipnet.Mask); ipnet.Contains(ip); ip = nextIP(ip) {
		hosts = append(hosts, ip.String())
	}
	return hosts, nil
}

func nextIP(ip net.IP) net.IP {
	out := make(net.IP, len(ip))
	copy(out, ip)
	for i := len(out) - 1; i >= 0; i-- {
		out[i]++
		if out[i] != 0 {
			break
		}
	}
	return out
}

// ConfiguredEndpoints collects normalized endpoint URLs already in the config。
func ConfiguredEndpoints(cfg *Config) map[string]bool {
	out := map[string]bool{}
	add := func(ep string) {
		if ep != "" {
			out[strings.TrimRight(ep, "/")] = true
		}
	}
	for _, m := range cfg.Models {
		for _, t := range m.Targets() {
			add(t.Endpoint)
		}
	}
	if cfg.Platform != nil {
		for _, ep := range cfg.Platform.RuntimeEndpoints {
			add(ep.BaseURL)
		}
	}
	return out
}

// DiscoverRuntimes probes hosts×既知ポートを並列に走査する(read-only)。
func DiscoverRuntimes(ctx context.Context, hosts []string, configured map[string]bool) []RuntimeCandidate {
	client := &http.Client{Timeout: discoverProbeTimeout}
	sem := make(chan struct{}, discoverConcurrency)
	var (
		mu  sync.Mutex
		out []RuntimeCandidate
		wg  sync.WaitGroup
	)
	for _, host := range hosts {
		for _, p := range discoverPorts {
			base := fmt.Sprintf("http://%s", net.JoinHostPort(host, fmt.Sprint(p.Port)))
			wg.Add(1)
			go func(base, guess string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				cand, ok := probeRuntime(ctx, client, base, guess)
				if !ok {
					return
				}
				cand.Configured = configured[strings.TrimRight(cand.Endpoint, "/")]
				mu.Lock()
				out = append(out, cand)
				mu.Unlock()
			}(base, p.Runtime)
		}
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Endpoint < out[j].Endpoint })
	return out
}

// probeRuntime identifies a runtime at base。Ollama ネイティブ API を先に
// 試し(機種確定+モデル一覧が取れる)、だめなら OpenAI 互換で判定する。
func probeRuntime(ctx context.Context, client *http.Client, base, guess string) (RuntimeCandidate, bool) {
	if models, ok := probeJSON(ctx, client, base+"/api/tags", func(raw []byte) []string {
		var v struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if json.Unmarshal(raw, &v) != nil || v.Models == nil {
			return nil
		}
		names := make([]string, 0, len(v.Models))
		for _, m := range v.Models {
			names = append(names, m.Name)
		}
		return names
	}); ok {
		return RuntimeCandidate{Endpoint: base, Runtime: "ollama", Models: models}, true
	}
	if models, ok := probeJSON(ctx, client, base+"/v1/models", func(raw []byte) []string {
		var v struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &v) != nil || v.Data == nil {
			return nil
		}
		names := make([]string, 0, len(v.Data))
		for _, m := range v.Data {
			names = append(names, m.ID)
		}
		return names
	}); ok {
		return RuntimeCandidate{Endpoint: base, Runtime: guess, Models: models}, true
	}
	return RuntimeCandidate{}, false
}

// probeJSON GETs url and extracts model names。nil 抽出結果は不一致扱い
// (単なる HTTP サーバーを Runtime と誤認しない)。
func probeJSON(ctx context.Context, client *http.Client, url string, extract func([]byte) []string) ([]string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, false
	}
	models := extract(raw)
	if models == nil {
		return nil, false
	}
	return models, true
}
