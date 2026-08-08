# Native LLM Platform — Phase 0 既存構成調査・Contract 凍結報告

| 項目 | 内容 |
|---|---|
| 対象文書 | LYK-NLP-SD-001 v2.0(統合基本設計書) |
| 調査日 | 2026-08-07 |
| 前提 | Private LLM Gateway は実装済み(PR #160〜#167、本番リリース済)。**Native Inference Engine は実装中**(別作業。本 Platform 統合では contract を凍結し mock/stub で統合、実 Engine 接続は Engine release 後の Verify 項目) |

---

## 1. Gateway As-Built(再利用の正本)

### 1.1 顧客側 Gateway(`internal/privategw/gwcore/`、約3,100行)

| 領域 | 実装事実 |
|---|---|
| 外部API | `GET /v1/models`、`POST /v1/chat/completions`・`/v1/embeddings`・`/v1/responses`・`/v1/rerank`(透過)、`/healthz`・`/readyz`・`/version`。chi ルータ、OpenAI互換エラー `{"error":{message,type,code,request_id}}` |
| 認証 | Virtual Key `lkpgw_` + SHA-256 hash(`auth.go`)。VirtualKeyDef = {ID, Name, KeyHash, AllowedModels, RPMLimit, Disabled, AllowTools}。RPM は in-memory 固定1分窓 |
| Policy | `X-Data-Class`(gateway.allowed_data_classes 照合)、`X-Routing-Mode`(local-only/hybrid)、StrictLocal 既定ON(nil=ON)、CloudEligible 6条件AND、tools 統制 |
| 推論経路 | `proxy.go proxyRequest`: model解決→キー許可→候補列(primary+fallback+適格時cloud)→透過転送(bodyはmodelのみ書換)→SSE/usage sniff→JSONL監査 |
| Config | `gateway.yaml` schema_version **"1"** 固定。Ed25519 署名済み config を Agent/import で検証(canonicalJSON→SHA-256→Ed25519→staging validate)。世代 `config-v{N}.json` 3世代 + LKG マーカー |
| Agent | `/api/gateway-agent/v1/{register,heartbeat,config,config-ack,usage}`。credential `lkpga_`。heartbeat 60s / config poll 300s |
| 監査 | ローカル JSONL(`content_logged:false` 強制)。Control Plane 非送信 |
| Usage | `(date, logical_model)` メモリ集計→Agent 経由 upload、失敗時 Restore |
| 状態 | DB なし。DataDir(`/var/lib/lykuro/gateway`)にconfig世代・credentialのみ |

### 1.2 SaaS 側(Control Plane)

| 領域 | 実装事実 |
|---|---|
| DB | migration 034(pgw_deployments/packages/install_tokens/config_versions/download_events)・035(agent列+pgw_usage_daily)。**次は 036** |
| 管理API | `/api/tenants/{tenantID}/private-gateways...`(CanManageTenant、Idempotency-Key、plan_entitlements `private_gateway`) |
| Agent API | `/api/gateway-agent/v1/*`(Bearer agent credential、AuthMiddleware 外) |
| 署名基盤 | Ed25519 PKCS#8 PEM(`PGW_SIGNING_KEY_FILE`、docker secret `pgw_signing_key`)。config/package/license の3用途 |
| 配布 | Package Builder Worker(topic `pgw.package_build`)、S3+presign 15分、決定性ビルド、manifest/checksums/signature.sig |
| License | admin API 発行、Ed25519 署名 JSON、valid/in_grace/expired |
| 画面 | `web/src/app/(dashboard)/enterprise/[tenantId]/private-gateways/`(一覧/作成/詳細) |

## 2. Native Engine As-Built

**実装中**(repository 未接続)。LYK-NIE-SD-001 v1.0 を contract 正本として扱い、以下を凍結する。実 Engine release 後に As-Built 差分を取り versioned adapter で吸収する。

- Data API: `Generate` / `GenerateStream` / `Cancel` / `CountTokens` / `GetCapabilities`(§9.2)
- Control API: `LoadModel` / `UnloadModel` / `Drain` / `Resume` / `GetModelStatus` / `GetCapacity` / `GetManifest`(§9.3)
- Request: `request_id, trace_id, tenant_scope, project_scope, model_instance_id, input{messages|prompt}, generation{max_output_tokens,temperature,top_p,top_k,seed,stop}, scheduling{priority,deadline_unix_ms}, cache{reuse_handle,allow_prefix_cache}`(§9.4)
- Response: `output_text, finish_reason, usage{input/output/total_tokens}, timing{queue/tokenize/prefill/decode/total_ms}, cache{reuse_handle}`(§9.6)
- Stream events: `response.started / response.output_text.delta / response.usage / response.completed / response.error`(§9.7)
- Error 17種(§21.2)、Engine state 8種 / Model Instance state 9種(§20)
- Transport: mTLS gRPC 推奨。**未確定のため Go interface として凍結し transport binding は Engine 側 release と同時に決定**

## 3. 本仕様との対応表(Reuse / Extend / New / Verify / Out of Scope)

| v2.0 論理コンポーネント | 分類 | 既存実装 / 実装先 |
|---|---|---|
| Private Gateway(外部API/auth/Policy/Audit/Usage) | **Reuse** | `gwcore` そのまま |
| Gateway Platform Adapter | **Extend** | `gwcore` に PlatformBackend interface + feature flag 経路を最小追加 |
| Platform Controller | **New** | `internal/privategw/platform/`(config・状態管理) |
| Inference Orchestrator | **New** | `internal/privategw/platform/orchestrator/` |
| Model Manager | **New** | `internal/privategw/platform/modelmanager/` |
| Runtime Connector | **New**(既存 proxyCore の透過ロジックを流用) | `internal/privategw/platform/connector/` |
| Native Engine | **Verify(実装中)** | `platform/enginecontract/` に contract + mock。実 Engine は接続時検証 |
| Distributed Coordinator / Node Agent | **New** | `internal/privategw/platform/pool/` |
| Conversation Memory / Context Builder | **New** | `internal/privategw/platform/memory/` |
| Control Agent | **Reuse+Extend** | 既存 gateway agent の signed config 経路に platform 節を同乗(§13.6 の別Agent は作らない) |
| app.lykuro.ai 管理画面(Models/Nodes/Retention) | **Extend(段階)** | 既存 private-gateways 画面配下。SaaS 側は config 検証拡張+platform_deployments(036)を先行、詳細画面は残課題として報告 |
| tenant/user/role/project/virtual_key/audit/usage | **Reuse** | 既存 entity。同義 table 新設しない |
| Gateway 再実装 / Engine 再実装 / 第三者 Runtime 配布 / Cloud fallback 拡張 | **Out of Scope** | — |

## 4. Contract 凍結

### 4.1 gateway-platform-v1(Gateway↔Platform)

- **形態**: Go interface(`internal/privategw/platform/contract`、`ContractVersion = "gateway-platform-v1"`)。現行 Gateway は単一プロセス・内部APIなしのため、**in-process binding を第一形態**とする(§5.3「既存GatewayがHTTP内部APIを使用している場合は同等の相互認証」— 既存は内部APIを持たないため、プロセス分離時に mTLS gRPC へ昇格する。interface はその前提で transport 非依存に定義)。
- Operations: `GetPlatformCapabilities / Execute / ExecuteStream / Cancel / CountTokens / GetRouteStatus`
- Request 必須項目: contract_version, request_id, trace_id, tenant_scope, project_scope, actor_scope(=virtual_key_id), policy_context{data_class, routing_mode, allowed_models, tools_allowed}, logical_model, normalized_input(OpenAI互換 raw JSON。**gwcore の透過原則を維持し再構造化しない**), conversation_context, deadline
- Gateway が渡さないもの: Virtual Key 原文 / 未検証 header 値 / Control Plane credential
- Platform が返すもの: normalized response/stream event(SSE生バイト互換)、logical model・deployment 非秘密ID、usage、安定 error code、retryable+Retry-After

### 4.2 Platform error → Gateway HTTP(§13.1.1 準拠・凍結)

| Platform code | HTTP | gwcore 既存 code へのマップ |
|---|---:|---|
| platform_invalid_request | 400 | invalid_request |
| conversation_conflict | 409 | conversation_conflict(新設) |
| model_not_allowed | 403 | policy_denied |
| model_not_available | 503 | model_unavailable |
| capacity_exhausted | 429 | rate_limit_exceeded(+Retry-After) |
| inference_timeout | 504 | inference_timeout |
| platform_not_ready | 503 | model_unavailable |
| internal_contract_error | 502 | server_error |

### 4.3 Engine error(17種)→ Platform code 正規化(凍結)

| Engine code | Platform code |
|---|---|
| invalid_request / context_length_exceeded | platform_invalid_request |
| authentication_failed / internal_error / inference_failed / stream_consumer_slow | internal_contract_error |
| model_not_loaded / unsupported_model / artifact_verification_failed / gpu_unhealthy / engine_draining | model_not_available |
| resource_exhausted / capacity_exhausted / gpu_oom | capacity_exhausted |
| deadline_rejected / deadline_exceeded | inference_timeout |
| request_cancelled | (クライアント切断として扱い応答なし) |

### 4.4 platform-engine-v1(Platform↔Engine)

`internal/privategw/platform/enginecontract` に Data API / Control API を Go interface で凍結(§2 の RPC 名・フィールド名どおり)。呼出主体の分離を型で強制: Control API クライアントは Model Manager のみが保持し、Orchestrator には Data API のみ注入する(AT-G03)。

## 5. Security boundary・Data flow

- 本文(prompt/response/summary/memory)は顧客環境内のみ。Control Plane への送信は既存許可リスト(usage rows/heartbeat)を維持し、Platform 追加項目も metadata のみ
- Conversation 本文は `/var/lib/lykuro/platform/conversations/` に AES-256-GCM 暗号化保存(opt-in 時のみ)。鍵はファイル参照(`platform.memory.encryption_key_file`)、DB/設定に原文を置かない
- Fail Closed: platform 節の署名検証は既存 config 署名経路に同乗(config 全体が Ed25519 署名済み)。検証不能時は旧 config 維持
- Feature flag: `platform.enabled`(config 内)。false または節欠落時は**既存 proxyRequest 経路を完全維持**(rollback 経路)

## 6. Migration・Test plan

- SaaS: migration 036(platform_deployments。pgw_deployments 1対1、契約version記録)。顧客側は DB レス方針を維持し、Platform 状態は DataDir 配下のファイル(config 世代方式を踏襲)+ 会話は暗号化ローカルストア
- Test: 既存 `make test-unit` / `make test-int` / CI ゲート(go build/vet/tsc/vitest)を維持。追加 test は gwcore 回帰(flag OFF)、contract 適合、tenant/key分離、Strict Local(cloud hit 0)、retention/deletion、pool failover
- 実 Engine・実GPU・k6 性能・Kubernetes HA は本統合では**未検証**として最終報告に明記

## 7. 差分・リスク(調査で確認した既知事項)

1. Engine「実装済み」前提(v2.0 §0)と実態(実装中)の差 → 本報告の前提欄のとおり contract-first で吸収
2. 文書間表記差(checksums.sha256/txt、license/licenses 等)→ 実装済み builder(`checksums.sha256`)を正とする
3. §13 の `/api/enterprise/...` パスは既存規約 `/api/tenants/{tenantID}/...` に読み替える(既存 Phase 0 報告と同判断)
4. §5.3 の gRPC 推奨は in-process 第一形態で開始(上記 4.1)。プロセス分離は将来 Phase
5. runbook の署名鍵パス表記不一致(`/run/secrets/pgw_signing.pem` vs 実マウント `/run/secrets/pgw_signing_key`)— 既知の軽微事項、本統合では触れない
