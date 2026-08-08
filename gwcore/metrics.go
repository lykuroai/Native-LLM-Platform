package gwcore

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// metrics.go implements the Prometheus text-format endpoint (ADD §17.1 /
// BD §19.3)。外部依存を増やさないため exposition format を直接生成する
// (顧客配布の単一静的バイナリを太らせない)。
//
// label 規約(BD §19.3): endpoint(固定パス)・status・result code・logical
// model(カタログ由来)のみ。user 入力・prompt・key・request_id は載せない。
// runtime_queue_depth は Gateway にキューが無いため公開しない(§8.3 と同じく
// 未実装値を作らない)。

var durationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 300}
var ttftBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30}

type histogram struct {
	buckets []float64
	counts  []int64
	sum     float64
	total   int64
}

func newHistogram(buckets []float64) *histogram {
	return &histogram{buckets: buckets, counts: make([]int64, len(buckets))}
}

func (h *histogram) observe(v float64) {
	h.sum += v
	h.total++
	for i, b := range h.buckets {
		if v <= b {
			h.counts[i]++
		}
	}
}

// Metrics is the in-process metrics registry。全 hook は低頻度 map 更新のみ。
type Metrics struct {
	mu            sync.Mutex
	requests      map[[2]string]int64 // endpoint, status
	durations     map[string]*histogram
	ttft          *histogram
	inputTokens   map[string]int64 // logical model
	outputTokens  map[string]int64
	modelErrors   map[string]int64 // result code(5xx/transport)
	policyDenied  int64
	rateLimited   int64
	authFailures  int64
	activeStreams int64
	configVersion int64
	heartbeats    map[bool]int64
}

func newMetrics() *Metrics {
	return &Metrics{
		requests:     map[[2]string]int64{},
		durations:    map[string]*histogram{},
		ttft:         newHistogram(ttftBuckets),
		inputTokens:  map[string]int64{},
		outputTokens: map[string]int64{},
		modelErrors:  map[string]int64{},
		heartbeats:   map[bool]int64{},
	}
}

// recordRequest is called from auditProxy(推論の全終了点)。
func (m *Metrics) recordRequest(endpoint string, status int, result, model string, inTok, outTok *int64, latency time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[[2]string{endpoint, strconv.Itoa(status)}]++
	h, ok := m.durations[endpoint]
	if !ok {
		h = newHistogram(durationBuckets)
		m.durations[endpoint] = h
	}
	h.observe(latency.Seconds())
	if model != "" {
		if inTok != nil {
			m.inputTokens[model] += *inTok
		}
		if outTok != nil {
			m.outputTokens[model] += *outTok
		}
	}
	switch {
	case result == "policy_denied":
		m.policyDenied++
	case status >= 500:
		m.modelErrors[result]++
	}
}

// recordDenied is called from auditDenied(認証・レート・ポリシー拒否)。
func (m *Metrics) recordDenied(endpoint string, status int, result string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[[2]string{endpoint, strconv.Itoa(status)}]++
	switch result {
	case "authentication_failed":
		m.authFailures++
	case "rate_limit_exceeded":
		m.rateLimited++
	case "policy_denied":
		m.policyDenied++
	}
}

// streamStarted records TTFT and increments active_streams。
func (m *Metrics) streamStarted(ttft time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ttft.observe(ttft.Seconds())
	m.activeStreams++
}

func (m *Metrics) streamEnded() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.activeStreams > 0 {
		m.activeStreams--
	}
}

// SetConfigVersion records the applied signed-config generation。
func (m *Metrics) SetConfigVersion(v int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configVersion = v
}

// RecordHeartbeat counts control-plane heartbeat outcomes。
func (m *Metrics) RecordHeartbeat(ok bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.heartbeats[ok]++
}

// ServeHTTP renders the Prometheus exposition format。
func (m *Metrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder

	b.WriteString("# HELP lykuro_pgw_build_info Gateway build information\n# TYPE lykuro_pgw_build_info gauge\n")
	fmt.Fprintf(&b, "lykuro_pgw_build_info{version=%q} 1\n", Version)

	b.WriteString("# HELP lykuro_pgw_requests_total Requests by endpoint and HTTP status\n# TYPE lykuro_pgw_requests_total counter\n")
	for _, k := range sortedKeys2(m.requests) {
		fmt.Fprintf(&b, "lykuro_pgw_requests_total{endpoint=%q,status=%q} %d\n", k[0], k[1], m.requests[k])
	}

	b.WriteString("# HELP lykuro_pgw_request_duration_seconds Request latency\n# TYPE lykuro_pgw_request_duration_seconds histogram\n")
	for _, ep := range sortedKeys(m.durations) {
		writeHistogram(&b, "lykuro_pgw_request_duration_seconds", fmt.Sprintf("endpoint=%q", ep), m.durations[ep])
	}

	b.WriteString("# HELP lykuro_pgw_first_token_latency_seconds Time to first streamed byte\n# TYPE lykuro_pgw_first_token_latency_seconds histogram\n")
	writeHistogram(&b, "lykuro_pgw_first_token_latency_seconds", "", m.ttft)

	b.WriteString("# HELP lykuro_pgw_input_tokens_total Input tokens by logical model\n# TYPE lykuro_pgw_input_tokens_total counter\n")
	for _, k := range sortedKeys(m.inputTokens) {
		fmt.Fprintf(&b, "lykuro_pgw_input_tokens_total{model=%q} %d\n", k, m.inputTokens[k])
	}
	b.WriteString("# HELP lykuro_pgw_output_tokens_total Output tokens by logical model\n# TYPE lykuro_pgw_output_tokens_total counter\n")
	for _, k := range sortedKeys(m.outputTokens) {
		fmt.Fprintf(&b, "lykuro_pgw_output_tokens_total{model=%q} %d\n", k, m.outputTokens[k])
	}

	b.WriteString("# HELP lykuro_pgw_model_errors_total Inference failures by result code\n# TYPE lykuro_pgw_model_errors_total counter\n")
	for _, k := range sortedKeys(m.modelErrors) {
		fmt.Fprintf(&b, "lykuro_pgw_model_errors_total{code=%q} %d\n", k, m.modelErrors[k])
	}

	fmt.Fprintf(&b, "# HELP lykuro_pgw_policy_denied_total Policy denials\n# TYPE lykuro_pgw_policy_denied_total counter\nlykuro_pgw_policy_denied_total %d\n", m.policyDenied)
	fmt.Fprintf(&b, "# HELP lykuro_pgw_rate_limited_total Rate-limited requests\n# TYPE lykuro_pgw_rate_limited_total counter\nlykuro_pgw_rate_limited_total %d\n", m.rateLimited)
	fmt.Fprintf(&b, "# HELP lykuro_pgw_auth_failures_total Authentication failures\n# TYPE lykuro_pgw_auth_failures_total counter\nlykuro_pgw_auth_failures_total %d\n", m.authFailures)
	fmt.Fprintf(&b, "# HELP lykuro_pgw_active_streams Currently open streaming responses\n# TYPE lykuro_pgw_active_streams gauge\nlykuro_pgw_active_streams %d\n", m.activeStreams)
	fmt.Fprintf(&b, "# HELP lykuro_pgw_config_version Applied signed-config generation\n# TYPE lykuro_pgw_config_version gauge\nlykuro_pgw_config_version %d\n", m.configVersion)
	fmt.Fprintf(&b, "# HELP lykuro_pgw_heartbeat_total Control-plane heartbeats by result\n# TYPE lykuro_pgw_heartbeat_total counter\nlykuro_pgw_heartbeat_total{result=\"success\"} %d\nlykuro_pgw_heartbeat_total{result=\"failure\"} %d\n",
		m.heartbeats[true], m.heartbeats[false])

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func writeHistogram(b *strings.Builder, name, label string, h *histogram) {
	sep := ""
	if label != "" {
		sep = ","
	}
	for i, bound := range h.buckets {
		fmt.Fprintf(b, "%s_bucket{%s%sle=%q} %d\n", name, label, sep, strconv.FormatFloat(bound, 'g', -1, 64), h.counts[i])
	}
	fmt.Fprintf(b, "%s_bucket{%s%sle=\"+Inf\"} %d\n", name, label, sep, h.total)
	if label != "" {
		fmt.Fprintf(b, "%s_sum{%s} %g\n", name, label, h.sum)
		fmt.Fprintf(b, "%s_count{%s} %d\n", name, label, h.total)
	} else {
		fmt.Fprintf(b, "%s_sum %g\n", name, h.sum)
		fmt.Fprintf(b, "%s_count %d\n", name, h.total)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeys2(m map[[2]string]int64) [][2]string {
	out := make([][2]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return out[i][1] < out[j][1]
	})
	return out
}
