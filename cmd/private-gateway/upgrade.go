package main

// upgrade.go implements `private-gateway upgrade` / `rollback`
// (BD §17.2 CLI、compose 自動編成)。docker-compose.yaml の Gateway image tag
// を書き換えて `docker compose up -d` を実行し、/healthz で疎通確認する。
// health 確認に失敗した場合は直前 version の compose を復元して再適用する
// (BD §20.2「Upgrade失敗: 直前versionへ自動復帰」)。
//
// 対象は compose 系デプロイのみ。Kubernetes/Helm は values の tag 変更 +
// `helm upgrade` を手動で行う(UPGRADE.md 参照)。

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"
)

// composeImageRe matches the Gateway image line of the packaged compose file。
// 他サービス(顧客が同居させた Runtime 等)の image 行は書き換えない。
var composeImageRe = regexp.MustCompile(`(?m)^(\s*image:\s*)(\S*/private-gateway):(\S+)(\s*)$`)

// upgradeState is persisted next to the compose file(rollback 用)。
type upgradeState struct {
	PreviousVersion string    `json:"previous_version"`
	CurrentVersion  string    `json:"current_version"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func stateFilePath(composeFile string) string {
	return filepath.Join(filepath.Dir(composeFile), ".lykuro-upgrade-state.json")
}

func loadUpgradeState(composeFile string) (*upgradeState, error) {
	raw, err := os.ReadFile(stateFilePath(composeFile))
	if err != nil {
		return nil, err
	}
	var st upgradeState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("upgrade state is corrupt: %w", err)
	}
	return &st, nil
}

func saveUpgradeState(composeFile string, st *upgradeState) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(stateFilePath(composeFile), raw, 0o600)
}

// currentComposeVersion extracts the Gateway image tag。
func currentComposeVersion(content []byte) (string, error) {
	m := composeImageRe.FindSubmatch(content)
	if m == nil {
		return "", fmt.Errorf("compose file has no private-gateway image line")
	}
	return string(m[3]), nil
}

// rewriteComposeVersion replaces the Gateway image tag only。
func rewriteComposeVersion(content []byte, version string) ([]byte, error) {
	if !composeImageRe.Match(content) {
		return nil, fmt.Errorf("compose file has no private-gateway image line")
	}
	return composeImageRe.ReplaceAll(content, []byte("${1}${2}:"+version+"${4}")), nil
}

// commandRunner is injected for tests。
type commandRunner func(name string, args ...string) error

func execRunner(name string, args ...string) error {
	// compose pull+起動を見込んだ上限(health 待ちとは別枠)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stderr // compose の進捗はユーザー向け stderr へ
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// composeUp applies the compose file。`docker compose` が無い環境では
// 旧 `docker-compose` へフォールバックする(precheck と同方針)。
func composeUp(run commandRunner, composeFile string) error {
	if err := run("docker", "compose", "-f", composeFile, "up", "-d"); err == nil {
		return nil
	}
	return run("docker-compose", "-f", composeFile, "up", "-d")
}

// waitHealthy polls /healthz until ok or timeout。
func waitHealthy(client *http.Client, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		req, rerr := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if rerr != nil {
			return rerr
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("health status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		time.Sleep(2 * time.Second)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout")
	}
	return fmt.Errorf("gateway did not become healthy within %s: %w", timeout, lastErr)
}

// composeFileMode preserves the original file permissions(gosec G306 対応:
// 固定 0644 を書かず、パッケージ展開時の mode を維持する)。
func composeFileMode(path string) fs.FileMode {
	if fi, err := os.Stat(path); err == nil {
		return fi.Mode().Perm()
	}
	return 0o600
}

// applyVersion rewrites compose to version, applies it and verifies health。
// 失敗時は元の compose 内容を復元して再適用する(自動復帰)。復元まで失敗した
// 場合は手動対応が必要なため、その旨を返す。
func applyVersion(run commandRunner, client *http.Client, composeFile, version, healthURL string, timeout time.Duration) error {
	original, err := os.ReadFile(composeFile)
	if err != nil {
		return fmt.Errorf("read compose: %w", err)
	}
	next, err := rewriteComposeVersion(original, version)
	if err != nil {
		return err
	}
	if err := os.WriteFile(composeFile, next, composeFileMode(composeFile)); err != nil {
		return fmt.Errorf("write compose: %w", err)
	}
	applyErr := composeUp(run, composeFile)
	if applyErr == nil {
		applyErr = waitHealthy(client, healthURL, timeout)
	}
	if applyErr == nil {
		return nil
	}
	// 自動復帰(BD §20.2)
	fmt.Fprintf(os.Stderr, "upgrade to %s failed (%v); rolling back to previous compose\n", version, applyErr)
	if err := os.WriteFile(composeFile, original, composeFileMode(composeFile)); err != nil {
		return fmt.Errorf("upgrade failed AND compose restore failed (manual recovery required): %w (original error: %v)", err, applyErr)
	}
	if err := composeUp(run, composeFile); err != nil {
		return fmt.Errorf("upgrade failed AND rollback apply failed (manual recovery required): %w (original error: %v)", err, applyErr)
	}
	if err := waitHealthy(client, healthURL, timeout); err != nil {
		return fmt.Errorf("rolled back but gateway is not healthy (manual check required): %w (original error: %v)", err, applyErr)
	}
	return fmt.Errorf("upgrade to %s failed; rolled back to previous version: %w", version, applyErr)
}

func upgradeFlags(fs *flag.FlagSet) (file, healthURL *string, timeout *time.Duration, noApply *bool) {
	file = fs.String("file", "docker-compose.yaml", "compose ファイルパス")
	healthURL = fs.String("health-url", "http://127.0.0.1:8443/healthz", "適用後の health 確認URL")
	timeout = fs.Duration("timeout", 120*time.Second, "health 確認のタイムアウト")
	noApply = fs.Bool("no-apply", false, "compose の書換のみ行い docker compose は実行しない")
	return
}

func runUpgrade(args []string) int {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	to := fs.String("to", "", "アップグレード先 version(必須)")
	file, healthURL, timeout, noApply := upgradeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *to == "" {
		fmt.Fprintln(os.Stderr, "usage: private-gateway upgrade -to <version> [-file docker-compose.yaml] [-health-url URL] [-timeout 120s] [-no-apply]")
		return 2
	}
	original, err := os.ReadFile(*file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read compose: %v\n", err)
		return 1
	}
	current, err := currentComposeVersion(original)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if current == *to {
		fmt.Fprintf(os.Stderr, "already at version %s\n", *to)
		return 0
	}

	if *noApply {
		next, rerr := rewriteComposeVersion(original, *to)
		if rerr != nil {
			fmt.Fprintln(os.Stderr, rerr)
			return 1
		}
		if err := os.WriteFile(*file, next, composeFileMode(*file)); err != nil {
			fmt.Fprintf(os.Stderr, "write compose: %v\n", err)
			return 1
		}
		saveState(*file, current, *to)
		fmt.Fprintf(os.Stderr, "compose updated to %s (not applied). Run: docker compose -f %s up -d\n", *to, *file)
		return 0
	}

	if err := applyVersion(execRunner, http.DefaultClient, *file, *to, *healthURL, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	saveState(*file, current, *to)
	fmt.Fprintf(os.Stderr, "upgraded %s -> %s (healthy)\n", current, *to)
	return 0
}

func runRollback(args []string) int {
	fs := flag.NewFlagSet("rollback", flag.ContinueOnError)
	file, healthURL, timeout, noApply := upgradeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	st, err := loadUpgradeState(*file)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "no upgrade history (.lykuro-upgrade-state.json not found); nothing to roll back")
			return 1
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if st.PreviousVersion == "" {
		fmt.Fprintln(os.Stderr, "upgrade history has no previous version; nothing to roll back")
		return 1
	}
	target := st.PreviousVersion

	if *noApply {
		original, rerr := os.ReadFile(*file)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "read compose: %v\n", rerr)
			return 1
		}
		next, rerr := rewriteComposeVersion(original, target)
		if rerr != nil {
			fmt.Fprintln(os.Stderr, rerr)
			return 1
		}
		if err := os.WriteFile(*file, next, composeFileMode(*file)); err != nil {
			fmt.Fprintf(os.Stderr, "write compose: %v\n", err)
			return 1
		}
		saveState(*file, st.CurrentVersion, target)
		fmt.Fprintf(os.Stderr, "compose reverted to %s (not applied). Run: docker compose -f %s up -d\n", target, *file)
		return 0
	}

	if err := applyVersion(execRunner, http.DefaultClient, *file, target, *healthURL, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	saveState(*file, st.CurrentVersion, target)
	fmt.Fprintf(os.Stderr, "rolled back %s -> %s (healthy)\n", st.CurrentVersion, target)
	return 0
}

// saveState records the transition(失敗してもコマンド自体は成功扱いにしない
// ため、保存エラーは警告のみ)。
func saveState(composeFile, from, to string) {
	if err := saveUpgradeState(composeFile, &upgradeState{
		PreviousVersion: from,
		CurrentVersion:  to,
		UpdatedAt:       time.Now().UTC(),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to record upgrade history: %v\n", err)
	}
}
