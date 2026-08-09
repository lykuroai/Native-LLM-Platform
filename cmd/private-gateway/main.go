// private-gateway is the customer-side Private LLM Gateway Web API service
// (docs/20260806/ Phase 4 MVP). 顧客環境で常時稼働し、ローカルLLM Runtime を
// OpenAI 互換APIとして提供する。Lykuro SaaS(cmd/gateway)とは別バイナリ。
//
// Usage:
//
//	private-gateway init   -config gateway.yaml   (Runtime検出から設定を自動生成)
//	private-gateway serve  -config /etc/lykuro/gateway/gateway.yaml
//	private-gateway genkey            (Virtual Key を1つ発行しハッシュを表示)
//	private-gateway admin-token       (管理画面トークンを発行しハッシュを保存)
//	private-gateway config validate -config <path>
//	private-gateway version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lykuroai/Native-LLM-Platform/gwcore"
	"github.com/lykuroai/Native-LLM-Platform/platform"
	"github.com/lykuroai/Native-LLM-Platform/sign"
)

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && args[0][0] != '-' {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "serve":
		os.Exit(runServe(args))
	case "genkey":
		os.Exit(runGenkey())
	case "admin-token":
		os.Exit(runAdminToken())
	case "register":
		os.Exit(runRegister(args))
	case "precheck":
		os.Exit(runPrecheck(args))
	case "discover":
		os.Exit(runDiscover(args))
	case "init":
		os.Exit(runInit(args))
	case "status":
		os.Exit(runStatus(args))
	case "diagnose":
		os.Exit(runDiagnose(args))
	case "config":
		if len(args) > 0 && args[0] == "validate" {
			os.Exit(runValidate(args[1:]))
		}
		if len(args) > 0 && args[0] == "import" {
			os.Exit(runConfigImport(args[1:]))
		}
		fmt.Fprintln(os.Stderr, "usage: private-gateway config validate|import ...")
		os.Exit(2)
	case "upgrade":
		os.Exit(runUpgrade(args))
	case "rollback":
		os.Exit(runRollback(args))
	case "version":
		fmt.Println(gwcore.Version)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		os.Exit(2)
	}
}

// dataDirDefault resolves the local state directory (LYKURO_DATA_DIR 優先)。
func dataDirDefault() string {
	if d := os.Getenv("LYKURO_DATA_DIR"); d != "" {
		return d
	}
	return "/var/lib/lykuro/gateway"
}

// agentSettings builds AgentSettings from the environment (BD §23)。
// LYKURO_CONTROL_PLANE_URL 未設定なら (nil 設定, false)。
func agentSettings() (gwcore.AgentSettings, bool) {
	url := os.Getenv("LYKURO_CONTROL_PLANE_URL")
	if url == "" {
		return gwcore.AgentSettings{}, false
	}
	dataDir := dataDirDefault()
	st := gwcore.AgentSettings{
		ControlPlaneURL:  url,
		DataDir:          dataDir,
		SigningPubFile:   os.Getenv("LYKURO_SIGNING_PUB_FILE"),
		InstallTokenFile: os.Getenv("LYKURO_INSTALL_TOKEN_FILE"),
		SoftwareVersion:  gwcore.Version,
	}
	if v, err := strconv.Atoi(os.Getenv("LYKURO_HEARTBEAT_INTERVAL_SECONDS")); err == nil && v > 0 {
		st.Heartbeat = time.Duration(v) * time.Second
	}
	if v, err := strconv.Atoi(os.Getenv("LYKURO_CONFIG_POLL_INTERVAL_SECONDS")); err == nil && v > 0 {
		st.ConfigPoll = time.Duration(v) * time.Second
	}
	return st, true
}

func configPath(args []string) string {
	// LYKURO_CONFIG_PATH 環境変数 > -config フラグ > 既定
	path := "/etc/lykuro/gateway/gateway.yaml"
	if env := os.Getenv("LYKURO_CONFIG_PATH"); env != "" {
		path = env
	}
	for i, a := range args {
		if (a == "-config" || a == "--config") && i+1 < len(args) {
			path = args[i+1]
		}
	}
	return path
}

func runServe(args []string) int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := gwcore.LoadConfig(configPath(args))
	if err != nil {
		slog.Error("config load failed", "error", err)
		return 2
	}
	audit, err := gwcore.NewAuditLogger(cfg.Audit.Path)
	if err != nil {
		slog.Error("audit logger init failed", "error", err)
		return 2
	}
	defer audit.Close()

	// ライセンス検証(Phase 6, BD §15)。LYKURO_LICENSE_FILE 構成時のみ。
	// 期限+grace 超過は起動拒否(Fail Closed)。grace 中は警告して継続。
	if code := checkLicense(cfg.Gateway.ID); code != 0 {
		return code
	}
	// 常駐中も日次で再検証し、期限接近・超過を可視化する(強制停止はしない —
	// 稼働中サービスの即断は契約判断のため、超過は ERROR ログで運用へ通知)。
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if code := checkLicense(cfg.Gateway.ID); code != 0 {
				slog.Error("license is no longer valid; service continues but renewal is required (restart will fail closed)")
			}
		}
	}()

	srv := gwcore.NewServer(cfg, audit)

	// Native LLM Platform(LYK-NLP-SD-001 v2.0): platform.enabled 時のみ
	// backend を構築。config hot reload(Agent/import)でも再構築する。
	// 構築失敗は Fail Closed — platform route は有効化されず、リクエストは
	// flag 判定により拒否ではなく旧 route(models 定義があれば)を通る。
	var (
		platformMu sync.Mutex
		activePf   *platform.Platform
	)
	applyPlatform := func(c *gwcore.Config) {
		platformMu.Lock()
		defer platformMu.Unlock()
		if !c.PlatformEnabled() {
			srv.SetPlatformBackend(nil)
			if activePf != nil {
				activePf.Close()
				activePf = nil
			}
			return
		}
		pf, perr := platform.New(c.Platform, platform.Options{})
		if perr != nil {
			slog.Error("platform init failed; platform route disabled", "error", perr)
			srv.SetPlatformBackend(nil)
			if activePf != nil {
				activePf.Close()
				activePf = nil
			}
			return
		}
		srv.SetPlatformBackend(pf)
		if activePf != nil {
			activePf.Close() // 旧 backend の background loop を停止
		}
		activePf = pf
		slog.Info("platform route enabled",
			"models", len(c.Platform.Models),
			"endpoints", len(c.Platform.RuntimeEndpoints))
	}
	srv.OnConfigApplied(applyPlatform)
	applyPlatform(cfg)

	httpSrv := &http.Server{
		Addr:              cfg.Gateway.Listen,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Air-Gapped: 制御プレーン未構成でも config import 済みの世代(LKG)を反映
	if _, ok := agentSettings(); !ok {
		dataDir := dataDirDefault()
		if raw, rerr := os.ReadFile(filepath.Join(dataDir, "config-current.json")); rerr == nil {
			if imported, perr := gwcore.ParseConfig(raw); perr == nil {
				srv.SetConfig(imported)
				slog.Info("applied imported config generation", "data_dir", dataDir)
			} else {
				slog.Warn("stored config invalid; using packaged config", "error", perr)
			}
		}
	}

	// Control Agent(Phase 5): LYKURO_CONTROL_PLANE_URL 構成時のみ起動。
	// Agent が無くてもパッケージ同梱の設定で単独稼働できる(BD §13.3)。
	if st, ok := agentSettings(); ok {
		agent, err := gwcore.NewAgent(st, srv)
		if err != nil {
			slog.Error("agent init failed; running standalone", "error", err)
		} else {
			if err := agent.LoadStoredConfig(); err != nil {
				slog.Warn("agent: stored config restore failed", "error", err)
			}
			if !agent.Registered() {
				if err := agent.Register(ctx); err != nil {
					slog.Warn("agent registration failed; will run standalone until restart", "error", err)
				}
			}
			go agent.Run(ctx)
		}
	}

	// /metrics(ADD §17.1): LYKURO_METRICS_ENABLED=true でのみ別listenerで公開。
	// 外部公開しない前提のため既定は loopback。非loopbackバインドは警告する。
	if v := os.Getenv("LYKURO_METRICS_ENABLED"); v == "true" || v == "1" {
		listen := os.Getenv("LYKURO_METRICS_LISTEN")
		if listen == "" {
			listen = "127.0.0.1:9464"
		}
		if host, _, herr := net.SplitHostPort(listen); herr == nil {
			if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
				slog.Warn("metrics listener is not loopback; ensure the address is management-network only (BD §19)",
					"listen", listen)
			}
		}
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", srv.Metrics())
		metricsSrv := &http.Server{
			Addr:              listen,
			Handler:           metricsMux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			slog.Info("metrics endpoint enabled", "listen", listen)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				// 監視系の失敗で推論を止めない(ADD §16「Metrics停止: inference継続」)
				slog.Error("metrics server failed; inference continues", "error", err)
			}
		}()
		defer metricsSrv.Close()
	}

	// 管理画面(embedded admin UI): LYKURO_ADMIN_ENABLED=true でのみ別listenerで
	// 公開。既定は loopback。admin-token 未発行なら起動しない(Fail Closed —
	// 認証なしの管理面は絶対に開けない)。
	if v := os.Getenv("LYKURO_ADMIN_ENABLED"); v == "true" || v == "1" {
		listen := os.Getenv("LYKURO_ADMIN_LISTEN")
		if listen == "" {
			listen = "127.0.0.1:9465"
		}
		if host, _, herr := net.SplitHostPort(listen); herr == nil {
			if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
				slog.Warn("admin listener is not loopback; ensure the address is management-network only",
					"listen", listen)
			}
		}
		hashPath := filepath.Join(dataDirDefault(), gwcore.AdminTokenHashFile)
		hashRaw, err := os.ReadFile(hashPath)
		if err != nil {
			slog.Error("admin UI enabled but admin token is not provisioned; run 'private-gateway admin-token' first (admin UI stays off)",
				"path", hashPath, "error", err)
		} else {
			_, connected := agentSettings()
			adminSrv := &http.Server{
				Addr: listen,
				Handler: gwcore.NewAdminHandler(srv, gwcore.AdminOptions{
					TokenHash:  strings.TrimSpace(string(hashRaw)),
					ConfigPath: configPath(args),
					AuditPath:  cfg.Audit.Path,
					Connected:  connected,
				}),
				ReadHeaderTimeout: 10 * time.Second,
			}
			go func() {
				slog.Info("admin UI enabled", "listen", listen)
				if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
					// 管理面の失敗で推論を止めない(metrics と同じ方針)
					slog.Error("admin server failed; inference continues", "error", err)
				}
			}()
			defer adminSrv.Close()
		}
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("shutdown failed", "error", err)
		}
	}()

	slog.Info("private gateway started",
		"gateway_id", cfg.Gateway.ID,
		"listen", cfg.Gateway.Listen,
		"strict_local_mode", cfg.StrictLocal(),
		"models", len(cfg.Models),
		"version", gwcore.Version,
	)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server failed", "error", err)
		return 1
	}
	slog.Info("private gateway stopped")
	return 0
}

// checkLicense verifies the offline license when configured (BD §15)。
// 戻り値 0 = 続行可、31 = ライセンス不正(ADD §11.6)。
func checkLicense(gatewayID string) int {
	licPath := os.Getenv("LYKURO_LICENSE_FILE")
	if licPath == "" {
		return 0 // ライセンスなし運転(PoC/評価)を許容
	}
	pubPath := os.Getenv("LYKURO_SIGNING_PUB_FILE")
	if pubPath == "" {
		slog.Error("LYKURO_LICENSE_FILE is set but LYKURO_SIGNING_PUB_FILE is missing")
		return 31
	}
	pub, err := gwcore.LoadSigningPublicKey(pubPath)
	if err != nil {
		slog.Error("license verification key load failed", "error", err)
		return 31
	}
	raw, err := os.ReadFile(licPath)
	if err != nil {
		slog.Error("license file read failed", "error", err)
		return 31
	}
	lic, state, err := sign.VerifyLicense(raw, pub, gatewayID, time.Now())
	switch state {
	case sign.LicenseValid:
		slog.Info("license valid", "edition", lic.Edition, "expires_at", lic.ExpiresAt)
	case sign.LicenseInGrace:
		slog.Warn("license expired but within grace period; renew soon",
			"expires_at", lic.ExpiresAt, "grace_days", lic.GraceDays)
	default:
		slog.Error("license invalid; refusing to start (fail closed)", "error", err)
		return 31
	}
	return 0
}

func runGenkey() int {
	plain, hash, err := gwcore.GenerateVirtualKey()
	if err != nil {
		fmt.Fprintf(os.Stderr, "key generation failed: %v\n", err)
		return 1
	}
	// 原文は一度きりの表示。設定へはハッシュのみ書く。
	fmt.Printf("virtual key (share securely, shown once):\n  %s\n\n", plain)
	fmt.Printf("gateway.yaml entry:\n  - id: vk-<name>\n    name: <purpose>\n    key_hash: %s\n", hash)
	return 0
}

// runAdminToken issues the admin-UI credential(原文は一度きりの表示、
// DataDir にはハッシュのみ保存。再実行で旧トークンは失効する)。
func runAdminToken() int {
	plain, hash, err := gwcore.GenerateAdminToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "admin token generation failed: %v\n", err)
		return 1
	}
	dataDir := dataDirDefault()
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "data dir create failed: %v\n", err)
		return 1
	}
	hashPath := filepath.Join(dataDir, gwcore.AdminTokenHashFile)
	if err := os.WriteFile(hashPath, []byte(hash+"\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "hash write failed: %v\n", err)
		return 1
	}
	fmt.Printf("admin token (share securely, shown once):\n  %s\n\n", plain)
	fmt.Printf("hash stored: %s\n", hashPath)
	fmt.Println("enable the UI with LYKURO_ADMIN_ENABLED=true (listen: LYKURO_ADMIN_LISTEN, default 127.0.0.1:9465)")
	return 0
}

func runValidate(args []string) int {
	path := configPath(args)
	if _, err := gwcore.LoadConfig(path); err != nil {
		fmt.Fprintf(os.Stderr, "invalid: %v\n", err)
		return 2
	}
	fmt.Printf("ok: %s\n", path)
	return 0
}

// runConfigImport applies a signed config envelope offline (Air-Gapped、
// BD §14.4/§22.1)。外部通信なしで署名検証→staging→LKG保存を行う。
// 反映は serve の再起動(LoadStoredConfig)で行われる。
func runConfigImport(args []string) int {
	var file string
	force := false
	for i, a := range args {
		if (a == "-file" || a == "--file") && i+1 < len(args) {
			file = args[i+1]
		}
		if a == "-force" || a == "--force" {
			force = true
		}
	}
	if file == "" {
		fmt.Fprintln(os.Stderr, "usage: private-gateway config import -file <signed-config.json> [-force]")
		return 2
	}
	// ローカル設定の gateway.id と突き合わせ、他Gateway向け設定の誤適用を防ぐ
	localCfg, err := gwcore.LoadConfig(configPath(args))
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load failed (gateway.yaml が必要です): %v\n", err)
		return 2
	}
	pubPath := os.Getenv("LYKURO_SIGNING_PUB_FILE")
	if pubPath == "" {
		fmt.Fprintln(os.Stderr, "LYKURO_SIGNING_PUB_FILE is not set (配布物同梱の lykuro-signing.pub を指定)")
		return 2
	}
	dataDir := os.Getenv("LYKURO_DATA_DIR")
	if dataDir == "" {
		dataDir = "/var/lib/lykuro/gateway"
	}
	pub, err := gwcore.LoadSigningPublicKey(pubPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verification key load failed: %v\n", err)
		return 20
	}
	envelope, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read failed: %v\n", err)
		return 2
	}
	version, cfg, err := gwcore.ImportSignedConfig(dataDir, pub, envelope, localCfg.Gateway.ID, force)
	if err != nil {
		fmt.Fprintf(os.Stderr, "import rejected: %v\n", err)
		return 20 // ADD §11.6: 署名・チェックサム不正
	}
	fmt.Printf("imported config version %d (gateway %s, models %d)\n",
		version, cfg.Gateway.ID, len(cfg.Models))
	fmt.Println("restart the gateway service to apply")
	return 0
}

// runRegister performs the one-time control plane registration (BD §13.1)。
func runRegister(args []string) int {
	cfg, err := gwcore.LoadConfig(configPath(args))
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load failed: %v\n", err)
		return 2
	}
	st, ok := agentSettings()
	if !ok {
		fmt.Fprintln(os.Stderr, "LYKURO_CONTROL_PLANE_URL is not set")
		return 2
	}
	// serve 前提のダミーサーバー(登録のみに使用)
	audit, _ := gwcore.NewAuditLogger("")
	agent, err := gwcore.NewAgent(st, gwcore.NewServer(cfg, audit))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent init failed: %v\n", err)
		return 2
	}
	if agent.Registered() {
		fmt.Println("already registered")
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := agent.Register(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "registration failed: %v\n", err)
		return 30 // ADD §11.6: 初回登録失敗
	}
	fmt.Println("registered")
	return 0
}

// runPrecheck は read-only の導入前検査(ADD §11.3)。OSを変更しない。
func runPrecheck(args []string) int {
	jsonOut := false
	for _, a := range args {
		if a == "--output=json" || a == "-output=json" {
			jsonOut = true
		}
	}
	type check struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
	}
	var checks []check
	add := func(name string, ok bool, detail string) {
		checks = append(checks, check{Name: name, OK: ok, Detail: detail})
	}

	add("os_arch", true, runtime.GOOS+"/"+runtime.GOARCH)

	cfgPath := configPath(args)
	cfg, err := gwcore.LoadConfig(cfgPath)
	if err != nil {
		add("config", false, err.Error())
	} else {
		add("config", true, cfgPath)
		// Runtime 到達性
		client := &http.Client{Timeout: 3 * time.Second}
		for _, m := range cfg.Models {
			resp, rerr := client.Get(m.Endpoint + "/v1/models")
			if rerr != nil {
				add("runtime:"+m.LogicalName, false, rerr.Error())
			} else {
				resp.Body.Close()
				add("runtime:"+m.LogicalName, true, m.Endpoint)
			}
		}
	}

	// データディレクトリ書込可否
	if st, ok := agentSettings(); ok {
		if err := os.MkdirAll(st.DataDir, 0o700); err != nil {
			add("data_dir", false, err.Error())
		} else if f, err := os.CreateTemp(st.DataDir, ".precheck-*"); err != nil {
			add("data_dir", false, err.Error())
		} else {
			f.Close()
			os.Remove(f.Name())
			add("data_dir", true, st.DataDir)
		}
		// 制御プレーン到達性(HTTPS 443 outbound)
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(st.ControlPlaneURL + "/health")
		if err != nil {
			add("control_plane", false, err.Error())
		} else {
			resp.Body.Close()
			add("control_plane", true, st.ControlPlaneURL)
		}
		if st.SigningPubFile != "" {
			if _, err := os.Stat(st.SigningPubFile); err != nil {
				add("signing_pub", false, err.Error())
			} else {
				add("signing_pub", true, st.SigningPubFile)
			}
		}
	} else {
		add("control_plane", true, "not configured (standalone mode)")
	}

	failed := 0
	for _, c := range checks {
		if !c.OK {
			failed++
		}
	}
	if jsonOut {
		out, _ := json.MarshalIndent(map[string]any{"checks": checks, "ok": failed == 0}, "", "  ")
		fmt.Println(string(out))
	} else {
		for _, c := range checks {
			mark := "ok  "
			if !c.OK {
				mark = "FAIL"
			}
			fmt.Printf("[%s] %-24s %s\n", mark, c.Name, c.Detail)
		}
	}
	if failed > 0 {
		return 10 // ADD §11.6: 前提不足
	}
	return 0
}

// runDiscover scans for local LLM runtimes (read-only。ADD precheck と同系)。
// 発見結果は表示のみで、取込は管理画面か gateway.yaml の手編集で行う。
func runDiscover(args []string) int {
	var cidr string
	jsonOut := false
	for i, a := range args {
		if (a == "-cidr" || a == "--cidr") && i+1 < len(args) {
			cidr = args[i+1]
		}
		if a == "--output=json" || a == "-output=json" {
			jsonOut = true
		}
	}
	hosts, err := gwcore.DiscoverHosts(cidr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "discover: %v\n", err)
		return 2
	}
	// 設定が読めれば登録済み endpoint を突合する(読めなくても走査は続行)
	configured := map[string]bool{}
	if cfg, cerr := gwcore.LoadConfig(configPath(args)); cerr == nil {
		configured = gwcore.ConfiguredEndpoints(cfg)
	} else {
		fmt.Fprintf(os.Stderr, "note: config not loaded (%v); all candidates shown as unregistered\n", cerr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	candidates := gwcore.DiscoverRuntimes(ctx, hosts, configured)

	if jsonOut {
		out, _ := json.MarshalIndent(map[string]any{
			"hosts_scanned": len(hosts), "candidates": candidates}, "", "  ")
		fmt.Println(string(out))
		return 0
	}
	if len(candidates) == 0 {
		fmt.Printf("no runtimes found (%d hosts scanned)\n", len(hosts))
		return 0
	}
	for _, c := range candidates {
		state := "new"
		if c.Configured {
			state = "configured"
		}
		fmt.Printf("[%s] %-28s %-18s models: %s\n", state, c.Endpoint, c.Runtime, strings.Join(c.Models, ", "))
	}
	fmt.Println("\ngateway.yaml snippet for new endpoints:")
	for _, c := range candidates {
		if c.Configured {
			continue
		}
		physical := "<physical-model>"
		if len(c.Models) > 0 {
			physical = c.Models[0]
		}
		fmt.Printf("  - logical_name: <name>\n    runtime: %s\n    endpoint: %s\n    physical_model: %s\n",
			c.Runtime, c.Endpoint, physical)
	}
	return 0
}

// localBase resolves the local API address from config.
func localBase(args []string) (string, error) {
	cfg, err := gwcore.LoadConfig(configPath(args))
	if err != nil {
		return "", err
	}
	listen := cfg.Gateway.Listen
	if strings.HasPrefix(listen, ":") {
		listen = "127.0.0.1" + listen
	}
	return "http://" + listen, nil
}

func runStatus(args []string) int {
	base, err := localBase(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load failed: %v\n", err)
		return 2
	}
	client := &http.Client{Timeout: 5 * time.Second}
	for _, path := range []string{"/healthz", "/readyz", "/version"} {
		resp, err := client.Get(base + path)
		if err != nil {
			fmt.Printf("%-10s unreachable (%v)\n", path, err)
			return 41
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Printf("%-10s %d %s\n", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return 0
}

// runDiagnose collects a support bundle (secret・本文なし、ADD §17.2)。
func runDiagnose(args []string) int {
	base, err := localBase(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load failed: %v\n", err)
		return 2
	}
	client := &http.Client{Timeout: 5 * time.Second}
	bundle := map[string]any{
		"version":  gwcore.Version,
		"os_arch":  runtime.GOOS + "/" + runtime.GOARCH,
		"time_utc": time.Now().UTC().Format(time.RFC3339),
	}
	for name, path := range map[string]string{"healthz": "/healthz", "readyz": "/readyz"} {
		resp, err := client.Get(base + path)
		if err != nil {
			bundle[name] = map[string]string{"error": err.Error()}
			continue
		}
		var v any
		_ = json.NewDecoder(resp.Body).Decode(&v)
		resp.Body.Close()
		bundle[name] = map[string]any{"status": resp.StatusCode, "body": v}
	}
	// env は「設定されているか」のみ(値は含めない)
	envPresence := map[string]bool{}
	for _, k := range []string{"LYKURO_CONTROL_PLANE_URL", "LYKURO_DATA_DIR",
		"LYKURO_SIGNING_PUB_FILE", "LYKURO_INSTALL_TOKEN_FILE", "LYKURO_CONFIG_PATH"} {
		envPresence[k] = os.Getenv(k) != ""
	}
	bundle["env_present"] = envPresence

	out, _ := json.MarshalIndent(bundle, "", "  ")
	name := fmt.Sprintf("lykuro-diagnose-%s.json", time.Now().UTC().Format("20060102T150405Z"))
	if err := os.WriteFile(name, out, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "write bundle failed: %v\n", err)
		return 1
	}
	fmt.Printf("diagnose bundle written: %s\n", name)
	return 0
}
