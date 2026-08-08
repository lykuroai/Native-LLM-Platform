// Package memory implements the Conversation Memory service (§10)。
//
// LLM node は会話の正本を保持しない — 本 Store が顧客環境内で会話履歴を管理し、
// Context Builder が推論ごとに必要 context を作る。KV Cache は正本にしない
// (§10.7)。本文は AES-256-GCM で暗号化したローカルファイルにのみ保存し
// (§14.13: 平文 column 禁止)、Control Plane へは送らない(§12.1)。
// Managed mode は明示 opt-in(§10.2)— stateless(既定)は本文保存 0 日。
package memory

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lykuroai/Native-LLM-Platform/platform/platformcfg"
)

// ErrConflict signals an optimistic-lock version mismatch (§10.6)。
var ErrConflict = errors.New("conversation version conflict")

// ErrNotFound signals a missing or deleted conversation。
var ErrNotFound = errors.New("conversation not found")

// conversation ID はファイル名に使うため形式を制限する(path traversal 防止)。
var convIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Message is one stored message (§14.8 相当。ファイル全体を暗号化するため
// フィールドは平文構造で持つ)。
type Message struct {
	Seq       int64     `json:"seq"`
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	TokenEst  int64     `json:"token_est"`
	ModelID   string    `json:"model_id,omitempty"`
	RequestID string    `json:"request_id"`
	Status    string    `json:"status"` // pending | complete | interrupted
	CreatedAt time.Time `json:"created_at"`
}

// Summary is one range summary (§14.9)。
type Summary struct {
	FromSeq        int64     `json:"source_from_seq"`
	ToSeq          int64     `json:"source_to_seq"`
	Text           string    `json:"text"`
	GeneratorModel string    `json:"generator_model"`
	CreatedAt      time.Time `json:"created_at"`
}

// Conversation is the encrypted-at-rest record (§14.7 相当)。
type Conversation struct {
	ID            string    `json:"id"`
	DataClass     string    `json:"data_class"`
	RetentionDays int       `json:"retention_days"`
	Version       int64     `json:"version"`
	Status        string    `json:"status"` // active | deleting
	NextSeq       int64     `json:"next_seq"`
	ExpiresAt     time.Time `json:"expires_at"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Messages      []Message `json:"messages"`
	Summaries     []Summary `json:"summaries"`
	// SeenRequests dedupes appends (Idempotency、§10.6)。
	SeenRequests []string `json:"seen_requests,omitempty"`
}

// Store is the file-backed encrypted conversation store。
type Store struct {
	dir       string
	key       []byte // AES-256
	retention platformcfg.Retention

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per-conversation
}

// Open initializes the store。鍵ファイルは 64 hex 文字(32 byte)を要求する。
func Open(cfg platformcfg.Memory) (*Store, error) {
	raw, err := os.ReadFile(cfg.EncryptionKeyFile)
	if err != nil {
		return nil, fmt.Errorf("memory: read encryption key: %w", err)
	}
	keyHex := strings.TrimSpace(string(raw))
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("memory: encryption key must be 64 hex chars (32 bytes)")
	}
	dir := filepath.Join(cfg.DataDir, "conversations")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("memory: create data dir: %w", err)
	}
	return &Store{dir: dir, key: key, retention: cfg.Retention, locks: map[string]*sync.Mutex{}}, nil
}

func (s *Store) lock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[id]
	if !ok {
		l = &sync.Mutex{}
		s.locks[id] = l
	}
	return l
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".enc") }

// --- encryption (AES-256-GCM、nonce 前置) ---

func (s *Store) seal(plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize(), gcm.NonceSize()+len(plain)+gcm.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return append(nonce, gcm.Seal(nil, nonce, plain, nil)...), nil
}

func (s *Store) unseal(sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
}

func (s *Store) read(id string) (*Conversation, error) {
	sealed, err := os.ReadFile(s.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	plain, err := s.unseal(sealed)
	if err != nil {
		return nil, fmt.Errorf("memory: decrypt conversation: %w", err)
	}
	var c Conversation
	if err := json.Unmarshal(plain, &c); err != nil {
		return nil, err
	}
	if c.Status == "deleting" || (!c.ExpiresAt.IsZero() && time.Now().After(c.ExpiresAt)) {
		// 期限切れは読み出さない(Fail Closed)— 掃除は sweep が行う
		return nil, ErrNotFound
	}
	return &c, nil
}

func (s *Store) write(c *Conversation) error {
	c.UpdatedAt = time.Now().UTC()
	plain, err := json.Marshal(c)
	if err != nil {
		return err
	}
	sealed, err := s.seal(plain)
	if err != nil {
		return err
	}
	tmp := s.path(c.ID) + ".tmp"
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(c.ID))
}

// GetOrCreate loads a conversation, creating it on first use。
// expectedVersion > 0 で不一致なら ErrConflict(§10.6 optimistic locking)。
func (s *Store) GetOrCreate(id, dataClass string, expectedVersion int64) (*Conversation, error) {
	if !convIDRe.MatchString(id) {
		return nil, fmt.Errorf("memory: invalid conversation id")
	}
	l := s.lock(id)
	l.Lock()
	defer l.Unlock()
	c, err := s.read(id)
	if errors.Is(err, ErrNotFound) {
		now := time.Now().UTC()
		days := s.retention.MessageDays
		c = &Conversation{
			ID: id, DataClass: dataClass, RetentionDays: days,
			Version: 1, Status: "active", NextSeq: 1,
			ExpiresAt: now.AddDate(0, 0, days), CreatedAt: now,
		}
		if err := s.write(c); err != nil {
			return nil, err
		}
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if expectedVersion > 0 && c.Version != expectedVersion {
		return nil, ErrConflict
	}
	return c, nil
}

// AppendTurn stores the user message(s) and the assistant reply after a
// successful inference (§10.3 手順8: 正常確定後に保存)。
// requestID が既出なら二重保存せず現 version を返す(idempotency)。
func (s *Store) AppendTurn(id string, expectedVersion int64, requestID, modelID string, userMsgs []Message, assistantContent, assistantStatus string) (int64, error) {
	l := s.lock(id)
	l.Lock()
	defer l.Unlock()
	c, err := s.read(id)
	if err != nil {
		return 0, err
	}
	if expectedVersion > 0 && c.Version != expectedVersion {
		return 0, ErrConflict
	}
	for _, seen := range c.SeenRequests {
		if seen == requestID {
			return c.Version, nil
		}
	}
	now := time.Now().UTC()
	for _, m := range userMsgs {
		m.Seq = c.NextSeq
		c.NextSeq++
		m.RequestID = requestID
		m.Status = "complete"
		m.CreatedAt = now
		m.TokenEst = estimateTokens(m.Content)
		c.Messages = append(c.Messages, m)
	}
	if assistantStatus == "" {
		assistantStatus = "complete"
	}
	c.Messages = append(c.Messages, Message{
		Seq: c.NextSeq, Role: "assistant", Content: assistantContent,
		TokenEst: estimateTokens(assistantContent), ModelID: modelID,
		RequestID: requestID, Status: assistantStatus, CreatedAt: now,
	})
	c.NextSeq++
	c.Version++
	c.SeenRequests = append(c.SeenRequests, requestID)
	if len(c.SeenRequests) > 256 {
		c.SeenRequests = c.SeenRequests[len(c.SeenRequests)-256:]
	}
	// retention は作成時 policy を維持(短縮は SetRetention 経由で明示適用)
	if err := s.write(c); err != nil {
		return 0, err
	}
	return c.Version, nil
}

// ReplaceWithSummary substitutes messages [fromSeq, toSeq] with a summary
// (§10.5: token 上限超過時は古い message から summary へ置換)。
func (s *Store) ReplaceWithSummary(id string, fromSeq, toSeq int64, text, generatorModel string) error {
	l := s.lock(id)
	l.Lock()
	defer l.Unlock()
	c, err := s.read(id)
	if err != nil {
		return err
	}
	kept := c.Messages[:0]
	for _, m := range c.Messages {
		if m.Seq < fromSeq || m.Seq > toSeq {
			kept = append(kept, m)
		}
	}
	c.Messages = kept
	c.Summaries = append(c.Summaries, Summary{
		FromSeq: fromSeq, ToSeq: toSeq, Text: text,
		GeneratorModel: generatorModel, CreatedAt: time.Now().UTC(),
	})
	c.Version++
	return s.write(c)
}

// Delete performs the cascade delete (§11.4)。message・summary は同一
// 暗号化ファイル内のためファイル削除で連動する。
func (s *Store) Delete(id string) error {
	if !convIDRe.MatchString(id) {
		return fmt.Errorf("memory: invalid conversation id")
	}
	l := s.lock(id)
	l.Lock()
	defer l.Unlock()
	err := os.Remove(s.path(id))
	if os.IsNotExist(err) {
		return ErrNotFound
	}
	return err
}

// SweepExpired physically removes expired conversations (§11.4: active
// storage からの physical delete)。返り値は削除件数。
func (s *Store) SweepExpired() (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, err
	}
	deleted := 0
	now := time.Now()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".enc") {
			continue
		}
		id := strings.TrimSuffix(name, ".enc")
		l := s.lock(id)
		l.Lock()
		sealed, rerr := os.ReadFile(s.path(id))
		if rerr != nil {
			l.Unlock()
			continue
		}
		expired := false
		if plain, derr := s.unseal(sealed); derr != nil {
			// 復号不能ファイルは正本になり得ない — 残さない
			expired = true
		} else {
			var c Conversation
			if json.Unmarshal(plain, &c) != nil {
				expired = true
			} else if !c.ExpiresAt.IsZero() && now.After(c.ExpiresAt) {
				expired = true
			}
		}
		if expired {
			if os.Remove(s.path(id)) == nil {
				deleted++
			}
		}
		l.Unlock()
	}
	return deleted, nil
}

// estimateTokens is a coarse token estimate(実 tokenizer は model 依存のため
// Context Builder では概算を使い、safety margin で吸収する。§10.5 の
// 「model 変更時の再計算」は概算では同一)。
func estimateTokens(s string) int64 {
	n := int64(len(s)) / 4
	if n == 0 && s != "" {
		n = 1
	}
	return n
}
