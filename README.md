# Lykuro Native LLM Platform

Customer-side Private LLM Gateway for [Lykuro](https://lykuro.ai) — 顧客環境(オンプレミス / VPC / 端末)で稼働し、ローカルLLM Runtime([Lykuro Native Inference Engine](https://github.com/lykuroai/engine) / vLLM / Ollama / TGI / OpenAI互換)を OpenAI 互換 API として提供するゲートウェイです。単体で完結して動作し、管理はバイナリ埋め込みの管理画面と CLI で行います(Lykuro SaaS への接続は任意機能)。

- 対応OS: **Linux / macOS / Windows**(amd64 / arm64、Windows は amd64)
- 配布形態: ネイティブバイナリ([Releases](https://github.com/lykuroai/Native-LLM-Platform/releases))/ ソースビルド / Docker / Helm
- プロンプト本文・レスポンス本文は保存しません(Zero-Retention)

## インストール

### 1. バイナリの取得

**インストールスクリプト**(取得・checksum 検証・配置のみ。サービス登録・常駐化はしません):

```bash
# macOS / Linux
curl -fsSL https://raw.githubusercontent.com/lykuroai/Native-LLM-Platform/main/deploy/install.sh | bash
```

```powershell
# Windows(PowerShell)— curl.exe 明示(curl は Invoke-WebRequest の別名のため)
curl.exe -fsSLO https://raw.githubusercontent.com/lykuroai/Native-LLM-Platform/main/deploy/install.bat; .\install.bat
```

```bat
:: Windows(コマンドプロンプト)
curl -fsSLO https://raw.githubusercontent.com/lykuroai/Native-LLM-Platform/main/deploy/install.bat && install.bat
```

手動の場合は [Releases](https://github.com/lykuroai/Native-LLM-Platform/releases) から自OSのバイナリと `checksums.txt` を取得し、SHA-256 を突合してください。ソースからのビルドは `make build`(Go 1.26+)。

### 2. 設定

稼働中の Runtime(Lykuro Native Inference Engine / vLLM / Ollama / TGI 等)を検出して設定を自動生成します。Runtime が見つからなくてもモデル 0 件の有効な設定を生成し、モデルは後から管理画面の取込・`init --force` の再実行・手編集で追加できます:

```bash
private-gateway init -config gateway.yaml   # Runtime 検出 → gateway.yaml 生成 + Virtual Key 発行
```

ローカルホストの既知ポートを走査し、見つかったモデルごとの定義と Virtual Key
(原文は一度きり表示)を含む検証済みの設定を書き出します。`--cidr` で走査範囲の
指定、`--force` で既存ファイルの上書きができます。

手動で作る場合は `config/gateway.example.yaml` を `gateway.yaml` へコピーし、
Gateway ID(任意の識別子)と Runtime エンドポイントを設定します。起動後は
管理画面からも編集できます。

### 3. 起動

単一の実行ファイルをそのまま起動するだけです(サービス登録は不要。常駐化したい場合は systemd / launchd / タスクスケジューラ等、各環境の流儀で自由にラップしてください)。

**Linux / macOS**

```bash
chmod +x private-gateway_<ver>_<os>_<arch>
./private-gateway_<ver>_<os>_<arch> serve -config gateway.yaml
```

**Windows (PowerShell)** — zip を展開して実行:

```powershell
Expand-Archive private-gateway_<ver>_windows_amd64.zip .
.\private-gateway_<ver>_windows_amd64.exe serve -config gateway.yaml
```

> **Note**: Windows 版は zip で配布しています。zip の SHA-256 を checksums.txt と
> 突合のうえ展開してください。コード署名が無いため、初回実行時に SmartScreen の
> 警告(発行元実績のブロック)が出た場合は「詳細情報 → 実行」または
> `Unblock-File` で解除します。

### 4. ローカル管理画面(任意)

バイナリ埋め込みの Web 管理画面(概要・Virtual Key 管理・設定編集・監査ログ・
メトリクス)を使う場合は、トークンを発行してから有効化します。

```bash
private-gateway admin-token        # トークン発行(一度きり表示。ハッシュのみ保存)
LYKURO_ADMIN_ENABLED=true private-gateway serve -config gateway.yaml
# → http://127.0.0.1:9465 を開き、発行したトークン(lkpadm_…)でログイン
```

| 環境変数 | 値 |
|----------|-----|
| `LYKURO_ADMIN_ENABLED` | `true` で有効化(既定は無効) |
| `LYKURO_ADMIN_LISTEN` | 待受アドレス(既定 `127.0.0.1:9465`。loopback 外は警告) |

- トークン未発行のまま有効化しても管理画面は起動しません(Fail Closed)
- 設定編集は検証 → gateway.yaml への保存 → 即時反映。検証エラー時は反映されません
- Control Plane 接続時は署名済み設定世代が優先され、ローカル編集は次回配信で
  上書きされることがあります(画面上に警告を表示)
- 管理操作(設定反映・キー発行/無効化/削除・Runtime 取込)は監査ログに記録されます(本文なし)

#### Virtual Key の保存仕様

管理画面(または `genkey`)で発行した Virtual Key の**原文はどこにも保存されません**。
発行時のレスポンスで一度だけ表示されるため、その場で控えてください(紛失時は
削除して再発行します)。

- 永続化されるのは SHA-256 ハッシュのみで、保存先は `gateway.yaml` の
  `auth.virtual_keys[]`(`id` / `name` / `key_hash` / `allowed_models` / `rpm_limit`)です
- 保存は一時ファイル書き込み → rename のアトミック置換(パーミッション `0600`)で行われ、
  即時ホットリロードされます
- 認証はリクエストの Bearer キーをハッシュ化して `key_hash` と定数時間比較する方式で、
  DB や別の資格情報ファイルは存在しません(設計原則「DBなし」)
- 「Runtime 検出」タブでローカルホスト(または明示指定した CIDR、最大 /22)の
  既知ポートを走査し、発見した Lykuro Native Inference Engine / vLLM / Ollama / TGI 等を承認操作(取込)で設定へ
  追加できます。**取込するまで発見済み Runtime へは一切接続しません**

### 5. デプロイ(任意)

**Docker Compose** — `deploy/docker-compose.example.yaml` 参照(`--build` でこのリポジトリからビルド)。

**Kubernetes (Helm)** — `deploy/helm/lykuro-private-gateway/` 参照。

## 社内ローカルLLM の安全な外部公開

社内 GPU サーバで動かしている Ollama / vLLM などの Runtime を、リモートワーク環境・他拠点・社外アプリケーションへ OpenAI 互換 API として公開する構成です。Runtime 本体は一切公開せず、Gateway だけを公開点にします。

```
[社外クライアント] ──HTTPS──> [リバースプロキシ(TLS終端)] ──> [private-gateway :8443] ──> [Ollama / vLLM(社内のみ)]
```

Gateway が公開経路で提供する保護:

- **Virtual Key 認証** — すべての API リクエストに Bearer キー(`lkpgw_…`)を必須化。設定ファイルには SHA-256 ハッシュのみを保存し、キー単位で発行・失効できます(`genkey` / 管理画面)
- **キー単位のレート制限** — Virtual Key ごとの RPM 上限で過剰利用・暴走クライアントを抑止
- **Zero-Retention** — プロンプト/レスポンス本文はログ・監査・メトリクスのどこにも残りません
- **監査ログ** — 誰のキーがいつどのモデルを呼んだか(本文なし)を JSONL で記録

構成手順:

1. Runtime(Ollama / vLLM 等)は loopback または社内ネットワークのみで待ち受けさせ、ファイアウォールで外部から直接到達できないことを確認します
2. `gateway.yaml` の `listen` を必要な待受(例 `":8443"`)にし、公開するモデルだけを `models` に定義します
3. 利用者・アプリごとに `private-gateway genkey` で Virtual Key を発行して配布します(原文キーは一度きり表示)
4. Gateway は TLS 終端を内蔵しないため、外部公開時は必ずリバースプロキシ(nginx / Caddy 等)・ロードバランサ・トンネルで **HTTPS を終端**し、Gateway へ転送します。生の HTTP をインターネットへ直接公開しないでください
5. 管理画面(`LYKURO_ADMIN_ENABLED`)は既定どおり loopback に限定したまま運用し、外部へは公開しません

## 管理コンソール(SaaS)接続(任意機能)

Gateway は単体で完結して動作します。複数拠点の一元管理・署名済み設定配信を
使いたい場合のみ、以下の環境変数で Control Plane へ接続します(未設定なら
スタンドアロン動作。接続時は配信された署名済み設定世代がローカル編集を
上書きすることがあります)。

| 環境変数 | 値 |
|----------|-----|
| `LYKURO_CONTROL_PLANE_URL` | Control Plane のベースURL |
| `LYKURO_DATA_DIR` | Agent資格情報・設定世代の保存先(既定 `/var/lib/lykuro/gateway`) |
| `LYKURO_SIGNING_PUB_FILE` | 配信設定の署名検証用 Ed25519 公開鍵 |
| `LYKURO_INSTALL_TOKEN_FILE` | 初回登録トークンのファイルパス |
| `LYKURO_HEARTBEAT_INTERVAL_SECONDS` | 任意(既定 60) |
| `LYKURO_CONFIG_POLL_INTERVAL_SECONDS` | 任意(既定 300) |

初回登録トークンは管理コンソールの Gateway 詳細画面で発行し(有効期限24時間・
1回限り)、ファイルへ保存して `LYKURO_INSTALL_TOKEN_FILE` で渡します。shell
引数・環境変数へ直接書かないでください。登録完了後は削除できます。

## CLI

```
private-gateway init [-config <path>] [--cidr <range>] [--force]   # 設定の自動生成
private-gateway serve  -config /etc/lykuro/gateway/gateway.yaml
private-gateway precheck            # 前提条件チェック
private-gateway discover [--cidr 192.168.1.0/24]   # Runtime 検出(read-only、最大 /22)
private-gateway status              # 稼働状態
private-gateway diagnose            # 診断バンドル
private-gateway genkey              # Virtual Key 発行(ハッシュ表示)
private-gateway admin-token         # 管理画面トークン発行(再実行で旧トークン失効)
private-gateway config validate -config <path>
private-gateway upgrade -to <version> -file compose/docker-compose.yml   # compose 運用のみ
private-gateway version
```

アップグレードは新バージョンを取得・検証のうえ、プロセス停止 → バイナリ差替 → 再起動で行います(旧バイナリを `.bak` として残すと即時ロールバックできます)。

## リポジトリ構成

| パス | 内容 |
|------|------|
| `cmd/private-gateway/` | CLI エントリポイント |
| `gwcore/` | ゲートウェイ本体(認証・プロキシ・設定・メトリクス・SaaS Agent) |
| `platform/` | Native LLM Platform 統合(contract / orchestrator / memory ほか) |
| `sign/` | Ed25519 署名・ライセンス・正規形JSON(安定公開API) |
| `token/` | トークン生成・ハッシュ(安定公開API) |
| `deploy/` | Docker Compose / Helm(任意。CLI 単体でも動作) |

## License

[PolyForm Noncommercial License 1.0.0](LICENSE)

非商用利用(個人利用・研究・教育・評価・非営利組織での利用など)は無料です。
商用利用には別途商用ライセンスが必要です。商用ライセンスについてはお問い合わせください。

なお、v0.6.0 以前に Apache License 2.0 で公開されたバージョンは、引き続き同ライセンスの下で利用できます(ライセンス変更は遡及しません)。
