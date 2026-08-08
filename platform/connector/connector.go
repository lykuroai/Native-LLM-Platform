// Package connector implements the External Runtime Connector (§7)。
//
// Connector は protocol 変換 component であり推論 Runtime そのものではない:
// 第三者 binary を含めず、承認済み endpoint(設定の allowlist)へのみ接続する。
// SSRF 防止のため、接続先は設定済み base_url のみ — リクエスト由来の URL を
// 一切使用しない(§7.4)。
package connector

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/lykuroai/Native-LLM-Platform/platform/contract"
	"github.com/lykuroai/Native-LLM-Platform/platform/platformcfg"
)

// タイムアウトは CLAUDE.md 外部API方針(gwcore と同値): 接続10秒・全体15分。
const (
	connectTimeout = 10 * time.Second
	overallTimeout = 15 * time.Minute
	healthTimeout  = 2 * time.Second
)

// circuit breaker: 連続失敗で open、cooldown 後 half-open(§7.4)。
const (
	breakerThreshold = 3
	breakerCooldown  = 30 * time.Second
)

// Result is a normalized upstream response。
type Result struct {
	StatusCode  int
	ContentType string
	Body        []byte
	Usage       contract.Usage
}

// StreamOutcome summarizes a passthrough stream。
type StreamOutcome struct {
	Usage      contract.Usage
	ClientGone bool
}

// OpenAICompatible connects to one customer-managed OpenAI-compatible runtime。
type OpenAICompatible struct {
	ep     platformcfg.RuntimeEndpoint
	client *http.Client

	mu       sync.Mutex
	failures int
	openedAt time.Time
}

// New builds a connector for a validated endpoint definition。
func New(ep platformcfg.RuntimeEndpoint) *OpenAICompatible {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: connectTimeout}).DialContext,
		ResponseHeaderTimeout: overallTimeout,
		MaxIdleConnsPerHost:   16,
	}
	return &OpenAICompatible{
		ep:     ep,
		client: &http.Client{Transport: transport, Timeout: overallTimeout},
	}
}

// EndpointID returns the non-secret endpoint identifier。
func (c *OpenAICompatible) EndpointID() string { return c.ep.ID }

// --- circuit breaker ---

func (c *OpenAICompatible) breakerOpen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failures < breakerThreshold {
		return false
	}
	if time.Since(c.openedAt) > breakerCooldown {
		// half-open: 1回だけ試行を許す
		c.failures = breakerThreshold - 1
		return false
	}
	return true
}

func (c *OpenAICompatible) recordResult(ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ok {
		c.failures = 0
		return
	}
	c.failures++
	if c.failures == breakerThreshold {
		c.openedAt = time.Now()
	}
}

// credential loads the service-account token from the referenced file。
// 原文は保持しない(毎回読取 — rotation を再起動なしで反映、§7.4)。
func (c *OpenAICompatible) credential() (string, error) {
	if c.ep.CredentialFile == "" {
		return "", nil
	}
	raw, err := os.ReadFile(c.ep.CredentialFile)
	if err != nil {
		return "", fmt.Errorf("read connector credential: %w", err)
	}
	return strings.TrimSpace(string(raw)), nil
}

func (c *OpenAICompatible) buildRequest(ctx context.Context, upstreamPath string, body []byte, accept, reqID string) (*http.Request, *contract.Error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ep.BaseURL+upstreamPath, bytes.NewReader(body))
	if err != nil {
		return nil, &contract.Error{Code: contract.ErrInternalContract, Message: "failed to build upstream request"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", reqID)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	cred, err := c.credential()
	if err != nil {
		return nil, &contract.Error{Code: contract.ErrModelNotAvailable, Message: "runtime credential unavailable"}
	}
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	return req, nil
}

// classifyTransport maps a transport error to a frozen platform code。
// タイムアウトは推論開始済みの可能性があり failover 不可(呼出側判断用に
// code で区別する)。
func classifyTransport(err error) *contract.Error {
	if isTimeout(err) {
		return &contract.Error{Code: contract.ErrInferenceTimeout, Message: "inference did not complete in time"}
	}
	return &contract.Error{Code: contract.ErrModelNotAvailable, Message: "runtime is unreachable", RetryAfter: 5}
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Do executes a non-streaming passthrough request。
func (c *OpenAICompatible) Do(ctx context.Context, upstreamPath string, body []byte, accept, reqID string) (*Result, *contract.Error) {
	if c.breakerOpen() {
		return nil, &contract.Error{Code: contract.ErrModelNotAvailable, Message: "runtime circuit is open", RetryAfter: int(breakerCooldown.Seconds())}
	}
	req, cerr := c.buildRequest(ctx, upstreamPath, body, accept, reqID)
	if cerr != nil {
		return nil, cerr
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.recordResult(false)
		return nil, classifyTransport(err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		c.recordResult(false)
		return nil, &contract.Error{Code: contract.ErrInternalContract, Message: "upstream body read failed"}
	}
	c.recordResult(resp.StatusCode < 500)
	in, out := SniffUsage(respBody)
	return &Result{
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        respBody,
		Usage:       contract.Usage{InputTokens: in, OutputTokens: out},
	}, nil
}

// DoStream executes a streaming passthrough request, writing raw SSE to w。
// 上流の生バイトを透過し usage は覗き見のみ(§0.3)。
func (c *OpenAICompatible) DoStream(ctx context.Context, upstreamPath string, body []byte, accept, reqID string, w contract.StreamWriter) (*StreamOutcome, *contract.Error) {
	if c.breakerOpen() {
		return nil, &contract.Error{Code: contract.ErrModelNotAvailable, Message: "runtime circuit is open", RetryAfter: int(breakerCooldown.Seconds())}
	}
	req, cerr := c.buildRequest(ctx, upstreamPath, body, accept, reqID)
	if cerr != nil {
		return nil, cerr
	}
	resp, err := c.client.Do(req)
	if err != nil {
		c.recordResult(false)
		return nil, classifyTransport(err)
	}
	defer resp.Body.Close()
	c.recordResult(resp.StatusCode < 500)

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		// 上流がSSEを返さなかった(エラー等)。stream 開始前なので通常応答へ。
		respBody, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			return nil, &contract.Error{Code: contract.ErrInternalContract, Message: "upstream body read failed"}
		}
		w.Start(resp.StatusCode, ct)
		w.Chunk(respBody)
		w.Flush()
		in, out := SniffUsage(respBody)
		return &StreamOutcome{Usage: contract.Usage{InputTokens: in, OutputTokens: out}}, nil
	}

	w.Start(resp.StatusCode, ct)
	outcome := &StreamOutcome{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !w.Chunk(append(line, '\n')) {
			outcome.ClientGone = true
			return outcome, nil
		}
		if len(line) == 0 {
			w.Flush() // イベント境界で送出
		}
		if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok && bytes.Contains(data, []byte(`"usage"`)) {
			if in, out := SniffUsage(data); in != nil || out != nil {
				outcome.Usage = contract.Usage{InputTokens: in, OutputTokens: out}
			}
		}
	}
	w.Flush()
	return outcome, nil
}

// Health probes the runtime (management mode を問わず read-only、§6.5)。
func (c *OpenAICompatible) Health(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	path := c.ep.HealthPath
	if path == "" {
		path = "/v1/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ep.BaseURL+path, nil)
	if err != nil {
		return false
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500 // 認証エラーでも到達はしている
}

// ListModels queries the runtime model inventory(observe_only 以上で許可)。
func (c *OpenAICompatible) ListModels(ctx context.Context) ([]string, error) {
	if c.ep.ManagementMode == "" {
		return nil, fmt.Errorf("management mode does not permit model listing")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ep.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	if cred, cerr := c.credential(); cerr == nil && cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// SniffUsage extracts token counts from an OpenAI-compatible payload
// (gwcore proxy.go と同じ対応範囲: Chat Completions / Responses API)。
func SniffUsage(data []byte) (inTok, outTok *int64) {
	var probe struct {
		Usage    *usageProbe `json:"usage"`
		Response *struct {
			Usage *usageProbe `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, nil
	}
	u := probe.Usage
	if u == nil && probe.Response != nil {
		u = probe.Response.Usage
	}
	if u == nil {
		return nil, nil
	}
	in := u.PromptTokens + u.InputTokens
	out := u.CompletionTokens + u.OutputTokens
	return &in, &out
}

type usageProbe struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
}
