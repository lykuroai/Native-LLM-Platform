package gwcore

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// AuditRecord is one no-content audit line (BD §19.2)。
// prompt・response 本文、鍵原文は絶対に含めない(Zero-Retention)。
type AuditRecord struct {
	Timestamp     time.Time `json:"ts"`
	RequestID     string    `json:"request_id"`
	GatewayID     string    `json:"gateway_id"`
	VirtualKeyID  string    `json:"virtual_key_id,omitempty"`
	Endpoint      string    `json:"endpoint"`
	LogicalModel  string    `json:"logical_model,omitempty"`
	PhysicalModel string    `json:"physical_model,omitempty"`
	DataClass     string    `json:"data_class,omitempty"`
	Routing       string    `json:"routing_decision"`
	Status        int       `json:"status"`
	Result        string    `json:"result"`
	InputTokens   *int64    `json:"input_tokens,omitempty"`
	OutputTokens  *int64    `json:"output_tokens,omitempty"`
	LatencyMS     int64     `json:"latency_ms"`
	ContentLogged bool      `json:"content_logged"` // 常に false
}

// AuditLogger appends JSONL records to a local file (BD §14.4: 顧客監査は
// 顧客環境内に保持、Control Plane へは送らない — 送信は Phase 5 Agent の
// メタデータ許可リストのみ)。
type AuditLogger struct {
	mu sync.Mutex
	w  io.Writer
	c  io.Closer
}

// NewAuditLogger opens the JSONL destination. path 空 = stdout。
func NewAuditLogger(path string) (*AuditLogger, error) {
	if path == "" {
		return &AuditLogger{w: os.Stdout}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit log: %w", err)
	}
	return &AuditLogger{w: f, c: f}, nil
}

// Record appends one line (best-effort呼出側でエラー処理)。
func (l *AuditLogger) Record(r *AuditRecord) error {
	r.ContentLogged = false
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal audit record: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.w.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write audit record: %w", err)
	}
	return nil
}

func (l *AuditLogger) Close() error {
	if l.c != nil {
		return l.c.Close()
	}
	return nil
}
