# Native LLM Platform 統合 — 最終報告(LYK-NLP-SD-001 v2.0 §25 形式)

| 項目 | 内容 |
|---|---|
| 対象文書 | LYK-NLP-SD-001 v2.0(統合基本設計書)、LYK-NIE-SD-001 v1.0(Engine 完全仕様) |
| 実施日 | 2026-08-07 |
| 前提報告 | `docs/20260806/platform_phase0_assessment.md`(Phase 0 調査・Contract 凍結) |

## 既存構成の調査結果

Phase 0 報告のとおり。Private LLM Gateway(PR #160〜#167)は本番リリース済みで、`internal/privategw/gwcore/`(顧客側、DB レス)と SaaS 側(migration 034/035・管理 API・Agent API・Ed25519 署名基盤・Package Builder)を再利用の正本とした。

## Gateway As-Built・再利用範囲

Gateway は**再実装していない**。外部 API(`/v1/models`・`/v1/chat/completions`・`/v1/embeddings`・`/v1/responses`・`/v1/rerank`)、Virtual Key 認証(`lkpgw_`)、RPM、Policy(`X-Data-Class`/`X-Routing-Mode`/tools 統制)、JSONL 監査、usage 集計、署名済み config 世代管理、Agent はすべてそのまま。変更は adapter 追加の最小差分のみ(下記「変更ファイル」)。

## Native Engine As-Built・対応能力

Engine は**実装中**(repository 未接続)。LYK-NIE-SD-001 v1.0 の Data API / Control API を `enginecontract` に Go interface として凍結し、mock(`enginecontract.NewMockEngine`)で統合した。`engines[].endpoint: "mock:"` のみ解決し、それ以外の endpoint は「未接続 = deployment unavailable」として扱う(未実装能力を公開しない、§8.3)。実 Engine の transport binding(mTLS gRPC 推奨)は Engine release 時に versioned adapter で確定する。

## 本仕様との対応表 / Reuse・Extend・New・Verify・Out of Scope

Phase 0 報告 §3 の表のとおり実装した。要点:

- **Reuse**: gwcore 外部 API・auth・Policy・Audit・Usage・Agent・SaaS 側 entity(tenant/virtual key 等の同義テーブル新設なし)
- **Extend**: gwcore に `contract.Backend` 差込点+feature flag、SaaS 側 config 検証(`gwcore.ParseConfig` が platform 節も検証)・deployment 詳細 API に platform 情報追加
- **New**: `internal/privategw/platform/`(下記)
- **Verify(未了)**: 実 Engine 接続、実 GPU、Kubernetes HA、k6 性能
- **Out of Scope**: Gateway/Engine 再実装、第三者 Runtime 配布、cloud fallback 拡張

## 実装対象Phase・Work Package

| Phase | 状態 |
|---|---|
| 0 Assessment・Contract Freeze | 完了(`platform_phase0_assessment.md`) |
| 1 Gateway Platform Adapter | 完了(flag OFF で旧経路完全維持、回帰テスト付き) |
| 2 Model Manager・Engine 統合 | 完了(Engine は mock。Control API は Model Manager 専有を型で強制) |
| 3 External Runtime Connector | 完了(OpenAI 互換 connector、circuit breaker、error 正規化) |
| 4 Conversation Memory・Retention | 完了(AES-256-GCM・30日既定・sweep・cascade delete・idempotency) |
| 5 Distributed Request Pool | 完了(health・sticky・failover。単一プロセス内 pool — Node Agent 分離は将来) |
| 6 Task・Multi-Model | **未実装**(capabilities で `task_decomposition:false` を公開) |
| 7 Production Hardening | 部分(既存パッケージ配布・署名を再利用。SBOM/OCI/soak/chaos は未了 — Gateway 側残課題と同一) |

## 変更ファイル

**新規(顧客側 Platform)**: `internal/privategw/platform/{platform.go, contract/, enginecontract/, platformcfg/, modelmanager/, connector/, orchestrator/, memory/, pool/}`(計14ファイル+テスト3ファイル)
**新規(Gateway adapter)**: `internal/privategw/gwcore/platform_adapter.go`、`platform_e2e_test.go`(E2E 12本)
**変更(Gateway)**: `gwcore/config.go`(platform 節+検証)、`gwcore/server.go`(`routeInference` による flag 分岐・`/v1/models` の platform catalog 提供)、`cmd/private-gateway/main.go`(backend 組立て・config hot reload 時の再構築・Fail Closed)
**変更(SaaS)**: `internal/model/private_gateway.go`(PGWPlatformDeployment)、`internal/store/postgres/private_gateway_repository{,_impl}.go`(Upsert/Get)、`internal/gateway/handler/private_gateway_handler.go`(SetConfig 時の契約 version 記録・Get 詳細に platform 情報)
**新規(schema/テスト)**: `schema/036_platform_deployments.{up,down}.sql`、`test/integration/platform_deployment_repository_test.go`

## DB Migration

migration **036** `pgw_platform_deployments`(§14.14 の全カラム、pgw_deployments と tenant 内 1対1 UNIQUE、tenant_id FK+CASCADE、楽観ロック version)。顧客側は DB レス方針を維持(Platform 状態は DataDir、会話は暗号化ローカルファイル)。**本番未適用** — 適用は `make migrate-up`(リリースはユーザー実施)。

## API・Schema

- 外部 API は完全互換のまま(パス・スキーマ追加なし)。拡張 header は `X-Lykuro-Conversation-ID` / `X-Lykuro-Conversation-Version` / `X-Lykuro-Retention-Policy` のみ(opt-in)
- 署名済み config に `platform:` 節を同乗(schema_version "1" のまま、節欠落/enabled:false で旧経路)
- 管理 API: `GET /api/tenants/{id}/private-gateways/{gwID}` の応答に `platform`(契約 version 等、非秘密)を追加。platform 節付き config の `SetConfig` で pgw_platform_deployments へ upsert

## Gateway↔Platform Contract

`gateway-platform-v1`(in-process Go interface、transport 非依存)。Operations: `GetPlatformCapabilities / Execute / ExecuteStream / Cancel / CountTokens / GetRouteStatus / ListModels`。Virtual Key 原文・未検証 header・Control Plane credential は渡さない。error →HTTP の凍結表(Phase 0 §4.2)は `contract.HTTPStatus`/`GatewayErrorCode` に実装し、適合テストで固定。

## Platform↔Engine Data API・Control API

`platform-engine-data-v1` / `platform-engine-control-v1`(§9 の RPC・フィールド名どおり)。Control API クライアントは Model Manager のみが保持し Orchestrator には Data API のみ注入(AT-G03 を型で強制)。Engine error 17種→platform code 正規化(Phase 0 §4.3)は `enginecontract.PlatformCode` に実装し、適合テストで固定。`request_cancelled` はクライアント切断として扱う。

## Model Manager・Connector

- catalog は署名済み config の `platform.models[]`(approval_status=approved のみ routing 可、suspended/retired は候補外)
- native deployment は signed load(artifact digest 検証)→status→capacity、drain/resume は Manager 経由のみ
- connector は Generic OpenAI-Compatible(endpoint は config 記載の allowlist そのもの、TLS、credential はファイル参照)。circuit breaker+error 正規化+health probe

## Conversation Memory・Retention

- 既定 **stateless(0日)**。`memory.mode: managed` の明示 opt-in でのみ有効
- 本文は `/var/lib/lykuro/…/conversations/*.enc` に AES-256-GCM(鍵は 32byte hex ファイル参照)。平文保存なしをテストで検証
- retention 既定 30日、期限切れは読出し不可(Fail Closed)+1時間毎 sweep で物理削除。`Delete` は message/summary cascade。version 楽観ロック+request_id idempotency
- 本文・summary・memory は Control Plane へ**送信しない**(既存許可リスト = usage 集計行/heartbeat のみを維持)

## Distributed Pool

runtime endpoint/engine を node として health 管理、sticky routing(TTL 既定 300s)、primary 障害時の failover、全滅時 `model_not_available`(503)。単一プロセス内 Coordinator であり、別プロセス Node Agent・request sharding は未実装(Phase 1 スコープ外、§7.4)。

## Native Inference Engine

未接続(実装中)。mock で contract 適合まで検証済み。実 Engine 接続時の Verify 項目: transport binding 決定、GetCapabilities/GetManifest 突合、certified profile、GPU 実機での性能・OOM 挙動。

## Security・Strict Local

- platform 経路は **local runtime のみ**を候補にし、hybrid(承認済み cloud)構成が存在しても cloud へ到達しない(cloud hit 0 を E2E で検証)
- Fail Closed: platform 節は署名済み config の一部(検証不能時は旧 config 維持)。platform 構築失敗時は route 無効化+旧経路
- tenant/key 分離: key の allowed_models・tools 統制・data class 検査は Gateway 側で従来どおり実施(platform へは検証済み policy context のみ)
- 監査 JSONL は `content_logged:false` を維持、物理 model/instance ID は外部応答へ出さない

## 実行したTestと結果

すべて 2026-08-07 にローカル(darwin/arm64、Docker+Testcontainers)で実行し合格:

- `go build ./...` / `go vet ./...` — 合格
- `go test ./...`(全ユニット)— 合格。うち platform 関連:
  - gwcore E2E 12本(chat/stream/SSE 枠組み/models/unknown model/failover/全滅 503/conversation memory/stateless 非保存/**flag OFF 旧経路回帰**/key モデル制限/**Strict Local cloud hit 0**)
  - contract 適合(§4.2 凍結表+contract version 固定)、enginecontract 適合(§4.3 凍結表 17種)
  - memory 7本(encryption at rest・idempotency・version conflict・cascade delete・retention expiry sweep・active 保持・復号不能除去)
- `go test -tags integration ./test/integration/ -run 'TestPGW|TestPlatformDeployment'` — 合格(実 PostgreSQL 16。migration 036 の upsert/get/tenant 分離を含む)
- `golangci-lint`(CI では soft gate): 新規コードの gofmt/staticcheck/unparam/prealloc は解消。残は既存コードと同型の `_ =` errcheck スタイルのみ

**実行していないもの**: 実 Engine/実 GPU 推論、Kubernetes HA、k6 性能、soak/chaos、実環境(顧客ネットワーク)通信試験。理由: 対応環境なし(Engine は実装中、GPU/K8s クラスタ未所持)。再現手順: Engine release 後に `engines[].endpoint` を実 endpoint に差替え、`platform_e2e_test.go` の fakeRuntime を実 vLLM に置換して同一テストを実行。

## Package・Deployment

既存の Package Builder(決定性ビルド・checksums.sha256・Ed25519 signature)・compose/K8s テンプレート・license 経路をそのまま使用(platform は同一バイナリ内のため配布物追加なし)。SBOM・OCI 公開・offline-images.tar は Gateway 側からの継続残課題。

## Compatibility Matrix・Upgrade・Rollback

- rollback は 3層: ① config で `platform.enabled:false`(即時・旧透過経路)② 節ごと削除 ③ 既存 config 世代 rollback(LKG)
- config hot reload(Agent/import)時は backend を再構築し、構築失敗時は platform route を無効化して旧経路へ(Fail Closed)
- 契約 version は pgw_platform_deployments に記録(gateway-platform-v1 / engine data・control v1)。契約変更は改版手続き(テストで固定)

## 未実装・未検証・既知問題

1. **Phase 6(Task Decomposition・Multi-Model)未実装** — capabilities で false を公開
2. **実 Engine 未接続** — mock のみ。transport binding は Engine release 時に確定
3. **app.lykuro.ai の Platform 詳細画面(§15)未実装** — API は deployment 詳細に platform 情報を返すのみ。Models/Nodes/Retention 画面は次期
4. **Node Agent 分離・request sharding 未実装**(単一プロセス内 pool)
5. **SSE 開始後の conversation version 通知不可**(HTTP header 制約。非 stream か次回要求で取得)
6. **summary 生成は同期 catalog モデル依存**(summary_model 未設定なら生成なし)
7. Gateway 側からの継続残課題: SBOM/OCI、HA(Helm PVC 1replica)、/metrics、CLI upgrade/rollback、Cloudflare register レート制御
