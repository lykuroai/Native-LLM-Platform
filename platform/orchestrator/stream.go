package orchestrator

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"

	"github.com/lykuroai/Native-LLM-Platform/platform/contract"
)

// accumulatingWriter wraps the gateway StreamWriter to (1) track whether the
// SSE response has started(開始後は failover 不可)and (2) optionally
// accumulate assistant text deltas for Conversation Memory(本文は顧客環境
// 内に留まる。透過出力自体は改変しない)。
type accumulatingWriter struct {
	inner      contract.StreamWriter
	accumulate bool

	mu      sync.Mutex
	started bool
	buf     strings.Builder
}

func newAccumulatingWriter(inner contract.StreamWriter, accumulate bool) *accumulatingWriter {
	return &accumulatingWriter{inner: inner, accumulate: accumulate}
}

func (a *accumulatingWriter) Start(statusCode int, contentType string) {
	a.mu.Lock()
	a.started = true
	a.mu.Unlock()
	a.inner.Start(statusCode, contentType)
}

func (a *accumulatingWriter) Chunk(p []byte) bool {
	if a.accumulate {
		if data, ok := bytes.CutPrefix(bytes.TrimSuffix(p, []byte("\n")), []byte("data: ")); ok {
			a.sniffDelta(data)
		}
	}
	return a.inner.Chunk(p)
}

func (a *accumulatingWriter) Flush() { a.inner.Flush() }

func (a *accumulatingWriter) Started() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.started
}

func (a *accumulatingWriter) content() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.buf.String()
}

// sniffDelta extracts choices[0].delta.content from an OpenAI chunk。
func (a *accumulatingWriter) sniffDelta(data []byte) {
	var probe struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &probe) != nil || len(probe.Choices) == 0 {
		return
	}
	if c := probe.Choices[0].Delta.Content; c != "" {
		a.mu.Lock()
		a.buf.WriteString(c)
		a.mu.Unlock()
	}
}
