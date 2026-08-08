package gwcore

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// SignedConfigEnvelope is the signed generation format served by the control
// plane(GET /api/gateway-agent/v1/config および管理API GET config と同形)。
// Air-Gapped 環境ではこの JSON をファイルで持ち込み config import する(BD §14.4)。
type SignedConfigEnvelope struct {
	Version       int64           `json:"version"`
	SchemaVersion string          `json:"schema_version"`
	ConfigJSON    json.RawMessage `json:"config_json"`
	SHA256        *string         `json:"sha256"`
	SignatureRef  *string         `json:"signature_ref"`
}

// verifySignedConfig checks digest + Ed25519 signature over the canonical
// form and stages the config through full validation (Agent と import の共通路)。
func verifySignedConfig(pub ed25519.PublicKey, raw json.RawMessage, sha, sig *string) (*Config, []byte, error) {
	if sha == nil || sig == nil {
		return nil, nil, fmt.Errorf("config is unsigned")
	}
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("canonicalize: %w", err)
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != *sha {
		return nil, nil, fmt.Errorf("digest mismatch")
	}
	sigBytes, err := base64.StdEncoding.DecodeString(*sig)
	if err != nil {
		return nil, nil, fmt.Errorf("decode signature: %w", err)
	}
	if !ed25519.Verify(pub, canonical, sigBytes) {
		return nil, nil, fmt.Errorf("signature mismatch")
	}
	cfg, err := ParseConfig(canonical)
	if err != nil {
		return nil, nil, fmt.Errorf("staging validation: %w", err)
	}
	return cfg, canonical, nil
}

// generationVersion parses N from a config-vN.json path.
func generationVersion(path string) (int64, bool) {
	var v int64
	if _, err := fmt.Sscanf(filepath.Base(path), "config-v%d.json", &v); err != nil {
		return 0, false
	}
	return v, true
}

// sortedGenerations lists stored generations in ascending NUMERIC version
// order。文字列ソートは v10 以降で v2 より前に並び、prune が最新世代を
// 消してしまうため必ず数値で比較する。
func sortedGenerations(dataDir string) ([]string, error) {
	entries, err := filepath.Glob(filepath.Join(dataDir, "config-v*.json"))
	if err != nil {
		return nil, err
	}
	valid := entries[:0]
	for _, e := range entries {
		if _, ok := generationVersion(e); ok {
			valid = append(valid, e)
		}
	}
	sort.Slice(valid, func(i, j int) bool {
		vi, _ := generationVersion(valid[i])
		vj, _ := generationVersion(valid[j])
		return vi < vj
	})
	return valid, nil
}

// storeConfigGeneration writes config-v{N}.json, atomically switches
// config-current.json (+版マーカー) and prunes to lkgKeep+1 generations.
func storeConfigGeneration(dataDir string, version int64, canonical []byte) error {
	path := filepath.Join(dataDir, fmt.Sprintf("config-v%d.json", version))
	if err := os.WriteFile(path, canonical, 0o600); err != nil {
		return err
	}
	tmp := filepath.Join(dataDir, "config-current.tmp")
	if err := os.WriteFile(tmp, canonical, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dataDir, "config-current.json")); err != nil {
		return err
	}
	// 現在適用中の版を明示保存する。「最大ファイル番号 = 適用版」の推定は
	// -force 巻き戻し後に嘘になる(古い版が current でも新しい版ファイルが
	// 残る)ため、復元は必ずこのマーカーを優先する。
	if err := os.WriteFile(filepath.Join(dataDir, "config-current.version"),
		[]byte(fmt.Sprintf("%d\n", version)), 0o600); err != nil {
		return err
	}
	entries, err := sortedGenerations(dataDir)
	if err != nil {
		return err
	}
	if len(entries) > lkgKeep+1 {
		for _, old := range entries[:len(entries)-(lkgKeep+1)] {
			if old == path {
				// -force 巻き戻しで最古になった「書き込んだ直後の世代」を
				// 消さない(current の LKG バックアップが失われるため)
				continue
			}
			_ = os.Remove(old)
		}
	}
	return nil
}

// currentAppliedVersion returns the version of config-current.json.
// マーカー優先、無ければ後方互換として最大世代番号へフォールバック。
func currentAppliedVersion(dataDir string) int64 {
	if raw, err := os.ReadFile(filepath.Join(dataDir, "config-current.version")); err == nil {
		var v int64
		if _, err := fmt.Sscanf(string(raw), "%d", &v); err == nil && v > 0 {
			return v
		}
	}
	return currentGenerationVersion(dataDir)
}

// currentGenerationVersion returns the highest stored generation (0 = なし)。
func currentGenerationVersion(dataDir string) int64 {
	entries, err := sortedGenerations(dataDir)
	if err != nil || len(entries) == 0 {
		return 0
	}
	v, _ := generationVersion(entries[len(entries)-1])
	return v
}

// ImportSignedConfig verifies an exported signed generation and stores it as
// the current configuration (Air-Gapped の手動設定更新、BD §14.4/§22.1)。
// 検証に失敗した場合は何も書き込まない(Fail Closed)。
//   - expectedGatewayID が空でなければ、設定の gateway.id との一致を要求する
//     (別 Gateway 向けの正規署名済み設定の誤適用・流用防止)。
//   - 保存済み世代より古い/同じ世代は拒否する(署名は正規でも黙った
//     ダウングレードを防ぐ)。force=true で明示的に上書きできる。
func ImportSignedConfig(dataDir string, pub ed25519.PublicKey, envelope []byte, expectedGatewayID string, force bool) (int64, *Config, error) {
	var env SignedConfigEnvelope
	if err := json.Unmarshal(envelope, &env); err != nil {
		return 0, nil, fmt.Errorf("import: parse envelope: %w", err)
	}
	if env.Version <= 0 {
		return 0, nil, fmt.Errorf("import: envelope version missing")
	}
	cfg, canonical, err := verifySignedConfig(pub, env.ConfigJSON, env.SHA256, env.SignatureRef)
	if err != nil {
		return 0, nil, fmt.Errorf("import: %w", err)
	}
	if expectedGatewayID != "" && cfg.Gateway.ID != expectedGatewayID {
		return 0, nil, fmt.Errorf("import: config is for gateway %q, this gateway is %q",
			cfg.Gateway.ID, expectedGatewayID)
	}
	if current := currentGenerationVersion(dataDir); env.Version <= current && !force {
		return 0, nil, fmt.Errorf("import: generation %d is not newer than stored generation %d (use -force to override)",
			env.Version, current)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return 0, nil, fmt.Errorf("import: data dir: %w", err)
	}
	if err := storeConfigGeneration(dataDir, env.Version, canonical); err != nil {
		return 0, nil, fmt.Errorf("import: store: %w", err)
	}
	return env.Version, cfg, nil
}
