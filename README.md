# Lykuro Native LLM Platform

Customer-side Private LLM Gateway for [Lykuro](https://lykuro.ai) — 顧客環境(オンプレミス / VPC / 端末)で稼働し、ローカルLLM Runtime(vLLM / Ollama / TGI / OpenAI互換)を OpenAI 互換 API として提供するゲートウェイです。管理・ポリシー配信・ライセンスは Lykuro SaaS(app.lykuro.ai)の企業コンソールから行います。

- 対応OS: **Linux / macOS / Windows**(amd64 / arm64、Windows は amd64)
- 配布形態: ネイティブバイナリ([Releases](https://github.com/lykuroai/Native-LLM-Platform/releases))/ ソースビルド / Docker / Helm
- プロンプト本文・レスポンス本文は保存しません(Zero-Retention)

## インストール

### 1. バイナリの取得

[Releases](https://github.com/lykuroai/Native-LLM-Platform/releases) から自OSのバイナリと `checksums.txt` を取得し、SHA-256 を突合してください。ソースからのビルドは `make build`(Go 1.26+)。

### 2. 設定

`config/gateway.example.yaml` を `gateway.yaml` へコピーし、Gateway ID(企業コンソールで作成)と Runtime エンドポイントを設定します。

### 3. 初回登録トークン

app.lykuro.ai の Gateway 詳細画面で発行したトークン(有効期限24時間・1回限り)を `install-token.txt` へ保存します。shell 引数・環境変数へ直接書かないでください。

### 4. サービスとして起動

**Linux (systemd)**

```bash
sudo install -D -m 0755 private-gateway_<ver>_linux_amd64 /opt/lykuro/private-gateway/bin/private-gateway
sudo install -D -m 0640 gateway.yaml /etc/lykuro/gateway/gateway.yaml
sudo install -D -m 0600 install-token.txt /etc/lykuro/gateway/install-token.txt
sudo cp deploy/native/lykuro-private-gateway.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now lykuro-private-gateway
```

**macOS (launchd)**

```bash
sudo install -d /opt/lykuro/private-gateway/bin /etc/lykuro/gateway /var/log/lykuro
sudo install -m 0755 private-gateway_<ver>_darwin_arm64 /opt/lykuro/private-gateway/bin/private-gateway
sudo install -m 0640 gateway.yaml /etc/lykuro/gateway/gateway.yaml
sudo install -m 0600 install-token.txt /etc/lykuro/gateway/install-token.txt
sudo cp deploy/native/ai.lykuro.private-gateway.plist /Library/LaunchDaemons/
sudo launchctl bootstrap system /Library/LaunchDaemons/ai.lykuro.private-gateway.plist
```

**Windows (管理者 PowerShell)**

```powershell
New-Item -ItemType Directory -Force "C:\Program Files\Lykuro\PrivateGateway", "C:\ProgramData\Lykuro\Gateway"
Copy-Item private-gateway_<ver>_windows_amd64.exe "C:\Program Files\Lykuro\PrivateGateway\private-gateway.exe"
Copy-Item gateway.yaml "C:\ProgramData\Lykuro\Gateway\gateway.yaml"
Copy-Item install-token.txt "C:\ProgramData\Lykuro\Gateway\install-token.txt"
powershell -ExecutionPolicy Bypass -File deploy\native\install-task.ps1
```

**Docker Compose** — `deploy/docker-compose.example.yaml` 参照(`--build` でこのリポジトリからビルド)。

**Kubernetes (Helm)** — `deploy/helm/lykuro-private-gateway/` 参照。

起動後、app.lykuro.ai の Gateway 詳細で接続状態が online になることを確認してください。

## CLI

```
private-gateway serve  -config /etc/lykuro/gateway/gateway.yaml
private-gateway precheck            # 前提条件チェック
private-gateway status              # 稼働状態
private-gateway diagnose            # 診断バンドル
private-gateway genkey              # Virtual Key 発行(ハッシュ表示)
private-gateway config validate -config <path>
private-gateway upgrade -to <version> -file compose/docker-compose.yml   # compose 運用のみ
private-gateway version
```

ネイティブバイナリのアップグレードは、新バージョンを取得・検証のうえ、サービス停止 → バイナリ差替 → サービス起動で行います(旧バイナリを `.bak` として残すと即時ロールバックできます)。

## リポジトリ構成

| パス | 内容 |
|------|------|
| `cmd/private-gateway/` | CLI エントリポイント |
| `gwcore/` | ゲートウェイ本体(認証・プロキシ・設定・メトリクス・SaaS Agent) |
| `platform/` | Native LLM Platform 統合(contract / orchestrator / memory ほか) |
| `sign/` | Ed25519 署名・ライセンス・正規形JSON(SaaS 側と共有) |
| `token/` | トークン生成・ハッシュ(SaaS 側と共有) |
| `deploy/` | systemd / launchd / Windows / Compose / Helm |

## License

[Apache License 2.0](LICENSE)
