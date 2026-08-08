package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const composeSample = `services:
  lykuro-private-gateway:
    image: registry.lykuro.ai/private-gateway:1.2.0
    restart: unless-stopped
  customer-vllm:
    image: vllm/vllm-openai:v0.9.0
`

func TestRewriteComposeVersion(t *testing.T) {
	out, err := rewriteComposeVersion([]byte(composeSample), "1.3.0")
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "registry.lykuro.ai/private-gateway:1.3.0") {
		t.Errorf("gateway image not rewritten:\n%s", s)
	}
	// 同居する他サービスの image は書き換えない
	if !strings.Contains(s, "vllm/vllm-openai:v0.9.0") {
		t.Errorf("unrelated image must not change:\n%s", s)
	}
	v, err := currentComposeVersion(out)
	if err != nil || v != "1.3.0" {
		t.Errorf("currentComposeVersion = %q, %v", v, err)
	}
}

func TestRewriteComposeVersionMissingImage(t *testing.T) {
	if _, err := rewriteComposeVersion([]byte("services: {}\n"), "1.3.0"); err == nil {
		t.Fatal("expected error for compose without gateway image")
	}
}

func writeCompose(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	file := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(file, []byte(composeSample), 0o644); err != nil {
		t.Fatal(err)
	}
	return file
}

// health server: healthy=true なら 200。
func healthServer(t *testing.T, healthy *atomic.Bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestApplyVersionSuccess(t *testing.T) {
	file := writeCompose(t)
	var healthy atomic.Bool
	healthy.Store(true)
	hs := healthServer(t, &healthy)

	var applied []string
	run := func(name string, args ...string) error {
		applied = append(applied, name+" "+strings.Join(args, " "))
		return nil
	}
	err := applyVersion(run, hs.Client(), file, "1.3.0", hs.URL, 5*time.Second)
	if err != nil {
		t.Fatalf("applyVersion: %v", err)
	}
	raw, _ := os.ReadFile(file)
	if !strings.Contains(string(raw), "private-gateway:1.3.0") {
		t.Error("compose not updated")
	}
	if len(applied) != 1 || !strings.Contains(applied[0], "up -d") {
		t.Errorf("compose up not executed once: %v", applied)
	}
}

// health 不合格 → 元 compose を復元して再適用(自動復帰、BD §20.2)。
func TestApplyVersionAutoRollbackOnUnhealthy(t *testing.T) {
	file := writeCompose(t)
	var healthy atomic.Bool // 常に unhealthy
	hs := healthServer(t, &healthy)

	applyCount := 0
	run := func(name string, args ...string) error {
		applyCount++
		return nil
	}
	err := applyVersion(run, hs.Client(), file, "1.3.0", hs.URL, 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "rolled back") && !strings.Contains(err.Error(), "manual") {
		t.Errorf("error should describe rollback outcome: %v", err)
	}
	raw, _ := os.ReadFile(file)
	if !strings.Contains(string(raw), "private-gateway:1.2.0") {
		t.Error("compose must be restored to previous version")
	}
	if applyCount != 2 {
		t.Errorf("compose up should run for upgrade and rollback, got %d", applyCount)
	}
}

// compose 適用自体の失敗でも復元される。
func TestApplyVersionRunFailure(t *testing.T) {
	file := writeCompose(t)
	var healthy atomic.Bool
	healthy.Store(true)
	hs := healthServer(t, &healthy)

	calls := 0
	run := func(name string, args ...string) error {
		calls++
		if calls <= 2 { // upgrade 時: docker compose / docker-compose 両方失敗
			return fmt.Errorf("docker not running")
		}
		return nil // 復元適用は成功
	}
	err := applyVersion(run, hs.Client(), file, "1.3.0", hs.URL, 5*time.Second)
	if err == nil {
		t.Fatal("expected failure")
	}
	raw, _ := os.ReadFile(file)
	if !strings.Contains(string(raw), "private-gateway:1.2.0") {
		t.Error("compose must be restored")
	}
}

func TestUpgradeStateRoundTrip(t *testing.T) {
	file := writeCompose(t)
	saveState(file, "1.2.0", "1.3.0")
	st, err := loadUpgradeState(file)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if st.PreviousVersion != "1.2.0" || st.CurrentVersion != "1.3.0" {
		t.Errorf("state = %+v", st)
	}
}

func TestRunUpgradeNoApply(t *testing.T) {
	file := writeCompose(t)
	if code := runUpgrade([]string{"-to", "1.3.0", "-file", file, "-no-apply"}); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	raw, _ := os.ReadFile(file)
	if !strings.Contains(string(raw), "private-gateway:1.3.0") {
		t.Error("compose not rewritten")
	}
	// rollback -no-apply で戻せる
	if code := runRollback([]string{"-file", file, "-no-apply"}); code != 0 {
		t.Fatalf("rollback exit = %d", code)
	}
	raw, _ = os.ReadFile(file)
	if !strings.Contains(string(raw), "private-gateway:1.2.0") {
		t.Error("compose not reverted")
	}
}

func TestRunUpgradeAlreadyCurrent(t *testing.T) {
	file := writeCompose(t)
	if code := runUpgrade([]string{"-to", "1.2.0", "-file", file}); code != 0 {
		t.Fatalf("exit = %d (no-op should succeed)", code)
	}
}

func TestRunRollbackWithoutHistory(t *testing.T) {
	file := writeCompose(t)
	if code := runRollback([]string{"-file", file}); code != 1 {
		t.Fatalf("exit = %d, want 1 (no history)", code)
	}
}
