# Private LLM Gateway — Phase 0 既存リポジトリ調査報告

| 項目 | 内容 |
|---|---|
| 対象文書 | LYK-PLG-BD-001 v1.1 / LYK-PLG-ADD-001 v1.0 |
| 調査日 | 2026-08-06 |
| 状態 | 調査完了・実装未着手（本報告のレビュー待ち） |

---

## 1. 技術スタック調査結果

| 領域 | 実態 |
|---|---|
| バックエンド | Go 1.22+ / chi / pgx / go-redis(Dragonfly) / franz-go(Redpanda)。モジュール名 `github.com/lykuro/gateway` |
| フロント | `web/`（顧客ダッシュボード、Next.js 15 App Router + TanStack Query v5、BFF `/api/proxy/[...path]` 経由）と `web-admin/`（社内管理、サービス名 `admin-web`） |
| ワーカー | `cmd/worker`（Redpanda consumer、トピックは `billing.consumption_events` 1本ハードコード）＋ `cmd/batch`（systemd timer 起動のワンショット群）。DBジョブキューは無し |
| DB | PostgreSQL（golang-migrate、`schema/NNN_*.up/down.sql`、**最新 033・次は 034**）＋ ClickHouse（自作 `chmigrate`、前進のみ） |
| オブジェクトストレージ | S3 は `cmd/batch/backup` のバックアップ用途のみ。presign / アップロード / ダウンロード API は**無し** |
| OCI Registry / 署名鍵 / KMS | **一切無し**（cosign・sigstore・KMS SDK 依存なし）。既存暗号は `internal/mcp/crypto.go` の AES-256-GCM（`MCP_MASTER_KEY` env 直読み）1箇所 |
| CI | `.github/workflows/ci.yml` 1本。必須ゲートは `go build` / `go vet` / web `tsc` + vitest のみ。integration（Testcontainers, `//go:build integration`）・Playwright・k6 は CI 未実行 |

## 2. 設計書の論理名 → 既存実装の対応表

| 設計書の論理名 | 既存実装 | 備考 |
|---|---|---|
| tenant | `tenants`（schema/015）/ `model.Tenant` | plan: personal/team/business/enterprise |
| organization | **存在しない** | 階層は tenants → departments → projects |
| project | `projects` / `model.Project` | あり |
| user / role | `accounts` + `tenant_memberships` / `model.TenantRole` | ロール: `owner, admin, dept_admin, developer, viewer, billing, auditor` |
| Virtual Key | `virtual_keys`（schema/016）/ `model.VirtualKey` | **本設計の「Gateway用Virtual Key」とは別物**（後述 4.2） |
| Tenant Owner / AI Administrator | `owner` / `admin` | マッピングで対応 |
| Security Auditor | `auditor` | あり |
| Billing Viewer | `billing` | あり |
| Project Manager | 最も近いのは `dept_admin` | project 単位の管理ロールは無し |
| 企業ユーザー判定 | `tenant_memberships` の有無（accounts に is_enterprise フラグ無し）＋ env `ENTERPRISE_ENABLED=true`（`server.go:205` で企業ルート登録を丸ごとゲート） | フロントのナビ出し分けは**無条件表示**（要改修余地） |
| RBAC 実施 | ミドルウェア無し。各ハンドラ冒頭で `TenantHandler.requireMembership` + `Role.CanManageTenant()` 等 | 新APIも同規約に従う |
| feature flag | env 汎用機構は無し。**plan_entitlements（DB駆動）** が正 | Private Gateway も `FeaturePrivateGateway` 的な feature_code を追加するのが自然 |
| 監査（企業版） | `enterprise_audit_logs`（schema/020）/ `governance_repository_impl.go RecordAudit()` | 本文なし・ハッシュのみ。best-effort 書き込み |
| 管理APIパス | `/api/enterprise/...` ではなく **`/api/tenants/{tenantID}/...`** | 新規は `/api/tenants/{tenantID}/private-gateways/...` が既存規約 |
| 画面ルート | `/enterprise/private-gateways` ではなく **`web/src/app/(dashboard)/enterprise/[tenantId]/private-gateways/`** | ハブは `[tenantId]/page.tsx` のカード群 |
| エラー形式 | 2系統混在: `errs.WriteError` → `{"error":{code,message}}`（/v1系）と handler ローカル `writeError` → `{"error":{type,code,message}}`（企業版） | 企業版は後者に合わせる。設計書のエラー形式（type/code/message/request_id）とほぼ一致するが request_id は現状未出力 |
| ページネーション | 規約ほぼ無し（usage_handler にカーソル1箇所のみ、企業版は全件返し） | 新API用に方針決定が必要 |
| Idempotency-Key | HTTPヘッダ対応は**リポジトリ全体でゼロ** | 新規実装（設計書 §8.3/15.3 要求） |
| Package Builder Worker | 該当基盤なし | 新トピック＋`internal/worker/<domain>/` 追加、または新バイナリ |
| Config Service / License Service / Control Agent | すべて新規 | — |

## 3. 完全新規となる領域（既存資産ゼロ）

1. **配布基盤一式**: パッケージ生成ジョブ、S3 等への成果物保管、短期ダウンロードURL（presign）、ダウンロード監査
2. **署名基盤**: 署名鍵管理（KMS/HSM 無し）、manifest/checksums 署名、OCI イメージ署名、SBOM 生成
3. **顧客環境側の成果物**: Private Gateway バイナリ（新 cmd/）、Control Agent、`lykuro-gateway` CLI、install.sh、Compose/Helm テンプレート
4. **Agent API**（register / config / heartbeat / usage）と install token・config 世代管理
5. **Idempotency-Key ミドルウェア**

## 4. リスク・注意点

### 4.1 「Gateway」の名前衝突（大）
既に3義で使用中: ①Goモジュール名 `github.com/lykuro/gateway`、②SaaS本体（`cmd/gateway`, `internal/gateway/`, `Dockerfile.gateway`, compose の `gateway` サービス）、③企業版機能「MCPゲートウェイ」（`FeatureMCPGateway`）。
→ 新機能のパッケージ名は `internal/privategw`（または `plg`）等の別名が必須。新バイナリも `cmd/private-gateway` / `cmd/pgw-agent` 等。DBテーブル `gateway_deployments` 等は現状衝突なしだが、`private_gateway_deployments` のように接頭辞を付ける方が安全（要決定）。

### 4.2 Virtual Key の二重定義
既存 `virtual_keys` は SaaS 側（api_keys 拡張）の企業キー。設計書の Virtual Key は**顧客環境内 Gateway が自前で発行・検証するキー**で、DB も検証経路も別。同名のまま両立させると混乱するため、命名（例: `pgw_virtual_keys` / 顧客側ローカルDB）と関係整理が必要。

### 4.3 その他
- `ENTERPRISE_ENABLED` は `os.Getenv` 直読み・`.env.example` 未記載・本番は `/data/lykuro/src/.env`（compose 補間）で制御。Private Gateway 管理APIも同ブロック内に登録するのが一貫。
- `enterprise_audit_logs` の INSERT は `ip_hash`/`user_agent_hash` を書いていない、tenant管理系操作（key発行等）の監査が未記録 — 設計書 §19 の監査要件を満たすには既存側の拡充も必要。
- 顧客環境側 Gateway は本リポジトリの SaaS 前提（Dragonfly/Redpanda/ClickHouse 依存）をそのまま持ち込めない。顧客側は SQLite/組込ストア等の軽量構成が必要 → 技術選定の未決事項。
- 監視: heartbeat はDBテーブルより既存 Prometheus/Grafana 基盤優先（設計書 §18.6 の指示どおり）。ただし Control Plane 側での last_seen 管理は `gateway_deployments.last_seen_at` で可。

## 5. 未決事項（実装前に決定が必要）

| # | 論点 | 提案初期値 |
|---|---|---|
| Q-1 | 新パッケージ/バイナリ命名（`privategw`? `plg`?）とDBテーブル接頭辞 | `internal/privategw`, テーブル `pgw_*` |
| Q-2 | 顧客側 Gateway を本リポジトリ内 monorepo で持つか別リポジトリか（設計書 §24 は monorepo 例、既存は単一 Go モジュール） | 本リポジトリ内 `cmd/private-gateway` + `internal/privategw/`（CI で分離ビルド） |
| Q-3 | 顧客側の状態ストア（Postgres 前提にするか、SQLite/ファイルか） | MVP は SQLite または単一ファイル（要確認） |
| Q-4 | 署名鍵の管理方式（AWS KMS 新規導入 vs 既存 secret file 方式で開始） | PoC は secret file、Enterprise 前に KMS |
| Q-5 | パッケージ保管先（既存バックアップ用 S3 バケット流用 vs 専用バケット） | 専用バケット + presigned URL(15分) |
| Q-6 | feature gate（plan_entitlements に `private_gateway` を追加 + `ENTERPRISE_ENABLED` 配下） | 両方 |
| Q-7 | Idempotency-Key 実装スコープ（Private Gateway 管理APIのみ vs 全企業API） | まず Private Gateway 管理APIのみ |
| Q-8 | 設計書 D-01〜D-09（初期モデル、Runtime、SLA 等の事業判断） | ユーザー決定待ち |

## 6. テスト実行方法（既存）

- ユニット: `make test-unit`（`/opt/lykuro/config/models.yaml` 絶対パス依存に注意）
- 統合: `make test-int`（Testcontainers、`-tags integration`）
- フロント: `web/` で vitest / Playwright（`npm run test` / `npx playwright test`）
- CI 必須ゲート: `go build` / `go vet` / `tsc --noEmit` / web vitest

## 7. 推奨実装順（設計書 Phase 1〜 への適合）

1. **Phase 1**: `schema/034_private_gateway.up/down.sql`（`pgw_deployments` / `pgw_packages` / `pgw_install_tokens` / `pgw_config_versions` / `pgw_download_events`、全テーブル tenant_id スコープ）＋ `internal/model/private_gateway.go` ＋ repository ＋ `/api/tenants/{tenantID}/private-gateways` CRUD（requireMembership 規約 + enterprise_audit_logs 記録 + Idempotency-Key）
2. **Phase 2**: `web/src/app/(dashboard)/enterprise/[tenantId]/private-gateways/` 一覧・作成・詳細・インストール画面（`my_role` 分岐、entitlement ゲート）
3. **Phase 3**: Package Builder（Redpanda 新トピック + `internal/worker/pkgbuild/`、S3 保管、checksums/署名/SBOM、短期DL URL）
4. **Phase 4以降**: 顧客側 Gateway MVP / Agent / CLI / オフライン（設計書どおり）
