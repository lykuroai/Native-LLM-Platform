package gwcore

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signedEnvelope builds an exported generation like the control plane does.
func signedEnvelope(t *testing.T, priv ed25519.PrivateKey, version int64, logicalModel string) []byte {
	t.Helper()
	raw := []byte(`{
		"schema_version": "1",
		"gateway": {"id": "gw-air", "allowed_data_classes": ["public", "confidential"]},
		"models": [{"logical_name": "` + logicalModel + `", "runtime": "ollama", "endpoint": "http://localhost:11434", "physical_model": "qwen3:32b"}]
	}`)
	canonical, err := canonicalJSON(raw)
	require.NoError(t, err)
	sum := sha256.Sum256(canonical)
	env := map[string]any{
		"version":        version,
		"schema_version": "1",
		"config_json":    json.RawMessage(canonical),
		"sha256":         hex.EncodeToString(sum[:]),
		"signature_ref":  base64.StdEncoding.EncodeToString(ed25519.Sign(priv, canonical)),
	}
	out, err := json.Marshal(env)
	require.NoError(t, err)
	return out
}

func TestImportSignedConfig(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	dataDir := t.TempDir()

	version, cfg, err := ImportSignedConfig(dataDir, pub, signedEnvelope(t, priv, 5, "airgap-model"), "gw-air", false)
	require.NoError(t, err)
	assert.Equal(t, int64(5), version)
	assert.NotNil(t, cfg.FindModel("airgap-model"))
	assert.FileExists(t, filepath.Join(dataDir, "config-current.json"))
	assert.FileExists(t, filepath.Join(dataDir, "config-v5.json"))

	// import した設定は起動時ロード(ParseConfig)と互換
	stored, err := LoadConfig(filepath.Join(dataDir, "config-current.json"))
	require.NoError(t, err)
	assert.Equal(t, "gw-air", stored.Gateway.ID)
}

func TestImportRejectsTampered(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	dataDir := t.TempDir()

	env := signedEnvelope(t, priv, 1, "good-model")
	tampered := []byte(replaceOnce(string(env), "good-model", "evil-model"))
	_, _, verr := ImportSignedConfig(dataDir, pub, tampered, "gw-air", false)
	assert.Error(t, verr)
	assert.NoFileExists(t, filepath.Join(dataDir, "config-current.json"), "拒否時は何も書かない")

	// 別鍵の署名も拒否
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, _, verr = ImportSignedConfig(dataDir, otherPub, env, "gw-air", false)
	assert.ErrorContains(t, verr, "signature mismatch")

	// エンベロープ形式不正
	_, _, verr = ImportSignedConfig(dataDir, pub, []byte("not json"), "gw-air", false)
	assert.Error(t, verr)
	_, _, verr = ImportSignedConfig(dataDir, pub, []byte(`{"version":0}`), "gw-air", false)
	assert.ErrorContains(t, verr, "version missing")

	// 別 Gateway 向けの正規署名済み設定は拒否(束縛)
	_, _, verr = ImportSignedConfig(dataDir, pub, env, "gw-other", false)
	assert.ErrorContains(t, verr, "this gateway is")
}

func TestImportRejectsDowngrade(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	dataDir := t.TempDir()

	_, _, err = ImportSignedConfig(dataDir, pub, signedEnvelope(t, priv, 5, "v5-model"), "gw-air", false)
	require.NoError(t, err)

	// 古い世代・同一世代は拒否(黙ったダウングレード防止)
	_, _, verr := ImportSignedConfig(dataDir, pub, signedEnvelope(t, priv, 3, "v3-model"), "gw-air", false)
	assert.ErrorContains(t, verr, "not newer")
	_, _, verr = ImportSignedConfig(dataDir, pub, signedEnvelope(t, priv, 5, "v5b-model"), "gw-air", false)
	assert.ErrorContains(t, verr, "not newer")
	// 現行世代は維持されている
	stored, err := LoadConfig(filepath.Join(dataDir, "config-current.json"))
	require.NoError(t, err)
	assert.NotNil(t, stored.FindModel("v5-model"))

	// -force で明示的に巻き戻せる
	version, cfg, err := ImportSignedConfig(dataDir, pub, signedEnvelope(t, priv, 3, "v3-model"), "gw-air", true)
	require.NoError(t, err)
	assert.Equal(t, int64(3), version)
	assert.NotNil(t, cfg.FindModel("v3-model"))
}

func TestForceRollbackKeepsCurrentGenerationAndVersion(t *testing.T) {
	// LKG が満杯(3世代)の Gateway で -force 巻き戻しした場合:
	//  - 書き込んだ直後の世代ファイルが prune で消えない
	//  - 適用版マーカーが current と一致する(最大ファイル番号への推定だと
	//    v10 と誤復元し、heartbeat が嘘を報告して再配信されない silent drift)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	dataDir := t.TempDir()

	for _, v := range []int64{8, 9, 10} {
		_, _, err := ImportSignedConfig(dataDir, pub, signedEnvelope(t, priv, v, fmt.Sprintf("model-v%d", v)), "gw-air", false)
		require.NoError(t, err)
	}

	_, cfg, err := ImportSignedConfig(dataDir, pub, signedEnvelope(t, priv, 3, "rollback-model"), "gw-air", true)
	require.NoError(t, err)
	assert.NotNil(t, cfg.FindModel("rollback-model"))

	// 書いた直後の v3 は保護され、current は v3 の内容
	assert.FileExists(t, filepath.Join(dataDir, "config-v3.json"))
	stored, err := LoadConfig(filepath.Join(dataDir, "config-current.json"))
	require.NoError(t, err)
	assert.NotNil(t, stored.FindModel("rollback-model"))

	// 適用版はマーカーで 3 と復元される(最大ファイル番号 10 ではなく)
	assert.Equal(t, int64(3), currentAppliedVersion(dataDir))
}

func replaceOnce(s, old, new string) string {
	i := len(s)
	for j := 0; j+len(old) <= len(s); j++ {
		if s[j:j+len(old)] == old {
			i = j
			break
		}
	}
	if i == len(s) {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}
