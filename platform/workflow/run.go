// Workflow Run / Step / Event 管理(MRCI-001 §10・MRCI-002 §6)。
//
// Run・Step・Attempt・Route Decision のメタデータはファイルへ永続化するが、
// Step Input/Output 本文はメモリにのみ保持し、ディスク・Event・監査へ
// 書かない(Zero-Retention、MRCI-002 §6.1)。
package workflow

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Run statuses (MRCI-001 §10.2 縮退版)。
const (
	RunQueued     = "QUEUED"
	RunRunning    = "RUNNING"
	RunCancelling = "CANCELLING"
	RunCompleted  = "COMPLETED"
	RunFailed     = "FAILED"
	RunCancelled  = "CANCELLED"
	RunTimedOut   = "TIMED_OUT"
)

// Step statuses (§10.3 縮退版)。
const (
	StepPending   = "PENDING"
	StepRunning   = "RUNNING"
	StepRetrying  = "RETRYING"
	StepCompleted = "COMPLETED"
	StepFailed    = "FAILED"
	StepSkipped   = "SKIPPED"
	StepCancelled = "CANCELLED"
	StepTimedOut  = "TIMED_OUT"
)

// EventSchemaVersion is stamped on every event payload (§10.5)。
const EventSchemaVersion = 1

// Event is one run event。Data に本文・Secret を入れない。
type Event struct {
	Seq           int64          `json:"seq"`
	Type          string         `json:"type"`
	SchemaVersion int            `json:"schema_version"`
	Data          map[string]any `json:"data,omitempty"`
	At            time.Time      `json:"at"`
}

// RouteDecision is the content-free routing record (§9.2 縮退版)。
type RouteDecision struct {
	Attempt      int    `json:"attempt"`
	LogicalModel string `json:"logical_model"`
	PoolID       string `json:"pool_id,omitempty"`
	DeploymentID string `json:"deployment_id,omitempty"` // debug 用(データプレーン応答には返さない)
	RouteType    string `json:"route_type,omitempty"`
	FallbackUsed bool   `json:"fallback_used"`
	DurationMS   int64  `json:"duration_ms"`
	ErrorCode    string `json:"error_code,omitempty"`
}

// StepState is the persisted per-step record(本文なし)。
type StepState struct {
	StepID       string          `json:"step_id"`
	Status       string          `json:"status"`
	Attempts     int             `json:"attempts"`
	Decisions    []RouteDecision `json:"route_decisions,omitempty"`
	InputTokens  int64           `json:"input_tokens"`
	OutputTokens int64           `json:"output_tokens"`
	ErrorCode    string          `json:"error_code,omitempty"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	CompletedAt  *time.Time      `json:"completed_at,omitempty"`
}

// Run is one workflow execution。exported フィールドは永続化対象。
type Run struct {
	ID           string      `json:"run_id"`
	FlowID       string      `json:"flow_id"`
	Alias        string      `json:"alias"`
	FlowVersion  int         `json:"flow_version"`
	FlowChecksum string      `json:"flow_checksum"`
	Status       string      `json:"status"`
	ResponseMode string      `json:"response_mode"`
	DataClass    string      `json:"data_class,omitempty"`
	KeyID        string      `json:"virtual_key_id,omitempty"`
	RequestID    string      `json:"request_id"`
	Steps        []StepState `json:"steps"`
	InputTokens  int64       `json:"input_tokens"`
	OutputTokens int64       `json:"output_tokens"`
	ErrorCode    string      `json:"error_code,omitempty"`
	Warnings     []string    `json:"warnings,omitempty"`
	CreatedAt    time.Time   `json:"created_at"`
	StartedAt    *time.Time  `json:"started_at,omitempty"`
	CompletedAt  *time.Time  `json:"completed_at,omitempty"`

	// メモリ内のみ(本文・実行制御 — 永続化しない)
	output   string
	seq      int64
	events   []Event
	subs     map[int]chan Event
	nextSub  int
	done     chan struct{}
	cancelFn func()
	curReqID string
}

var ErrRunNotFound = fmt.Errorf("workflow: run not found")

// RunStore keeps runs in memory and persists content-free metadata。
type RunStore struct {
	dir string

	mu   sync.Mutex
	runs map[string]*Run
	idem map[string]idemEntry // hash -> run
}

type idemEntry struct {
	runID   string
	payload string // payload checksum(同一 Key・異 payload の検出)
	expires time.Time
}

const idemTTL = 24 * time.Hour

func newRunID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "wrun_" + hex.EncodeToString(b)
}

// OpenRunStore loads persisted run metadata。再起動時、未完了 Run は
// FAILED(interrupted)へ確定する — 二重実行ゼロを優先(MRCI-002 §10-1)。
func OpenRunStore(dir string) (*RunStore, error) {
	runsDir := filepath.Join(dir, "runs")
	if err := os.MkdirAll(runsDir, 0o700); err != nil {
		return nil, err
	}
	s := &RunStore{dir: dir, runs: map[string]*Run{}, idem: map[string]idemEntry{}}
	days, err := os.ReadDir(runsDir)
	if err != nil {
		return nil, err
	}
	for _, day := range days {
		if !day.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(runsDir, day.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".json") || strings.HasSuffix(f.Name(), ".events.jsonl") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(runsDir, day.Name(), f.Name()))
			if err != nil {
				continue
			}
			var r Run
			if json.Unmarshal(raw, &r) != nil || r.ID == "" {
				continue
			}
			switch r.Status {
			case RunQueued, RunRunning, RunCancelling:
				r.Status = RunFailed
				r.ErrorCode = "interrupted"
				now := time.Now().UTC()
				r.CompletedAt = &now
				for i := range r.Steps {
					if r.Steps[i].Status == StepRunning || r.Steps[i].Status == StepRetrying || r.Steps[i].Status == StepPending {
						r.Steps[i].Status = StepCancelled
					}
				}
				_ = s.persistLocked(&r)
			}
			s.runs[r.ID] = &r
		}
	}
	return s, nil
}

func (s *RunStore) runPath(r *Run) string {
	return filepath.Join(s.dir, "runs", r.CreatedAt.UTC().Format("20060102"), r.ID+".json")
}

func (s *RunStore) eventsPath(r *Run) string {
	return filepath.Join(s.dir, "runs", r.CreatedAt.UTC().Format("20060102"), r.ID+".events.jsonl")
}

func (s *RunStore) persistLocked(r *Run) error {
	path := s.runPath(r)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return atomicWrite(path, append(raw, '\n'))
}

// Create registers a new run(QUEUED)。
func (s *RunStore) Create(r *Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.ID = newRunID()
	r.Status = RunQueued
	r.CreatedAt = time.Now().UTC()
	r.subs = map[int]chan Event{}
	r.done = make(chan struct{})
	s.runs[r.ID] = r
	return s.persistLocked(r)
}

// Get returns a run by ID。
func (s *RunStore) Get(runID string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok {
		return nil, ErrRunNotFound
	}
	return r, nil
}

// List returns run snapshots sorted by created_at desc(最大 limit 件)。
func (s *RunStore) List(limit int) []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Run, 0, len(s.runs))
	for _, r := range s.runs {
		out = append(out, r.snapshotLocked())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// snapshotLocked copies persisted fields(メモリ専用フィールドは含めない)。
func (r *Run) snapshotLocked() Run {
	cp := *r
	cp.Steps = append([]StepState(nil), r.Steps...)
	cp.subs = nil
	cp.events = nil
	cp.done = nil
	cp.cancelFn = nil
	return cp
}

// Snapshot returns a copy safe for serialization。
func (s *RunStore) Snapshot(runID string) (*Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok {
		return nil, ErrRunNotFound
	}
	cp := r.snapshotLocked()
	return &cp, nil
}

// Output returns the in-memory final output(完了後のみ)。
func (s *RunStore) Output(runID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok || r.Status != RunCompleted {
		return "", false
	}
	return r.output, true
}

// Emit appends an event, persists it, and fans out to subscribers。
// Event Data に本文・Secret を入れないこと(§10.5)。
func (s *RunStore) Emit(r *Run, typ string, data map[string]any) {
	s.mu.Lock()
	r.seq++
	ev := Event{Seq: r.seq, Type: typ, SchemaVersion: EventSchemaVersion, Data: data,
		At: time.Now().UTC()}
	r.events = append(r.events, ev)
	if raw, err := json.Marshal(ev); err == nil {
		f, err := os.OpenFile(s.eventsPath(r), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.Write(append(raw, '\n'))
			_ = f.Close()
		}
	}
	subs := make([]chan Event, 0, len(r.subs))
	for _, ch := range r.subs {
		subs = append(subs, ch)
	}
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default: // 遅い購読者はイベントを取りこぼす(再接続で追い付く)
		}
	}
}

// Subscribe returns buffered events after lastSeq plus a live channel。
func (s *RunStore) Subscribe(runID string, lastSeq int64) ([]Event, <-chan Event, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[runID]
	if !ok {
		return nil, nil, nil, ErrRunNotFound
	}
	var replay []Event
	for _, ev := range r.events {
		if ev.Seq > lastSeq {
			replay = append(replay, ev)
		}
	}
	ch := make(chan Event, 64)
	id := r.nextSub
	r.nextSub++
	r.subs[id] = ch
	cancel := func() {
		s.mu.Lock()
		delete(r.subs, id)
		s.mu.Unlock()
	}
	return replay, ch, cancel, nil
}

// Update mutates a run under the store lock and persists it。
func (s *RunStore) Update(r *Run, fn func(*Run)) {
	s.mu.Lock()
	fn(r)
	_ = s.persistLocked(r)
	s.mu.Unlock()
}

// Done returns the completion channel。
func (r *Run) Done() <-chan struct{} { return r.done }

// Terminal reports whether status is final。
func Terminal(status string) bool {
	switch status {
	case RunCompleted, RunFailed, RunCancelled, RunTimedOut:
		return true
	}
	return false
}

// IdemCheck implements Idempotency-Key dedup (§15.1)。
// 戻り値: 既存 run_id(あれば)、payload 不一致 conflict。
func (s *RunStore) IdemCheck(hash, payloadSum string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.idem[hash]
	if !ok || time.Now().After(e.expires) {
		return "", false
	}
	if e.payload != payloadSum {
		return "", true // conflict
	}
	return e.runID, false
}

// IdemRecord stores the mapping。
func (s *RunStore) IdemRecord(hash, payloadSum, runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.idem) > 4096 { // opportunistic sweep
		now := time.Now()
		for k, e := range s.idem {
			if now.After(e.expires) {
				delete(s.idem, k)
			}
		}
	}
	s.idem[hash] = idemEntry{runID: runID, payload: payloadSum, expires: time.Now().Add(idemTTL)}
}

// Sweep removes on-disk run data older than the retention windows and
// drops those runs from memory。日次ディレクトリ単位で削除する。
func (s *RunStore) Sweep(runDays, eventDays int) {
	runsDir := filepath.Join(s.dir, "runs")
	days, err := os.ReadDir(runsDir)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for _, day := range days {
		if !day.IsDir() {
			continue
		}
		t, err := time.Parse("20060102", day.Name())
		if err != nil {
			continue
		}
		age := int(now.Sub(t).Hours() / 24)
		dir := filepath.Join(runsDir, day.Name())
		if age > runDays {
			_ = os.RemoveAll(dir)
			s.mu.Lock()
			for id, r := range s.runs {
				if r.CreatedAt.UTC().Format("20060102") == day.Name() {
					delete(s.runs, id)
				}
			}
			s.mu.Unlock()
			continue
		}
		if age > eventDays {
			files, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, f := range files {
				if strings.HasSuffix(f.Name(), ".events.jsonl") {
					_ = os.Remove(filepath.Join(dir, f.Name()))
				}
			}
		}
	}
}
