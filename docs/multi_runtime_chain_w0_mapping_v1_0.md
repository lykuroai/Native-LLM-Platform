# マルチRuntime連鎖推論 — W0 読み替え差分表(本リポジトリ適用版)

| 項目 | 内容 |
|---|---|
| 文書番号 | LYK-NLP-MRCI-002 |
| 版 | v1.0 |
| 制定日 | 2026-08-16 |
| 文書種別 | W0 成果物(Task W0-01 差分表・Component Mapping) |
| 対象文書 | LYK-NLP-MRCI-001「マルチRuntime連鎖推論・Workflow Orchestrator 追加拡張仕様書」v1.1 |
| 適用先 | 本リポジトリ(Private LLM Gateway、単一バイナリ) |
| 採用方針 | 案A: 単一バイナリ・DBなし・シングルテナントの枠内へ縮退適用 |

本書は v1.1 仕様書を本リポジトリの設計原則(単一バイナリ / DBなし / Zero-Retention / 依存 chi・yaml.v3・testify のみ)へ読み替えるための差分表である。v1.1 と本書が衝突する場合は **本書と CLAUDE.md を優先**する。

---

## 1. 前提の読み替え(基本方針)

| v1.1 の前提 | 本リポジトリでの読み替え |
|---|---|
| 親文書 LYK-NLP-SD-001 の統合基盤が実装済み | 本リポジトリの `gwcore/` + `platform/` を「実装済み基盤」とみなす |
| DB(PostgreSQL 等)+ Migration | **DBなし**。永続化はローカルファイルのみ(§6) |
| マルチテナント(tenant_id / project_id) | **シングルテナント**。tenant/project 列・分離要件はすべて対象外 |
| RBAC Scope(workflow:*) | Virtual Key(データプレーン)+ admin-token(管理プレーン)の2層へ集約(§8) |
| Distributed Coordinator / Node Agent / llm_nodes | 存在しない。`platform/pool` の Node スコアリング(`pool.go:147 Order`)を「Scheduler」とみなす |
| Enterprise SPA(app.lykuro.ai 画面再利用) | **不採用**。バイナリ埋め込み管理画面(`gwcore/adminui/index.html`)への最小限のタブ追加のみ |
| 独立 Service / 水平拡張 / Worker Lease | 単一プロセス内 Component。Lease 不要、プロセス内排他で代替(§10) |
| Control Plane 連携(Metadata/課金集計) | 任意アドオンのまま。Workflow 関連データは一切送信しない(既存方針維持) |

## 2. 用語・Entity 読み替え表

| v1.1 用語 | 本リポジトリでの実体 |
|---|---|
| Private Gateway | `gwcore/server.go` + `platform_adapter.go` |
| Model Manager / Logical Model | `platform/modelmanager`(`ModelEntry`、`Resolve`) |
| Runtime Connector | `platform/connector.OpenAICompatible` |
| Native Engine | `platform/enginecontract`(現状 mock のみ) |
| Distributed Coordinator / Scheduler | `platform/pool`(`Order` / `Acquire` / sticky) |
| LLM Pool | `platform.pools[]` を新設(Deployment ID の論理集合。gateway.yaml で定義、Fail Closed 検証) |
| Node | `pool.Node` = Deployment 単位 |
| inference_jobs(Step Attempt 記録) | 監査 JSONL(`gwcore/audit.go`)+ Run ファイル内の Attempt 記録。専用 Table なし |
| Conversation Memory | `platform/memory`(AES-256-GCM 暗号化ファイル) |
| Runtime Target(logical_model + pool) | `runtime_target.logical_model` + 任意 `pool_id`(候補 Deployment を Pool 所属分へ絞り込み)。固定 Runtime 指定は W1 対象外 |
| Route Decision | Run ファイル内の軽量 JSON(候補数・選択 DeploymentID・fallback 有無・latency)。専用 Table なし |
| Idempotency-Key | 既存 `contract.Request.IdempotencyKey`(現状未消費)を Workflow Run 作成で初めて消費する |

## 3. v1.1 章別 採否差分表

凡例: **採用**=ほぼそのまま / **読み替え**=概念維持・実装方式変更 / **縮退**=範囲を狭めて採用 / **対象外**=実装しない

| v1.1 章 | 採否 | 内容 |
|---|---|---|
| §0 絶対条件 | 採用 | 第三者Runtime非同梱・未承認Cloud非Fallback・Control Plane非送信・公開Version不変・Template安全制約はすべて維持 |
| §3 目的・連鎖方式 CH-01〜05 | 採用 | W1 の中核。CH-06〜08 は W2/W3 |
| §4.1 W1 必須機能 | 縮退 | 15項目中、Tenant分離・RBAC細分・Flow Designer UI を除き採用(§5, §9) |
| §5 アーキテクチャ | 読み替え | Orchestrator は `platform/workflow` パッケージとして単一プロセス内に追加。`contract.Backend`(= `platform.Platform`)を Step 実行に再利用 |
| §6 Component | 読み替え | Flow Manager / Workflow Runner / Safe Template Engine / Run Store を新設。Flow Designer UI・Monitor UI は管理画面タブへ縮退 |
| §7 Flow Definition | 採用 | JSON スキーマはほぼそのまま。`pool_id` / `network_zone` / `conversation_mode=CONVERSATION` は W1 対象外 |
| §8 実行仕様 | 採用 | Sequential・Handoff(TEXT/STRUCTURED)・擬似Codeの制御構造を踏襲。REF/SUMMARY Handoff は W2 以降 |
| §9 Retry/Fallback | 採用 | Step 再試行(max_attempts、backoff)と `fallback_policy.allowed_pool_ids`(承認済み Pool のみ)を採用。Pool は `platform.pools[]` として新設。Timeout 非Failover 原則(`orchestrator.go:262`)を Workflow 層でも維持 |
| §10 状態・Event | 縮退 | Run/Step 状態機械と Event 種別は採用。`WAITING_FOR_MODEL` / `WAITING_FOR_CAPACITY` は統合。Event はメモリリング + 本文非含有 JSONL |
| §11 API | 読み替え | §7 参照。`/api/enterprise/*` → 管理プレーン `/api/workflows/*` |
| §12 Data Model / Migration | 読み替え | DB 全面不採用。ファイルレイアウトへ読み替え(§6)。Migration Test → ファイル互換 Test |
| §13 UI | 縮退 | Canvas Designer・Wizard・Virtualized Table・1,000 Runtime 要件・WCAG SPA は対象外。管理画面に「Workflows」タブ(JSON 編集 + Validate + Publish + Run 一覧)を追加 |
| §14 Security/Retention | 縮退 | Strict Local・Data Class 継承・Secret 非出力は採用。Tenant 分離は対象外。Retention 既定: 本文 0 日(§6.3) |
| §15 Idempotency/Concurrency | 縮退 | Idempotency-Key・Draft ETag(revision)・Cancel は採用。Worker Lease は対象外(単一プロセス)。Recovery は縮退(§10) |
| §16 Audit/Metrics | 採用 | 既存 audit JSONL・手書き Prometheus(`gwcore/metrics.go`)へ Workflow 系列を追加。`content_logged: false` 維持 |
| §17 障害処理 | 採用 | Event Store 停止 → プロセス内のため「書込失敗時 Fail Closed」に読み替え |
| §18 非機能 | 縮退 | 水平拡張・SPOF 排除は対象外(単一バイナリ)。Scale 上限: Flow 100 / Version 50 / Step 20 / 同時 Run は `orchestrator.max_concurrent` に従属 |
| §19 Feature Flag | 読み替え | `platform.workflows.enabled` 等、gateway.yaml のキーへ(§7.4) |
| §20 Test | 縮退 | Unit / Fake Connector / IT-01〜17 のうち Tenant系(IT-13)を除き採用。UI Test は管理画面の範囲で縮退 |
| §21–22 受入/DoD | 縮退 | AT-MR-01〜17・20 を採用。18(1,000台)・19のTenant部・21〜24(UI)は縮退版で置換 |

## 4. 新規追加 Component と配置

| Component | 配置 | 責務 |
|---|---|---|
| Flow 定義・検証 | `platform/workflow/flowdef.go` | Flow JSON の Parse、Validation(step_id 一意・循環なし・Template 参照先存在・Logical Model 存在・上限チェック)。Fail Closed |
| Flow Store | `platform/workflow/store.go` | Draft/Version のファイル永続化(§6)。revision(ETag 相当)・checksum・公開後不変 |
| Safe Template Engine | `platform/workflow/template.go` | `{{inputs.x}}` `{{steps.id.output}}` 等の許可変数のみ展開。任意コード・環境変数・ファイル参照なし(自前実装、依存追加なし) |
| Workflow Runner | `platform/workflow/runner.go` | Sequential 実行・Handoff・Retry/Fallback・Cancel・状態遷移。Step 実行は `contract.Backend.Execute/ExecuteStream` を呼ぶだけ(候補選択・failover・pool・memory は既存を再利用) |
| Run Store / Event | `platform/workflow/run.go` | Run/Step/Attempt/Route Decision のメモリ管理 + メタデータ JSONL。SSE 用 Event リング(Last-Event-ID 再開) |
| Gateway 統合 | `gwcore/workflow_api.go`(新規) | `/v1/workflows/*` ルート、`model: "flow:{alias}"` の分岐(`routeInference` 手前)、SSE |
| 管理 API/UI | `gwcore/admin.go` + `adminui/index.html` 拡張 | `/api/workflows` CRUD・validate・publish・runs 照会。「Workflows」タブ追加 |

Step Attempt = `contract.Request` 1発行。RequestID は `{run_id}-{step_id}-a{attempt}` 形式とし、既存の監査・メトリクス・Cancel(`orchestrator.go:90`)にそのまま乗る。

## 5. API 差分表

### 5.1 データプレーン(既存 `:8443`、Virtual Key 認証)

| v1.1 | 本適用版 | 備考 |
|---|---|---|
| POST /v1/workflows/{alias}/runs | 採用 | response_mode: sync / stream。async は W1 後半(§10 注記) |
| GET /v1/workflow-runs/{run_id} | 採用 | メタデータ + 最終 Output(権限・保持内のみ) |
| GET /v1/workflow-runs/{run_id}/steps | 採用 | |
| GET /v1/workflow-runs/{run_id}/events | 採用 | SSE、Last-Event-ID 再開 |
| POST /v1/workflow-runs/{run_id}/cancel | 採用 | 既存 `Backend.Cancel` を初めて HTTP 公開 |
| POST /v1/workflow-runs/{run_id}/retry | W1 対象外 | 新 Run 作成で代替可のため後回し |
| /v1/chat/completions の `model: "flow:{alias}"` | 採用 | 既存 Model 名との衝突を config Validation で拒否(§18.4 準拠) |

### 5.2 管理プレーン(既存 admin listener `:9465`、admin-token 認証)

v1.1 の `/api/enterprise/inference-flows/*` を以下へ読み替え:

`GET/POST /api/workflows`、`GET/PATCH /api/workflows/{id}`(PATCH は revision 必須=ETag 相当)、`POST /api/workflows/{id}/validate`、`POST /api/workflows/{id}/publish`、`GET /api/workflows/{id}/versions`、`GET /api/workflow-runs`(一覧)。suspend/retire は status 変更として PATCH に統合。clone / route-test / test-runs は W1 対象外(route-test は Scheduler が単純なため価値が薄い)。

### 5.3 エラーコード

v1.1 §11.11 の Uniform Error を既存 `contract.Error` 系(`contract.go:103`)へ追加登録する形で採用: `workflow_not_found` / `workflow_not_published` / `workflow_input_validation_error` / `template_render_error` / `no_eligible_runtime`(= 全候補 `model_not_available`)/ `dependency_failed` / `budget_exceeded` / `idempotency_conflict` / `workflow_version_conflict`。**追加は semver マイナー、既存コードの変更なし**(設計原則5)。

## 6. 永続化設計(§12 の読み替え)

`<LYKURO_DATA_DIR>/workflows/` 配下。すべて 0600、temp+rename の原子的書込(`import.go:87 storeConfigGeneration` と同型)。

```
workflows/
  flows/<flow_id>/
    draft.json          # revision・checksum 付き。更新可
    v<N>.json           # 公開版。作成後、書込禁止(アプリ層で拒否 + checksum 検証)
    meta.json           # alias / status / latest_version / retired_at
  runs/<YYYYMMDD>/<run_id>.json    # Run/Step/Attempt/Route Decision メタデータ。本文なし
  runs/<YYYYMMDD>/<run_id>.events.jsonl  # Event(本文なし、sequence 単調増加)
```

- **6.1 本文の扱い(Zero-Retention 整合)**: Step Input/Output 本文は既定で**メモリ内のみ**(Run 完了・結果取得後に破棄)。ディスク保存する場合(`retention_days > 0` を明示設定した場合のみ)は `platform/memory` の AES-256-GCM `seal` を再利用した `.enc` とする。監査・メトリクス・events.jsonl に本文を書かない(`content_logged: false` テストを Workflow にも適用)。
- **6.2 alias 一意性**: 全 Flow の meta.json ロード時に検証(シングルテナントのため単純)。
- **6.3 Retention 既定**: Run メタデータ 90日 / Event 30日(日次ディレクトリ削除)/ 本文 0日。既存 `retentionLoop`(`platform.go:110`)と同型の sweep。

## 7. Flow Definition / 設定の差分

- **7.1** v1.1 §7.3 の JSON をそのまま有効とする。ただし W1 では以下フィールドを「受理するが無視」ではなく**明示的に拒否**(Fail Closed): `network_zone`、`conversation_mode: "CONVERSATION"`、`output_schema`(Schema 検証は W2)、Handoff REF/SUMMARY、W2/W3 の step type。
- **7.2** `runtime_target` = `{ "logical_model": "<logical_name>", "pool_id": "<任意>" }`。候補選択・sticky・failover は既存 `Resolve` + `pool.Order` に委譲し、`pool_id` 指定時は候補 Deployment を Pool 所属分に絞る(`contract.Request` へ追加する `PoolID` フィールド経由。追加のみの後方互換変更)。
- **7.3** Pool 定義を `platform.pools[]` として新設: `{ "id", "description", "deployment_ids": [] }`。Validation で ID 一意・Deployment 存在を確認。`fallback_policy.allowed_pool_ids` は定義済み Pool のみ許可。
- **7.4** Feature Flag(gateway.yaml `platform.workflows`): `enabled`(既定 false)、`openai_alias_enabled`(既定 true)、`max_flows`、`run_retention_days`、`content_retention_days`(既定 0)。v1.1 の parallel/iteration/fixed_runtime フラグは W2/W3 で追加。

## 8. 認可モデル読み替え(§14.2 RBAC → 2層)

| v1.1 Scope | 本適用版 |
|---|---|
| workflow:read / write / publish / admin / test | admin-token(管理プレーン)。単一権限 |
| workflow:execute | Virtual Key。`allowed_models` に `flow:{alias}` を記載して制御(空 = 全許可、既存規約踏襲) |
| workflow:run:read / cancel | 実行した Virtual Key 自身の Run のみ照会・Cancel 可(Run に key_id を記録) |
| workflow:content:read | 中間 Output 参照は admin-token のみ + `content_retention_days > 0` の場合のみ |
| workflow:debug(Node 詳細) | admin-token のみ(データプレーン応答には DeploymentID を含めない) |
| workflow:runtime:pin | W1 対象外 |

## 9. UI 縮退方針(§13)

管理画面(`adminui/index.html`、vanilla JS・依存追加なし)へ2タブ追加:

1. **Workflows**: Flow 一覧(status/version/alias)、JSON エディタ(textarea)での Draft 編集、Validate ボタン(エラー一覧表示)、Publish ボタン(確認ダイアログ + revision 競合時 409 表示)、Version 履歴。
2. **Workflow Runs**: Run 一覧(status/flow/duration/usage)、Run 詳細(Step・Attempt・Route Decision・Error Code のテーブル)、Cancel ボタン。本文は表示しない(content_retention 設定時のみ admin が明示操作で取得、監査記録付き — v1.1 §13.12 の Mask 原則を踏襲)。

Canvas / Wizard / Diff Viewer / Mini Map / Virtualization / WCAG AA 全準拠は対象外(受入条件 UI-AT-02〜04・12〜15 は対象外または自明成立)。

## 10. 縮退に伴う設計判断(明示)

1. **Recovery(§15.4)**: 単一プロセスのため、再起動時に RUNNING の Run を `FAILED(error_code=interrupted)` へ確定する(二重実行ゼロを優先)。Lease・Event 再構築は実装しない。Run メタデータは Step 確定ごとに書き込むため、どこまで進んだかは追跡可能。
2. **async mode**: 最終 Output をメモリ保持(TTL、既定 1h)する必要があり Zero-Retention と緊張関係にあるため、W1 前半は sync/stream のみ。async は本文メモリ保持の TTL 設計とセットで W1 後半に判断。
3. **route-test API / UI**: 候補選択が `Resolve`+`Order` に単純化されたため W1 では見送り。
4. **Timeout 非 Failover 原則**: 既存 3 箇所(`proxy.go:180` / `orchestrator.go:262` / `connector.go:146`)と同じく、Step Timeout では別 Deployment 再試行をしない(二重推論防止)。`retry_policy` が効くのは接続失敗・503・capacity 系のみ。

## 11. W1 実装順序と変更予定ファイル

| 順 | Task(v1.1 対応) | 変更・新規ファイル |
|---|---|---|
| 1 | W1-02/03: Flow 定義・Validation・Template Engine | 新規 `platform/workflow/{flowdef,template}.go` + tests |
| 2 | W1-01: Flow Store(ファイル永続化・Version 不変) | 新規 `platform/workflow/store.go` + tests |
| 3 | W1-04: Runner(Sequential・Handoff・状態・Event) | 新規 `platform/workflow/{runner,run}.go` + tests(Fake Backend) |
| 4 | W1-05: Retry・Fallback・Cancel | `runner.go` 内 + tests |
| 5 | W1-06: Gateway API・SSE・`flow:{alias}`・Idempotency | 新規 `gwcore/workflow_api.go`、`gwcore/server.go`(ルート追加)、`gwcore/platform_adapter.go`(flow: 分岐)、`gwcore/config.go`(`platform.workflows` 検証・alias 衝突検証) |
| 6 | W1-07: 管理 API・UI タブ | `gwcore/admin.go`、`gwcore/adminui/index.html` |
| 7 | W1-08: Audit・Metrics・Retention | `gwcore/audit.go`(result 値追加)、`gwcore/metrics.go`(workflow 系列)、sweep |
| 8 | W1-09: E2E・Regression・README | 新規 `gwcore/workflow_e2e_test.go`(`platform_e2e_test.go` の fakeRuntime 流用)、README、CHANGELOG |

契約上の注意: `platform/contract` へのエラーコード追加は後方互換(追加のみ)。`sign` / `token` は変更なし。

## 12. テスト対応(§20 → 本適用版)

IT-01(2Runtime追加質問)・02(異種エンドポイント)・03(Logical Model)・04(候補選択)・05(Retry別Deployment)・06(Fallback Model)・07(Fail Closed)・08(SSE Resume)・09(Idempotency)・10(Version固定)・11(Cancel)・15(Retention 0日)・16(flow: alias)・17(既存 Model 回帰)を採用。IT-12(Recovery)は「再起動後 FAILED 確定・二重実行なし」の縮退版。IT-13(Tenant)・14(Control Plane 非送信)は、14 のみ「Workflow データを Agent 送信対象に含めない」テストとして採用。

## 13. 決定事項(2026-08-16 確認済み)

1. 管理プレーン API は admin listener(`:9465`、admin-token 認証)へ配置する。
2. Pool 概念を `platform.pools[]` として新設し、`runtime_target.pool_id` / `fallback_policy.allowed_pool_ids` は v1.1 のまま採用する(将来の Distributed 化に近い形を維持)。
3. W1 の response_mode は sync / stream のみ。async は本文メモリ保持 TTL の設計とセットで次段判断。
4. Workflow 機能は通常機能として提供する(ライセンスゲートなし)。
