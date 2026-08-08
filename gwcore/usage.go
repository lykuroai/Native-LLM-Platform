package gwcore

import (
	"sync"
	"time"
)

// UsageRow is one daily aggregate reported to the control plane
// (BD §6.3 許可リスト項目のみ。本文・キー・IPは含めない)。
type UsageRow struct {
	UsageDate    string `json:"usage_date"` // YYYY-MM-DD (UTC)
	LogicalModel string `json:"logical_model"`
	RequestCount int64  `json:"request_count"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	ErrorCount   int64  `json:"error_count"`
}

type usageKey struct {
	date  string
	model string
}

// usageCounter accumulates in-memory usage deltas between agent uploads.
type usageCounter struct {
	mu   sync.Mutex
	rows map[usageKey]*UsageRow
}

func newUsageCounter() *usageCounter {
	return &usageCounter{rows: map[usageKey]*UsageRow{}}
}

func (u *usageCounter) add(logicalModel string, inTok, outTok *int64, isError bool) {
	if logicalModel == "" {
		return
	}
	key := usageKey{date: time.Now().UTC().Format("2006-01-02"), model: logicalModel}
	u.mu.Lock()
	defer u.mu.Unlock()
	row := u.rows[key]
	if row == nil {
		row = &UsageRow{UsageDate: key.date, LogicalModel: key.model}
		u.rows[key] = row
	}
	row.RequestCount++
	if isError {
		row.ErrorCount++
	}
	if inTok != nil {
		row.InputTokens += *inTok
	}
	if outTok != nil {
		row.OutputTokens += *outTok
	}
}

// Drain returns the accumulated deltas and resets the counter.
// 送信失敗時は Restore で戻す(欠損防止)。
func (s *Server) DrainUsage() []UsageRow {
	s.usage.mu.Lock()
	defer s.usage.mu.Unlock()
	out := make([]UsageRow, 0, len(s.usage.rows))
	for _, r := range s.usage.rows {
		out = append(out, *r)
	}
	s.usage.rows = map[usageKey]*UsageRow{}
	return out
}

// RestoreUsage re-adds rows after a failed upload.
func (s *Server) RestoreUsage(rows []UsageRow) {
	s.usage.mu.Lock()
	defer s.usage.mu.Unlock()
	for _, in := range rows {
		key := usageKey{date: in.UsageDate, model: in.LogicalModel}
		row := s.usage.rows[key]
		if row == nil {
			copied := in
			s.usage.rows[key] = &copied
			continue
		}
		row.RequestCount += in.RequestCount
		row.InputTokens += in.InputTokens
		row.OutputTokens += in.OutputTokens
		row.ErrorCount += in.ErrorCount
	}
}
