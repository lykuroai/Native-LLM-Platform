// Flow Store — Draft / Version のファイル永続化(MRCI-002 §6)。
//
// DBは使わない。<data_dir>/flows/<flow_id>/ 配下に meta.json / draft.json /
// v<N>.json を 0600・temp+rename の原子的書込で保存する(既存
// storeConfigGeneration と同型)。公開済み Version は上書きを拒否する。
package workflow

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Flow statuses (MRCI-001 §10.1 の縮退: DRAFT/PUBLISHED/SUSPENDED/RETIRED。
// INVALID は Validation 結果であり保存状態にしない)。
const (
	FlowDraft     = "draft"
	FlowPublished = "published"
	FlowSuspended = "suspended"
	FlowRetired   = "retired"
)

var (
	ErrFlowNotFound     = fmt.Errorf("workflow: flow not found")
	ErrAliasTaken       = fmt.Errorf("workflow: alias already in use")
	ErrRevisionConflict = fmt.Errorf("workflow: draft revision conflict")
	ErrVersionNotFound  = fmt.Errorf("workflow: version not found")
	ErrFlowRetired      = fmt.Errorf("workflow: flow is retired")
	ErrTooManyFlows     = fmt.Errorf("workflow: max_flows limit reached")
)

// FlowMeta is the non-content flow record。
type FlowMeta struct {
	FlowID        string    `json:"flow_id"`
	Alias         string    `json:"alias"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	DraftRevision int       `json:"draft_revision"`
	LatestVersion int       `json:"latest_version"` // 0 = 未公開
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	RetiredAt     time.Time `json:"retired_at,omitempty"`
}

// Draft is the editable definition with optimistic-lock revision(ETag 相当)。
type Draft struct {
	Revision   int             `json:"revision"`
	Definition json.RawMessage `json:"definition"`
	Checksum   string          `json:"checksum"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// FlowVersion is an immutable published definition。
type FlowVersion struct {
	Version     int             `json:"version"`
	Definition  json.RawMessage `json:"definition"`
	Checksum    string          `json:"checksum"`
	PublishedAt time.Time       `json:"published_at"`
}

// FlowStore persists flows under dir。単一プロセス前提で mutex 排他。
type FlowStore struct {
	dir      string
	maxFlows int

	mu    sync.Mutex
	metas map[string]*FlowMeta // flow_id -> meta
	alias map[string]string    // alias -> flow_id
}

// OpenFlowStore loads the flow index from disk。
func OpenFlowStore(dir string, maxFlows int) (*FlowStore, error) {
	flowsDir := filepath.Join(dir, "flows")
	if err := os.MkdirAll(flowsDir, 0o700); err != nil {
		return nil, fmt.Errorf("workflow: create %s: %w", flowsDir, err)
	}
	s := &FlowStore{dir: dir, maxFlows: maxFlows, metas: map[string]*FlowMeta{}, alias: map[string]string{}}
	entries, err := os.ReadDir(flowsDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(flowsDir, e.Name(), "meta.json"))
		if err != nil {
			return nil, fmt.Errorf("workflow: read meta for %s: %w", e.Name(), err)
		}
		var m FlowMeta
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("workflow: corrupt meta for %s: %w", e.Name(), err)
		}
		if prev, dup := s.alias[m.Alias]; dup {
			return nil, fmt.Errorf("workflow: alias %q used by both %s and %s", m.Alias, prev, m.FlowID)
		}
		s.metas[m.FlowID] = &m
		s.alias[m.Alias] = m.FlowID
	}
	return s, nil
}

func (s *FlowStore) flowDir(flowID string) string {
	return filepath.Join(s.dir, "flows", flowID)
}

// Checksum is the canonical checksum of a definition(compact JSON の sha256)。
func Checksum(def json.RawMessage) (string, json.RawMessage, error) {
	var v any
	if err := json.Unmarshal(def, &v); err != nil {
		return "", nil, err
	}
	compact, err := json.Marshal(v)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(compact)
	return hex.EncodeToString(sum[:]), compact, nil
}

func newFlowID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "flow_" + hex.EncodeToString(b)
}

// atomicWrite writes 0600 via temp+rename(config generation と同じ方式)。
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(raw, '\n'))
}

// List returns all flow metas sorted by updated_at desc。
func (s *FlowStore) List() []FlowMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]FlowMeta, 0, len(s.metas))
	for _, m := range s.metas {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// Get returns one meta。
func (s *FlowStore) Get(flowID string) (*FlowMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metas[flowID]
	if !ok {
		return nil, ErrFlowNotFound
	}
	cp := *m
	return &cp, nil
}

// ResolveAlias maps a published alias to (flow_id, latest_version)。
// suspended / retired / 未公開 flow は解決しない(新規実行拒否)。
func (s *FlowStore) ResolveAlias(alias string) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.alias[alias]
	if !ok {
		return "", 0, ErrFlowNotFound
	}
	m := s.metas[id]
	if m.Status != FlowPublished || m.LatestVersion == 0 {
		return "", 0, ErrFlowNotFound
	}
	return id, m.LatestVersion, nil
}

// Create registers a new draft flow。
func (s *FlowStore) Create(def json.RawMessage, alias, name string) (*FlowMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxFlows > 0 && len(s.metas) >= s.maxFlows {
		return nil, ErrTooManyFlows
	}
	if _, taken := s.alias[alias]; taken {
		return nil, ErrAliasTaken
	}
	sum, compact, err := Checksum(def)
	if err != nil {
		return nil, fmt.Errorf("workflow: definition is not valid JSON: %w", err)
	}
	now := time.Now().UTC()
	m := &FlowMeta{FlowID: newFlowID(), Alias: alias, Name: name, Status: FlowDraft,
		DraftRevision: 1, CreatedAt: now, UpdatedAt: now}
	dir := s.flowDir(m.FlowID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	d := Draft{Revision: 1, Definition: compact, Checksum: sum, UpdatedAt: now}
	if err := writeJSON(filepath.Join(dir, "draft.json"), d); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, "meta.json"), m); err != nil {
		return nil, err
	}
	s.metas[m.FlowID] = m
	s.alias[alias] = m.FlowID
	cp := *m
	return &cp, nil
}

// GetDraft loads the current draft。
func (s *FlowStore) GetDraft(flowID string) (*Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.metas[flowID]; !ok {
		return nil, ErrFlowNotFound
	}
	return s.readDraft(flowID)
}

func (s *FlowStore) readDraft(flowID string) (*Draft, error) {
	raw, err := os.ReadFile(filepath.Join(s.flowDir(flowID), "draft.json"))
	if err != nil {
		return nil, err
	}
	var d Draft
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// UpdateDraft replaces the draft if expectRevision matches(If-Match 相当)。
func (s *FlowStore) UpdateDraft(flowID string, expectRevision int, def json.RawMessage) (*Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metas[flowID]
	if !ok {
		return nil, ErrFlowNotFound
	}
	if m.Status == FlowRetired {
		return nil, ErrFlowRetired
	}
	if m.DraftRevision != expectRevision {
		return nil, ErrRevisionConflict
	}
	sum, compact, err := Checksum(def)
	if err != nil {
		return nil, fmt.Errorf("workflow: definition is not valid JSON: %w", err)
	}
	now := time.Now().UTC()
	d := Draft{Revision: expectRevision + 1, Definition: compact, Checksum: sum, UpdatedAt: now}
	if err := writeJSON(filepath.Join(s.flowDir(flowID), "draft.json"), d); err != nil {
		return nil, err
	}
	m.DraftRevision = d.Revision
	m.UpdatedAt = now
	if err := writeJSON(filepath.Join(s.flowDir(flowID), "meta.json"), m); err != nil {
		return nil, err
	}
	return &d, nil
}

// Publish freezes the draft at expectRevision as the next immutable version。
// Validation は呼出側(Service)が済ませていること。
func (s *FlowStore) Publish(flowID string, expectRevision int) (*FlowVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metas[flowID]
	if !ok {
		return nil, ErrFlowNotFound
	}
	if m.Status == FlowRetired {
		return nil, ErrFlowRetired
	}
	if m.DraftRevision != expectRevision {
		return nil, ErrRevisionConflict
	}
	d, err := s.readDraft(flowID)
	if err != nil {
		return nil, err
	}
	ver := m.LatestVersion + 1
	path := filepath.Join(s.flowDir(flowID), fmt.Sprintf("v%d.json", ver))
	if _, err := os.Stat(path); err == nil {
		// 公開済み Version の上書きは常に拒否(不変性)
		return nil, fmt.Errorf("workflow: version %d already exists for %s", ver, flowID)
	}
	fv := FlowVersion{Version: ver, Definition: d.Definition, Checksum: d.Checksum, PublishedAt: time.Now().UTC()}
	if err := writeJSON(path, fv); err != nil {
		return nil, err
	}
	m.LatestVersion = ver
	m.Status = FlowPublished
	m.UpdatedAt = fv.PublishedAt
	if err := writeJSON(filepath.Join(s.flowDir(flowID), "meta.json"), m); err != nil {
		return nil, err
	}
	return &fv, nil
}

// GetVersion loads an immutable published version and verifies its checksum。
func (s *FlowStore) GetVersion(flowID string, version int) (*FlowVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.metas[flowID]; !ok {
		return nil, ErrFlowNotFound
	}
	raw, err := os.ReadFile(filepath.Join(s.flowDir(flowID), fmt.Sprintf("v%d.json", version)))
	if err != nil {
		return nil, ErrVersionNotFound
	}
	var fv FlowVersion
	if err := json.Unmarshal(raw, &fv); err != nil {
		return nil, err
	}
	sum, _, err := Checksum(fv.Definition)
	if err != nil || sum != fv.Checksum {
		// 改変・破損した公開 Version で実行しない(Fail Closed)
		return nil, fmt.Errorf("workflow: version %d of %s failed checksum verification", version, flowID)
	}
	return &fv, nil
}

// Versions lists published version numbers。
func (s *FlowStore) Versions(flowID string) ([]FlowVersion, error) {
	s.mu.Lock()
	m, ok := s.metas[flowID]
	if !ok {
		s.mu.Unlock()
		return nil, ErrFlowNotFound
	}
	latest := m.LatestVersion
	s.mu.Unlock()
	out := make([]FlowVersion, 0, latest)
	for v := 1; v <= latest; v++ {
		fv, err := s.GetVersion(flowID, v)
		if err != nil {
			return nil, err
		}
		fv.Definition = nil // 一覧は本体を含めない
		out = append(out, *fv)
	}
	return out, nil
}

// SetStatus transitions published<->suspended, or retires the flow。
func (s *FlowStore) SetStatus(flowID, status string) (*FlowMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metas[flowID]
	if !ok {
		return nil, ErrFlowNotFound
	}
	now := time.Now().UTC()
	switch status {
	case FlowSuspended, FlowPublished:
		if m.LatestVersion == 0 {
			return nil, fmt.Errorf("workflow: flow %s has no published version", flowID)
		}
		if m.Status == FlowRetired {
			return nil, ErrFlowRetired
		}
	case FlowRetired:
		m.RetiredAt = now
	default:
		return nil, fmt.Errorf("workflow: status %q is not settable", status)
	}
	m.Status = status
	m.UpdatedAt = now
	if err := writeJSON(filepath.Join(s.flowDir(flowID), "meta.json"), m); err != nil {
		return nil, err
	}
	cp := *m
	return &cp, nil
}
