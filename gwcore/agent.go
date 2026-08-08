package gwcore

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Control Agent (docs/20260806/ BD §13, Phase 5)。
// 責務: 初回登録、設定取得+署名検証+staging+Last Known Good、heartbeat、
// 利用量メタデータ送信。本文は一切送らない。

// AgentSettings は環境変数(BD §23)から組み立てる。
type AgentSettings struct {
	ControlPlaneURL  string        // LYKURO_CONTROL_PLANE_URL
	DataDir          string        // LYKURO_DATA_DIR(資格情報・設定世代の保存先)
	SigningPubFile   string        // LYKURO_SIGNING_PUB_FILE(設定検証鍵、配布物同梱)
	InstallTokenFile string        // LYKURO_INSTALL_TOKEN_FILE(初回登録のみ)
	Heartbeat        time.Duration // LYKURO_HEARTBEAT_INTERVAL_SECONDS(既定60s)
	ConfigPoll       time.Duration // LYKURO_CONFIG_POLL_INTERVAL_SECONDS(既定300s)
	SoftwareVersion  string
}

const (
	credentialFile = "agent-credential"
	lkgKeep        = 2 // Last Known Good 保持世代数(BD §13.2)
)

// Agent runs the control-plane loop next to the gateway server.
type Agent struct {
	st         AgentSettings
	pub        ed25519.PublicKey
	http       *http.Client
	srv        *Server
	credential string
	applied    int64
}

// NewAgent loads the verification key and any stored credential.
func NewAgent(st AgentSettings, srv *Server) (*Agent, error) {
	if st.ControlPlaneURL == "" {
		return nil, fmt.Errorf("agent: control plane URL is required")
	}
	if st.DataDir == "" {
		return nil, fmt.Errorf("agent: data dir is required")
	}
	if err := os.MkdirAll(st.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: create data dir: %w", err)
	}
	if st.Heartbeat <= 0 {
		st.Heartbeat = 60 * time.Second
	}
	if st.ConfigPoll <= 0 {
		st.ConfigPoll = 300 * time.Second
	}
	pub, err := loadPublicKey(st.SigningPubFile)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		st:   st,
		pub:  pub,
		http: &http.Client{Timeout: 30 * time.Second},
		srv:  srv,
	}
	if raw, err := os.ReadFile(filepath.Join(st.DataDir, credentialFile)); err == nil {
		a.credential = strings.TrimSpace(string(raw))
	}
	return a, nil
}

// LoadSigningPublicKey loads the Ed25519 verification key distributed with
// the package (lykuro-signing.pub)。設定・ライセンス・パッケージ検証で共用。
func LoadSigningPublicKey(path string) (ed25519.PublicKey, error) {
	return loadPublicKey(path)
}

func loadPublicKey(path string) (ed25519.PublicKey, error) {
	if path == "" {
		return nil, fmt.Errorf("agent: signing public key file is required")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agent: read public key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("agent: public key %s: no PEM block", path)
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("agent: parse public key: %w", err)
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("agent: %s is not an Ed25519 key", path)
	}
	return pub, nil
}

// Registered reports whether an agent credential is stored.
func (a *Agent) Registered() bool { return a.credential != "" }

// Register performs the one-time registration (BD §13.1)。
// token 原文はログ・エラーへ出さない。使用後の token file は .used へ改名する。
func (a *Agent) Register(ctx context.Context) error {
	if a.Registered() {
		return nil
	}
	if a.st.InstallTokenFile == "" {
		return fmt.Errorf("agent: not registered and no install token file configured")
	}
	raw, err := os.ReadFile(a.st.InstallTokenFile)
	if err != nil {
		return fmt.Errorf("agent: read install token: %w", err)
	}
	token := strings.TrimSpace(string(raw))

	body, _ := json.Marshal(map[string]string{
		"install_token":    token,
		"software_version": a.st.SoftwareVersion,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.st.ControlPlaneURL+"/api/gateway-agent/v1/register", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("agent: build register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("agent: register request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("agent: register rejected with status %d", resp.StatusCode)
	}
	var out struct {
		AgentToken string `json:"agent_token"`
		GatewayID  string `json:"gateway_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.AgentToken == "" {
		return fmt.Errorf("agent: malformed register response")
	}
	credPath := filepath.Join(a.st.DataDir, credentialFile)
	if err := os.WriteFile(credPath, []byte(out.AgentToken+"\n"), 0o600); err != nil {
		return fmt.Errorf("agent: store credential: %w", err)
	}
	a.credential = out.AgentToken
	// ワンタイムトークンを退避(shell historyに残さない・再利用防止の目印)
	_ = os.Rename(a.st.InstallTokenFile, a.st.InstallTokenFile+".used")
	slog.Info("agent registered", "gateway_id", out.GatewayID)
	return nil
}

// Run drives heartbeat + config poll + usage upload until ctx is done.
func (a *Agent) Run(ctx context.Context) {
	if !a.Registered() {
		slog.Warn("agent: not registered; control plane sync disabled")
		return
	}
	// 起動直後に1回同期
	a.tick(ctx)
	hb := time.NewTicker(a.st.Heartbeat)
	poll := time.NewTicker(a.st.ConfigPoll)
	defer hb.Stop()
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-hb.C:
			a.tick(ctx)
		case <-poll.C:
			if err := a.FetchAndApplyConfig(ctx); err != nil {
				slog.Warn("agent: config poll failed", "error", err)
			}
		}
	}
}

// tick sends one heartbeat (+usage) and pulls config when a newer generation
// is advertised.
func (a *Agent) tick(ctx context.Context) {
	latest, err := a.sendHeartbeat(ctx)
	a.srv.Metrics().RecordHeartbeat(err == nil)
	if err != nil {
		slog.Warn("agent: heartbeat failed", "error", err)
		return
	}
	a.uploadUsage(ctx)
	if latest > a.applied {
		if err := a.FetchAndApplyConfig(ctx); err != nil {
			slog.Warn("agent: config apply failed", "error", err)
		}
	}
}

func (a *Agent) sendHeartbeat(ctx context.Context) (latestConfig int64, err error) {
	// Runtime 到達性を実測して報告する(全滅なら degraded、/readyz と同基準)。
	health := "healthy"
	if !a.srv.RuntimeHealthy(ctx) {
		health = "degraded"
	}
	body, _ := json.Marshal(map[string]any{
		"software_version": a.st.SoftwareVersion,
		"config_version":   a.applied,
		"health_status":    health,
	})
	resp, err := a.call(ctx, http.MethodPost, "/api/gateway-agent/v1/heartbeat", body)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("heartbeat status %d", resp.StatusCode)
	}
	var out struct {
		LatestConfigVersion int64 `json:"latest_config_version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.LatestConfigVersion, nil
}

func (a *Agent) uploadUsage(ctx context.Context) {
	rows := a.srv.DrainUsage()
	if len(rows) == 0 {
		return
	}
	body, _ := json.Marshal(map[string]any{"rows": rows})
	resp, err := a.call(ctx, http.MethodPost, "/api/gateway-agent/v1/usage", body)
	if err != nil {
		a.srv.RestoreUsage(rows) // 上限なし蓄積はしない前提の簡易復元(BD §13.3)
		slog.Warn("agent: usage upload failed", "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		a.srv.RestoreUsage(rows)
		slog.Warn("agent: usage upload rejected", "status", resp.StatusCode)
	}
}

// FetchAndApplyConfig pulls, verifies, stages and applies the active config
// (BD §13.2)。検証失敗時は現設定を維持し config-ack で本文なしコードを返す。
func (a *Agent) FetchAndApplyConfig(ctx context.Context) error {
	resp, err := a.call(ctx, http.MethodGet, "/api/gateway-agent/v1/config", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil // 設定未投入は正常(パッケージ同梱の設定で稼働継続)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("config status %d", resp.StatusCode)
	}
	var cv struct {
		Version       int64           `json:"version"`
		SchemaVersion string          `json:"schema_version"`
		ConfigJSON    json.RawMessage `json:"config_json"`
		SHA256        *string         `json:"sha256"`
		SignatureRef  *string         `json:"signature_ref"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cv); err != nil {
		return fmt.Errorf("decode config: %w", err)
	}
	if cv.Version <= a.applied {
		return nil
	}

	cfg, canonical, verr := verifySignedConfig(a.pub, cv.ConfigJSON, cv.SHA256, cv.SignatureRef)
	if verr != nil {
		slog.Error("agent: config verification failed, keeping current config",
			"version", cv.Version, "error", verr)
		a.ackConfig(ctx, cv.Version, false, "verification_failed")
		return verr
	}

	// 適用世代を保存し LKG を維持してから atomic に切替
	if err := storeConfigGeneration(a.st.DataDir, cv.Version, canonical); err != nil {
		slog.Warn("agent: config store failed (applying in-memory only)", "error", err)
	}
	a.srv.SetConfig(cfg)
	a.applied = cv.Version
	a.srv.Metrics().SetConfigVersion(cv.Version)
	a.ackConfig(ctx, cv.Version, true, "")
	slog.Info("agent: config applied", "version", cv.Version)
	return nil
}

// canonicalJSON mirrors privategw.CanonicalJSON (import cycle 回避のため複製。
// 署名側と同一実装でなければならない)。
func canonicalJSON(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// LoadStoredConfig applies the last applied generation at startup
// (制御プレーン断でも Last Known Good で継続、BD §13.3)。
func (a *Agent) LoadStoredConfig() error {
	raw, err := os.ReadFile(filepath.Join(a.st.DataDir, "config-current.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // まだ配信されていない
		}
		return err
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		return fmt.Errorf("stored config invalid: %w", err)
	}
	a.srv.SetConfig(cfg)
	// 適用版はマーカーから復元する(最大ファイル番号は -force 巻き戻し後に
	// current と一致しないため信用しない)
	a.applied = currentAppliedVersion(a.st.DataDir)
	a.srv.Metrics().SetConfigVersion(a.applied)
	slog.Info("agent: restored stored config", "version", a.applied)
	return nil
}

func (a *Agent) ackConfig(ctx context.Context, version int64, applied bool, code string) {
	body, _ := json.Marshal(map[string]any{
		"version":    version,
		"applied":    applied,
		"error_code": code,
	})
	resp, err := a.call(ctx, http.MethodPost, "/api/gateway-agent/v1/config-ack", body)
	if err != nil {
		slog.Warn("agent: config ack failed", "error", err)
		return
	}
	resp.Body.Close()
}

func (a *Agent) call(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.st.ControlPlaneURL+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.credential)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return a.http.Do(req)
}
