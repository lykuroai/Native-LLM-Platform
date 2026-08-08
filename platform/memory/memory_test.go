package memory

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/lykuroai/Native-LLM-Platform/platform/platformcfg"
)

func newStore(t *testing.T, messageDays int) *Store {
	t.Helper()
	dir := t.TempDir()
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "mem.key")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(buf)), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(platformcfg.Memory{
		Mode: "managed", DataDir: dir, EncryptionKeyFile: keyFile,
		Retention: platformcfg.Retention{MessageDays: messageDays},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

const secret = "TOP-SECRET-PROMPT-BODY-do-not-store-in-plaintext"

func appendSecretTurn(t *testing.T, s *Store, id, reqID string) int64 {
	t.Helper()
	c, err := s.GetOrCreate(id, "internal", 0)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	v, err := s.AppendTurn(id, c.Version, reqID, "qwen-local",
		[]Message{{Role: "user", Content: secret}}, "reply:"+secret, "complete")
	if err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	return v
}

// 本文は保存時に必ず AES-256-GCM 暗号化される(§12.1 encryption at rest)。
func TestEncryptionAtRest(t *testing.T) {
	s := newStore(t, 30)
	appendSecretTurn(t, s, "conv-enc", "req-1")

	raw, err := os.ReadFile(s.path("conv-enc"))
	if err != nil {
		t.Fatalf("read stored file: %v", err)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("conversation body stored in plaintext")
	}
	// 復号経由では読める
	c, err := s.GetOrCreate("conv-enc", "internal", 0)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(c.Messages) != 2 || c.Messages[0].Content != secret {
		t.Fatalf("decrypted messages = %+v", c.Messages)
	}
}

// 同一 request_id の再送は二重保存しない(§10.6 idempotency)。
func TestAppendIdempotency(t *testing.T) {
	s := newStore(t, 30)
	v1 := appendSecretTurn(t, s, "conv-idem", "req-1")
	v2, err := s.AppendTurn("conv-idem", v1, "req-1", "qwen-local",
		[]Message{{Role: "user", Content: secret}}, "reply", "complete")
	if err != nil {
		t.Fatalf("duplicate AppendTurn: %v", err)
	}
	if v2 != v1 {
		t.Errorf("duplicate append advanced version: %d → %d", v1, v2)
	}
	c, _ := s.GetOrCreate("conv-idem", "internal", 0)
	if len(c.Messages) != 2 {
		t.Errorf("messages = %d, want 2 (no double append)", len(c.Messages))
	}
}

// version 不一致は ErrConflict(§10.6 optimistic locking)。
func TestVersionConflict(t *testing.T) {
	s := newStore(t, 30)
	appendSecretTurn(t, s, "conv-ver", "req-1")
	if _, err := s.GetOrCreate("conv-ver", "internal", 999); !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict, got %v", err)
	}
}

// Delete は cascade で message・summary を物理削除する(§11.4)。
func TestDeleteCascade(t *testing.T) {
	s := newStore(t, 30)
	appendSecretTurn(t, s, "conv-del", "req-1")
	if err := s.Delete("conv-del"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(s.path("conv-del")); !os.IsNotExist(err) {
		t.Error("file must be physically removed")
	}
	if err := s.Delete("conv-del"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
	// 削除後の再作成は過去 version を引き継がない(新規会話として version 1)
	c, err := s.GetOrCreate("conv-del", "internal", 5)
	if !errors.Is(err, nil) || c.Version != 1 {
		t.Errorf("recreate after delete: version=%v err=%v, want fresh version 1", c, err)
	}
}

// 期限切れ会話は読み出せず(Fail Closed)、sweep で物理削除される(§11.4)。
func TestRetentionExpiry(t *testing.T) {
	s := newStore(t, -1) // ExpiresAt を過去にする
	c, err := s.GetOrCreate("conv-exp", "internal", 0)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if c.RetentionDays != -1 {
		t.Fatalf("retention days = %d", c.RetentionDays)
	}

	// 期限切れは読み出し不可(新規作成として扱われる前に sweep 対象)
	if _, err := os.Stat(s.path("conv-exp")); err != nil {
		t.Fatalf("stored file missing: %v", err)
	}
	n, err := s.SweepExpired()
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if n != 1 {
		t.Errorf("swept = %d, want 1", n)
	}
	if _, err := os.Stat(s.path("conv-exp")); !os.IsNotExist(err) {
		t.Error("expired conversation must be physically removed")
	}
}

// 有効期限内の会話は sweep で消えない。
func TestSweepKeepsActive(t *testing.T) {
	s := newStore(t, 30)
	appendSecretTurn(t, s, "conv-live", "req-1")
	n, err := s.SweepExpired()
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if n != 0 {
		t.Errorf("swept = %d, want 0", n)
	}
	if _, err := os.Stat(s.path("conv-live")); err != nil {
		t.Errorf("active conversation must survive sweep: %v", err)
	}
}

// 復号不能ファイル(鍵ローテーション等)は正本になり得ないため sweep で除去。
func TestSweepRemovesUndecryptable(t *testing.T) {
	s := newStore(t, 30)
	if err := os.WriteFile(s.path("conv-bad"), []byte("garbage-not-encrypted"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := s.SweepExpired()
	if err != nil {
		t.Fatalf("SweepExpired: %v", err)
	}
	if n != 1 {
		t.Errorf("swept = %d, want 1", n)
	}
}
