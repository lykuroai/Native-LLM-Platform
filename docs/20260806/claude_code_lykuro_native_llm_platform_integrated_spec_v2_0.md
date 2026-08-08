# Lykuro Native LLM Platform 統合基本設計書

## Private LLM Gateway統合完全版

| 項目 | 内容 |
|---|---|
| 文書番号 | LYK-NLP-SD-001 |
| 版 | v2.0 Claude Code Complete Integration Edition |
| 制定日 | 2026-08-07 |
| 最終改定日 | 2026-08-07 |
| 作成 | 株式会社eビジネスソリューション / Lykuro.ai |
| 正式製品名 | Lykuro Native LLM Platform |
| 既存統合対象 | 実装済みLykuro Private LLM Gateway、実装済みLykuro Native Inference Engine |
| 新規・拡張対象 | Platform Controller、Model Manager、Inference Orchestrator、Runtime Connector、Distributed Local LLM Pool、Conversation Memory |
| 機密区分 | 社内・提案先限定 |
| 実装担当 | Claude Codeおよび開発担当者 |
| 関連文書 | LYK-PLG-BD-001「Lykuro Private LLM Gateway 基本設計書 v1.1」、LYK-NIE-SD-001「Lykuro Native Inference Engine 完全仕様書」 |
| 置換対象 | LYK-NLP-SD-001 v1.0 |
| 文書状態 | Claude Code実装用・統合完全版 |

---

## 0. Claude Codeへの実装指示

本書は、実装済みのLykuro Private LLM Gatewayを入口として再利用し、その下位にモデル管理、推論オーケストレーション、分散実行、会話記憶および実装済みLykuro Native Inference Engineを統合するための基本設計書である。

本書の製品名は「Lykuro Native LLM Platform」とする。「Lykuro Private LLM Gateway」は製品内の既存API Gatewayコンポーネントであり、新規開発対象ではない。

Claude Codeは、Gatewayを再実装、複製、置換してはならない。コード変更前にGateway、Native Engineおよびapp.lykuro.aiの既存リポジトリを調査し、本書の論理コンポーネントを既存構成へ対応付けること。既存Gatewayの認証、tenant、Virtual Key、課金、監査、OpenAI互換API、画面、配布、Control Agent、migration、CI/CDを再利用する。

Native Engineも実装済みとして扱う。Claude CodeはNative Engine内部を本書から新規実装せず、実装済みData API、Control API、build、対応modelおよび制約をAs-Built調査してPlatformへ接続する。

### 0.1 文書の責務と優先関係

| 対象 | 正本 |
|---|---|
| Gateway内部、外部OpenAI互換API、認証、Virtual Key、Gateway配布 | LYK-PLG-BD-001および既存Gateway実装 |
| Platform全体構成、Gateway連携、Model Manager、Pool、Memory | 本書 |
| Native Engine内部、推論、Scheduler、KV Cache、GPU管理 | LYK-NIE-SD-001、実装済みsourceおよびAs-Built結果 |
| Gateway↔Platform、Platform↔Engine | version管理されたAPI contract |

本書は、関連文書に含まれる次の記述を上書きする。

- Ollama、llama.cpp、vLLM、TGI等をLykuro製品へ内蔵・同梱する記述
- 第三者RuntimeをLykuroインストーラーが導入・更新する記述
- 第三者Runtimeの管理APIを利用者へ直接公開する記述
- 会話本文をLykuro Control Planeへ保存する記述
- GatewayまたはNative Engineをこれから新規開発する記述

Gateway内部について本書とLYK-PLG-BD-001が衝突する場合はGateway実装とGateway仕様を優先する。Platformとの境界についてはversion管理されたAPI contractを優先する。境界変更が必要な場合は、互換性評価とADRを作成し、暗黙の破壊的変更を行わない。

### 0.2 実装開始前の必須調査

1. app.lykuro.aiのfrontend、backend、worker、DB、cache、object storage構成
2. tenant、organization、user、role、project、API keyの既存entity
3. 実装済みPrivate Gateway、Control Agent、package builder、installer、CLIのrepositoryとrelease状況
4. GatewayのOpenAI互換API、streaming、error、request ID、Virtual Key、Policy、Auditの既存仕様
5. audit、usage、billing、retention、deletionの既存実装
6. model、provider、endpoint、capabilityの既存entity
7. container、Kubernetes、GPU、secret管理方式
8. 実装済みNative Engineのrepository、build、Data API、Control API、対応model、対応GPU、test結果
9. Gateway↔PlatformおよびPlatform↔Engineの現行接続方式
10. 本書と既存コードの対応表
11. 新規実装、既存拡張、設定変更、対象外の分類

調査結果と影響範囲を報告するまでは、大規模な新規構成や認証方式変更を行わない。

### 0.3 絶対条件

- Ollama、llama.cpp、vLLM、TGIその他の第三者推論Runtimeを内蔵・再配布しない。
- 第三者Runtimeのbinary、container image、source、installer、Python環境を配布packageへ含めない。
- 第三者Runtimeを顧客OSへ自動導入・更新・削除しない。
- 標準版は顧客が管理する外部RuntimeへProtocol Connectorで接続する。
- Private LLM Gatewayを再実装、fork、複製または別認証方式で置換しない。
- Gatewayの既存OpenAI互換API、Virtual Key、RBAC、Policy、Audit、Usage、Control Agentを再利用する。
- Native Inference Engineは実装済みのLykuro独自componentとして利用する。
- Native Engine内部を本書の記載だけから再開発しない。
- Native Engineの拡張時も第三者Runtimeのsourceをコピーしない。
- CUDA、ROCm、BLAS、NCCL等の低レベルSDK利用は、license、security、SBOM審査後に許可できる。
- Strict Local Modeを既定ONとする。
- prompt、response、添付、RAG結果、tool入出力をLykuro Control Planeへ送信しない。
- 通常APIは本文保存0日を既定とする。
- 会話記憶は企業管理者が明示的に有効化した場合だけ保存する。
- tenant分離、server-side RBAC、署名検証、Fail Closedを必須とする。
- 顧客hostへNode.js、npm、nvmを要求しない。
- 未承認Cloud Runtimeへ自動fallbackしない。
- GatewayからNative EngineのControl APIを呼び出さない。
- Model ManagerだけがNative Engineのload、unload、drain、resumeを実行できる。
- Gateway、Platform、Engine間の本文通信は顧客環境内に限定する。

### 0.4 実装上の原則

- Model ManagerとInference Engineを分離する。
- GatewayとPlatform Inference Orchestratorを分離する。
- GatewayとConversation Memoryを分離する。
- Conversation Memoryを正本とし、KV Cacheを正本にしない。
- Runtime固有情報を外部APIへ漏らさない。
- model、runtime、node、conversationの識別子を分離する。
- interface、schema、migration、feature flagを先に定義する。
- 実行していないtestを成功と報告しない。
- 既存Gateway回帰testを通過しない変更を統合完了としない。

### 0.5 実装対象の判定

| 区分 | 取扱い |
|---|---|
| Existing / Reuse | Gateway、Native Engine、既存tenant/auth/billing/auditをそのまま再利用 |
| Extend | GatewayへPlatform Adapterまたは内部routeを最小追加。後方互換必須 |
| New | Model Manager、Inference Orchestrator、Runtime Connector、Pool、Memoryの未実装部分 |
| Verify | Engine API、Gateway API、model compatibility、Strict Local通信 |
| Out of Scope | Gateway再開発、Engine再開発、第三者Runtime配布、Cloud fallback |

---

## 1. 目的

実装済みLykuro Private LLM Gatewayを企業アプリケーション向けの唯一のAPI入口として再利用し、その背後にLykuro独自のモデル管理、推論オーケストレーション、分散処理、会話記憶および実装済みNative Engineを統合する。

本仕様の目的:

- 第三者RuntimeをLykuro製品へ内蔵せず、顧客Runtimeへ安全に接続する。
- モデル管理、承認、配置、監査をLykuro独自実装とする。
- 必要な顧客へ実装済みLykuro Native Inference EngineをNative Editionとして提供する。
- 複数のローカルLLMノードを一つのPoolとして管理する。
- 会話contextをLLM nodeではなく、顧客環境内のMemory Serviceで管理する。
- 本文保存期間と削除を企業policyとして強制する。
- Gatewayで確立済みの認証、RBAC、Virtual Key、Policy、Audit、Usageを重複実装せずPlatform全体へ適用する。

### 1.1 完成形

Lykuro Native LLM Platformは、顧客から見て一つの製品、一つの管理画面、一つのインストールおよび運用体系を提供する。内部では次のサービス境界を維持する。

1. Private LLM Gateway: 外部API、認証、認可、Policy、Audit、Usage
2. Platform Controller: Platform構成、model、node、pool、retention管理
3. Inference Orchestrator: route、context、capacity、failover
4. Native Inference Engine: 実際のtoken生成とGPU管理
5. Conversation Memory: 明示的に有効化された会話の正本

製品統合は、sourceやprocessを一つにすることを意味しない。配布、設定、管理、互換性を統一し、security boundaryと独立scaleを維持する。

### 1.2 利用形態

顧客環境に導入される本体は常時稼働するWeb service群である。

| 利用者 | Interface | 用途 |
|---|---|---|
| 社内application・AI agent・MCP client | 既存Gateway HTTPS/OpenAI互換API | 日常の推論利用 |
| 企業管理者 | app.lykuro.ai企業管理画面またはlocal console | model、node、policy、retention、運用 |
| 運用担当者 | CLI | install、status、diagnose、upgrade、rollback |
| Platform service | internal mTLS API | route、inference、control、health |

CLIは日常のprompt入力interfaceではない。推論要求は既存Gateway APIを利用する。

### 1.3 ビジネス要求

| ID | 要求 | 優先度 |
|---|---|---:|
| BR-01 | 実装済みGatewayを壊さずNative LLM Platformへ拡張できる | 必須 |
| BR-02 | 顧客local LLMをOpenAI互換APIとして安全に利用できる | 必須 |
| BR-03 | tenant、project、key、model、node、conversationを分離できる | 必須 |
| BR-04 | Strict Localと本文非送信を証明できる | 必須 |
| BR-05 | modelのlicense、承認、version、artifactを管理できる | 必須 |
| BR-06 | Native Engineと外部Runtimeの差異を利用者から隠蔽できる | 必須 |
| BR-07 | 複数nodeのthroughput、failover、業務分担を管理できる | 推奨 |
| BR-08 | 会話保存期間、削除、legal holdを企業policyとして強制できる | 必須 |
| BR-09 | Gateway/Platform/Engineを独立更新・rollbackできる | 必須 |
| BR-10 | online、customer VPC、on-premises、air-gappedへ導入できる | 必須 |
| BR-11 | 既存usage/billing/auditへ本文なしmetadataを統合できる | 必須 |
| BR-12 | セキュリティ質問票、監査、SLAに必要な証跡を出力できる | 必須 |

### 1.4 Usage・課金責務

Gateway、Platform、Engineは請求書を発行しない。既存app.lykuro.aiのusage/billingが次のmetadataを集計する。

- request_count
- input_tokens、output_tokens
- gpu_secondsまたはinference_seconds
- logical_model_id、deployment_id
- tenant_id、project_id
- result_code
- route type（native/connector。秘密endpoint名は除外）

prompt、response、summary、memory、tool本文は課金連携へ送信しない。

---

## 2. 対象範囲

### 2.1 対象

| 領域 | 対象 |
|---|---|
| Gateway Integration | 既存GatewayとのData API、stream、error、usage、health連携 |
| Model Manager | model catalog、approval、version、deployment、runtime mapping |
| Runtime Connector | 顧客管理Runtimeへのgeneric protocol接続 |
| Native Engine | Lykuro独自model loader、scheduler、generation、KV cache |
| Distributed Pool | node登録、request分散、task分割、協調、failover |
| Conversation Memory | conversation、message、summary、structured memory |
| Context Builder | token budget、履歴選択、summary、model切替 |
| Retention | 保存期間、削除、backup expiration、audit |
| Admin UI | model、runtime、node、conversation policy、retention |
| Security | Strict Local、tenant分離、mTLS、署名、本文非送信 |
| Packaging | 既存GatewayとPlatform componentの互換manifest、統合導入・更新 |
| Operations | health、diagnose、backup、rollback、SLO、責任分界 |

### 2.2 対象外

- 第三者Runtimeの配布、install、update、support
- Private LLM Gatewayの再実装または別Gatewayの新設
- Native Inference Engine coreの再実装
- 第三者modelの無審査download
- 基盤modelの学習
- Full training platform
- 顧客GPU driverの自動install
- Public Internetへ無認証の推論API公開
- Phase 1での独自CUDA kernel
- Phase 1での複数server Tensor Parallelism
- 会話本文のLykuro Control Plane保存
- Gatewayの既存認証、Virtual Key、Policy、Audit、Usageの重複実装

### 2.3 実装済みGatewayから再利用する機能

| Gateway機能 | Platformでの取扱い |
|---|---|
| OpenAI互換API | 外部APIとしてそのまま利用 |
| Virtual Key、OIDC/JWT、mTLS | 認証主体とtenant/projectをPlatformへ伝播 |
| RBAC | Platform管理APIでも同じrole体系を使用 |
| Policy Engine | data class、model許可、local-onlyを最優先 |
| Rate Limit、Budget | Platform/Engine capacityより前に適用 |
| Audit、Usage | Platform eventを既存audit/usage pipelineへ統合 |
| Control Agent、署名設定 | Platform設定の配信にも拡張 |
| Package、Installer、CLI | Platform componentを追加する拡張方式 |
| app.lykuro.ai企業画面 | 同じenterprise navigationとdesign systemを使用 |

上記機能の同義service、table、API、画面を新設しない。

---

## 3. 製品構成・Edition

正式な顧客向け製品名は「Lykuro Native LLM Platform」とする。Private LLM Gatewayは全Edition共通の既存入口である。

| Edition | 推論実行 | Lykuro配布物 |
|---|---|---|
| Connector Edition | 顧客管理Runtime | 実装済みGateway、Model Manager、Connector、Control Agent |
| Native Runtime Edition | 実装済みLykuro Native Engine | 上記＋Native Engine Adapter、Engine package |
| Distributed Edition | 複数の顧客RuntimeまたはNative Engine | 上記＋Coordinator、Node Agent |
| Air-Gapped Edition | 顧客RuntimeまたはNative Engine | 署名済みoffline package、config、license |

### 3.1 Connector Edition

- Lykuroは推論Runtimeを配布しない。
- 顧客が用意したendpointを登録する。
- Generic OpenAI-Compatible、Generic HTTP/gRPC等のConnectorで接続する。
- Runtime固有のinstall、model pull、binary updateは顧客責任とする。
- 管理操作はRuntimeが対応し、顧客が明示許可した範囲だけ実行する。

### 3.2 Native Runtime Edition

- Lykuroが実装済みのNative Inference Engineを配布する。
- 第三者推論Runtimeのbinary、container、sourceを含めない。
- 対応model、hardware、weight formatをLykuroが明示する。
- Platformはversioned Data API/Control APIでEngineへ接続する。
- GatewayからEngine Control APIを呼ばない。
- Gateway標準版をEngine変更のためにforkしない。

### 3.3 Distributed Edition

- 複数nodeをLogical Poolとして管理する。
- 最初はrequest単位の分散を対象とする。
- task decompositionとmulti-model collaborationを段階導入する。
- 一つのmodelを複数serverへ分割する機能は将来Phaseとする。

### 3.4 動作Mode

| Mode | 本文経路 | 既定 | 条件 |
|---|---|---:|---|
| Strict Local | 顧客環境内のみ | ON | 標準 |
| Connector Local | 顧客管理Runtimeへ顧客network内で接続 | OFF | endpoint承認 |
| Hybrid | Localまたは承認Cloud | OFF | 個別契約、feature flag、明示Policy |
| Air-Gapped | 外部通信なし | OFF | offline license/config/package |

可用性を理由にStrict LocalからCloudへ暗黙fallbackしない。

### 3.5 配置Model

| 配置 | 主な構成 | 用途 |
|---|---|---|
| Single Host | Gateway、Platform、Engineを別process/containerで同一host | PoC、小規模 |
| Split Host | Gateway/管理DBとGPU Engineを別host | 標準企業構成 |
| Kubernetes | Gateway、Platform service、Engine nodeを独立scale | Enterprise/HA |
| Multi-Site | siteごとのnode/pool、中央metadata管理 | 複数拠点 |
| Air-Gapped | local console、offline package/license | 閉域 |

Single Hostでもprocess、port、service identity、data directoryを分離する。

### 3.6 Source・License方針

- 標準契約ではLykuro source codeを配布しない。
- 署名済みOCI imageまたはnative package、config、CLI、SBOM、手順書を提供する。
- OEM、source license、source escrowは別契約とする。
- 第三者Runtime、model weight、GPU driverの再配布権をLykuro packageの一部として推測しない。
- model artifactは商用利用、API提供、再配布、地域、training useを審査する。

---

## 4. 全体アーキテクチャ

### 4.0 統合構成

~~~mermaid
flowchart TD
    A["Enterprise Application / MCP Client"] --> B["Existing Private LLM Gateway"]
    B --> C["Platform Inference Orchestrator"]
    C --> D["Model Manager / Router"]
    C --> E["Conversation Memory / Context Builder"]
    D --> F["Existing Native Inference Engine"]
    D --> G["External Runtime Connector"]
    H["app.lykuro.ai"] -. "Signed config / metadata only" .-> I["Control Agent / Platform Controller"]
    I --> D
~~~

### 4.0.1 Data Path

~~~text
Client
  -> Existing Gateway OpenAI-Compatible API
  -> Gateway Platform Adapter
  -> Platform Inference Orchestrator
  -> Context Builder（conversation利用時）
  -> Model Router / Scheduler
  -> Native Engine Data API または External Runtime Connector
  -> Gateway SSE/OpenAI response
~~~

### 4.0.2 Control Path

~~~text
app.lykuro.ai / Local Admin
  -> Control Agent / Platform Controller
  -> Model Manager
  -> Native Engine Control API
~~~

GatewayはControl Pathへ入らない。Native Engineのload、unload、drain、resumeはModel Managerのみが実行する。

### 4.1 Connector Edition

~~~mermaid
flowchart TD
    A["Enterprise Application"] --> B["Existing Lykuro Private Gateway"]
    B --> C["Platform Inference Orchestrator"]
    C --> D["Runtime Connector"]
    D --> E["Customer Managed Runtime"]
    F["app.lykuro.ai"] -. "Signed config / metadata" .-> B
~~~

### 4.2 Native Runtime Edition

~~~mermaid
flowchart TD
    A["Enterprise Application"] --> B["Existing Lykuro Private Gateway"]
    B --> C["Platform Inference Orchestrator"]
    C --> D["Existing Lykuro Native Engine Data API"]
    E["Lykuro Model Manager"] --> F["Native Engine Control API"]
    F --> D
    D --> G["GPU / CPU / Model Weight"]
~~~

### 4.3 Distributed Edition

~~~mermaid
flowchart TD
    A["Lykuro Gateway"] --> B["Distributed Coordinator"]
    B --> C["Node Agent A"]
    B --> D["Node Agent B"]
    B --> E["Node Agent C"]
    C --> F["Local LLM A"]
    D --> G["Local LLM B"]
    E --> H["Local LLM C"]
~~~

### 4.4 Conversation Memory

~~~mermaid
flowchart TD
    A["Client"] --> B["Gateway"]
    B --> C["Conversation Memory"]
    C --> D["Context Builder"]
    D --> E["Model Router"]
    E --> F["Selected LLM Node"]
    F --> C
~~~

---

## 5. コンポーネント設計

| Component | 状態 | 配置 | 責務 |
|---|---|---|---|
| Private Gateway | 実装済み・再利用 | 顧客環境 | 外部API、auth、RBAC、Policy、audit、usage |
| Gateway Platform Adapter | 最小拡張 | Gateway内部 | Platform Data API呼出、stream/error変換 |
| Platform Controller | 新規または既存拡張 | 顧客環境 | config、model、node、pool、retention管理 |
| Inference Orchestrator | 新規または既存拡張 | 顧客環境 | context、route、capacity、failover、cancel |
| Model Manager | 新規または既存拡張 | 顧客環境 | model catalog、approval、deployment、runtime mapping |
| Runtime Connector | 顧客環境 | 外部Runtimeとのprotocol変換 |
| Native Engine | 実装済み・再利用 | 顧客環境・Native Edition | model load、inference、scheduler、KV cache |
| Distributed Coordinator | 顧客環境 | node選択、task分割、failover、aggregation |
| Node Agent | 各node | capacity、health、model inventory、job control |
| Conversation Memory | 顧客環境 | message、summary、memory、retention |
| Context Builder | 顧客環境 | model別context構築、token budget |
| Control Agent | 顧客環境 | signed config、heartbeat、update |
| app.lykuro.ai | Control Plane | enterprise policy、package、license、metadata |

### 5.1 責務境界

- Gatewayはmodel weightをloadしない。
- GatewayはNative Engine Control APIを呼ばない。
- Gatewayはmodel、node、pool、conversationの正本を持たない。
- Model Managerはtoken生成を行わない。
- Native Engineはtenant認証を外部公開しない。
- Conversation Memoryはmodel routingを決定しない。
- Coordinatorは本文をControl Planeへ送信しない。
- Node Agentはapp.lykuro.aiから直接Inbound接続を受けない。

### 5.2 Gateway再利用境界

Gatewayで処理を完了してからPlatformへ渡す情報:

- request_id、trace_id
- tenant_id、project_id、actor scopeの内部識別子
- 認証済みrole/scope
- data_class、policy decision、allowed model条件
- rate limit/budget判定結果
- normalized OpenAI-compatible request
- deadline、priority上限

GatewayがPlatformへ渡してはならない情報:

- Virtual Key原文
- JWT原文
- tenantをheaderだけから採用した未検証値
- Control Plane credential
- 未承認Cloud fallback指示

PlatformからGatewayへ返す情報:

- normalized response/stream event
- logical model、selected deploymentの非秘密ID
- token usage、queue/timing、finish reason
- 安定したPlatform error code
- retry可能性とRetry-After候補

PlatformはGatewayの認証判断を再実装しない。ただし内部service identity、tenant scope、request署名を検証し、Gateway以外からの未認証呼出を拒否する。

### 5.3 Gateway↔Platform内部Contract

Production transportはmTLS付きgRPCを推奨し、既存GatewayがHTTP内部APIを使用している場合は同等の相互認証を必須とする。

| RPC/Operation | 用途 |
|---|---|
| GetPlatformCapabilities | API、model、conversation、stream能力取得 |
| Execute | 非stream推論 |
| ExecuteStream | stream推論 |
| Cancel | request取消 |
| CountTokens | route候補modelでtoken見積り |
| GetRouteStatus | logical modelとdeployment readiness |

contract必須項目:

~~~text
contract_version
request_id
trace_id
tenant_scope
project_scope
actor_scope
policy_context
logical_model
normalized_input
generation_parameters
conversation_context
deadline
idempotency_key（保存を伴う操作のみ）
~~~

本文を含むData APIは顧客環境内だけで通信する。Control Plane経由、heartbeat、usage uploadへ本文を載せない。

### 5.4 標準推論Sequence

~~~mermaid
sequenceDiagram
    participant Client
    participant Gateway as Existing Gateway
    participant Platform as Inference Orchestrator
    participant Memory as Context Builder
    participant Engine as Native Engine
    Client->>Gateway: OpenAI-compatible request
    Gateway->>Gateway: Auth / Policy / Rate Limit
    Gateway->>Platform: Execute or ExecuteStream
    Platform->>Memory: Build context when enabled
    Platform->>Engine: Generate or GenerateStream
    Engine-->>Platform: token / usage / timing
    Platform-->>Gateway: normalized stream / response
    Gateway-->>Client: SSE / JSON
~~~

### 5.5 失敗時の所有者

| 失敗 | 所有component | Gateway応答 |
|---|---|---|
| 認証、Virtual Key、RBAC | Gateway | 401/403 |
| Policy deny、data class不明 | Gateway | 403 |
| logical model未解決 | Platform Router | 404/403またはmodel_not_available |
| conversation conflict | Conversation Memory | 409 |
| queue超過 | Orchestrator/Engine | 429 |
| model未load、node停止 | Model Manager/Engine | 503 |
| inference deadline | Orchestrator/Engine | 504 |
| stream開始後の失敗 | Platform→Gateway | SSE error event後に終了 |

---

## 6. Lykuro Model Manager

### 6.1 目的

modelとRuntimeを統一的に管理し、外部RuntimeまたはNative Engineの差異を利用者から隠蔽する。

### 6.2 主な機能

- Logical Model登録
- model version、digest、weight format管理
- capability登録
- license審査
- approval workflow
- Runtime Endpoint登録
- connectivity test
- model inventory取得
- deployment作成・起動・停止
- health、capacity、GPU情報取得
- model alias切替
- rollback
- tenant/projectへのmodel許可
- audit

### 6.3 共通Model定義

~~~yaml
model_id: mdl_01
display_name: Qwen Local 14B
family: qwen
version: "1"
artifact:
  format: safetensors
  digest: sha256:example
capabilities:
  chat: true
  streaming: true
  embeddings: false
  tool_calling: true
  context_length: 32768
governance:
  approval_status: approved
  allowed_data_classes:
    - internal
    - confidential
  license_review_id: lic_01
~~~

### 6.4 Runtime Endpoint定義

~~~yaml
runtime_endpoint_id: rte_01
display_name: customer-runtime-a
connector_type: openai_compatible
base_url: https://runtime.internal.example
network_zone: secure-ai
credential_ref: vault://runtime-a/service-account
management_mode: read_only
health_path: /healthz
enabled: true
~~~

credential原文をDBへ保存しない。

### 6.5 管理Mode

| Mode | 許可操作 |
|---|---|
| observe_only | model一覧、health、capacity |
| inference_only | 推論、health |
| lifecycle_limited | 承認済みmodelのstart/stop |
| native_managed | Native Engineのload/unload/update |

第三者Runtimeに対するpull、delete、upgradeは標準で禁止する。

### 6.6 Model状態

~~~mermaid
stateDiagram-v2
    [*] --> draft
    draft --> reviewing
    reviewing --> approved
    reviewing --> rejected
    approved --> deploying
    deploying --> available
    deploying --> failed
    available --> suspended
    suspended --> available
    available --> retired
~~~

---

## 7. Runtime Connector

### 7.1 方針

Runtime Connectorはprotocol変換componentであり、推論Runtimeそのものではない。

- third-party binaryを含めない。
- third-party containerを起動しない。
- public model registryへ接続しない。
- external endpointのservice accountで接続する。
- response、stream、error、usageをLykuro共通形式へ正規化する。

### 7.2 共通Interface

~~~go
type RuntimeConnector interface {
    Capabilities(ctx context.Context) (Capabilities, error)
    ListModels(ctx context.Context) ([]RuntimeModel, error)
    Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
    ChatStream(ctx context.Context, req ChatRequest) (Stream, error)
    Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error)
    Health(ctx context.Context) error
    Capacity(ctx context.Context) (Capacity, error)
}
~~~

実際の言語と型は既存repositoryへ合わせる。

### 7.3 Connector種別

- Generic OpenAI-Compatible
- Generic HTTP JSON
- Generic gRPC
- Customer Plugin
- Lykuro Native Engine

vendor名を外部contractへ固定しない。

### 7.4 Security

- endpoint allowlist
- DNS/IP pinning方針
- SSRF防止
- TLS検証
- high-security経路のmTLS
- credential rotation
- request/response本文の通常log禁止
- timeout、circuit breaker、rate limit

---

## 8. Lykuro Native Inference Engine

### 8.1 位置づけ

Lykuroが独自開発済みの推論実行componentであり、Native Runtime Editionだけに含める。本書ではEngine内部を実装対象にせず、Platformとの統合、API適合性、security、package互換性および運用を対象とする。

Engine内部の正本は実装済みrepository、release artifact、test結果およびLYK-NIE-SD-001である。本書にあるAPI名や能力が実装と異なる場合は、推測でEngineを変更せずAs-Built差分を報告し、versioned adapterで吸収するか、承認済みcontract変更として扱う。

### 8.2 Platformとの責務境界

Platformが担当:

- tenant、project、actor scope
- RBAC、Policy、Virtual Key、budget
- Logical Modelとdeployment
- model approvalとlicense review
- Conversation MemoryとContext Builder
- node/pool routing
- usage aggregation、audit
- load/unloadの指示と状態管理

Engineが担当:

- signed model artifact検証
- model instance load/unload
- tokenize、prefill、decode、sampling
- request scheduler、batch、deadline、cancel
- KV Cache、GPU/CPU memory
- stream、usage、timing
- engine health、capacity、metrics

Gatewayが担当:

- 外部OpenAI互換API
- client認証、RBAC、rate limit
- data class、Policy入口判定
- SSE/JSON外部応答

GatewayはEngine Control APIを利用しない。Model ManagerのみがControl APIを利用する。Inference OrchestratorだけがEngine Data APIへ推論要求を送る。

### 8.3 As-Built確認項目

実装作業前に次を実Engineから取得し、互換性表を作成する。

- Engine semantic version、build ID、commit/tag
- Data API version、Control API version、transport
- Engine ABI、Architecture Plugin version
- Manifest schema version
- 対応model family、architecture、precision、weight format
- 対応GPU、driver、CUDA/ROCm、OS、CPU architecture
- Generate、Stream、Cancel、CountTokens、Capabilitiesの実装状況
- Load、Unload、Drain、Resume、Capacity、Statusの実装状況
- error code、deadline、cancel、stream event順序
- certified profile、golden test、benchmark、soak test
- package署名、SBOM、provenance、rollback artifact

未実装能力を実装済みとしてPlatform UIやAPIへ公開しない。capability discoveryとfeature flagで制御する。

### 8.4 Engine内部構成（参照）

次は責務理解のための参照であり、本書から再開発しない。

~~~text
Lykuro Native Inference Engine
├── Internal API Server
├── Model Loader
├── Tokenizer Manager
├── Request Scheduler
├── Batch Manager
├── KV Cache Manager
├── Generation Engine
├── Hardware Backend
│   ├── NVIDIA CUDA
│   ├── AMD ROCm
│   └── CPU
├── Model Architecture
└── Metrics / Diagnostics
~~~

Engineは第三者推論Runtimeのsource、binary、container、schedulerを組み込まない。低レベルSDKはEngine SBOMとlicense審査の対象とする。

### 8.5 Engine Data API

Data APIはInference Orchestratorだけが利用する。実装がgRPCの場合はRPC名、HTTPの場合はversioned pathをcontractへ固定する。

| Operation | 用途 | 呼出主体 |
|---|---|---|
| Generate | 非streaming生成 | Inference Orchestrator |
| GenerateStream | streaming生成 | Inference Orchestrator |
| Cancel | request取消 | Inference Orchestrator |
| CountTokens | model固有token数 | Context Builder/Orchestrator |
| GetCapabilities | load済みinstance能力 | Model Manager/Orchestrator |

### 8.6 Engine Control API

| Operation | 用途 | 呼出主体 |
|---|---|---|
| LoadModel | 承認済み署名artifactをload | Model Managerのみ |
| UnloadModel | model instanceをunload | Model Managerのみ |
| Drain | 新規request受付停止 | Model Managerのみ |
| Resume | 受付再開 | Model Managerのみ |
| GetModelStatus | model state | Model Manager |
| GetCapacity | device、VRAM、queue | Model Manager/Coordinator |
| GetManifest | manifest metadata | Model Manager |

Data APIとControl APIはpublic networkへ公開しない。service identityごとにauthorization scopeを分離し、Gateway service identityへControl scopeを付与しない。

### 8.7 Model load検証

1. model approval確認
2. artifact manifest署名確認
3. digest確認
4. license review確認
5. architecture/format/version確認
6. hardware capacity確認
7. staging load
8. smoke inference
9. availableへ切替

不明なmodelをbest effortでloadしない。

### 8.8 Platform↔Engine互換性

| 項目 | 方針 |
|---|---|
| Platform↔Engine Data API | N/N-1または実装済みcontractに従う |
| Model Manager↔Control API | version rangeをrelease manifestへ記録 |
| Engine↔Plugin ABI | Engine manifestで許可rangeを宣言 |
| Engine↔Model Artifact | certified matrixに存在する組合せだけ許可 |
| Engine↔GPU Driver | certified profileに存在する組合せだけ許可 |

統合packageは次のmatrixを必須とする。

~~~text
platform_version
gateway_version_range
gateway_platform_contract_version
engine_version_range
engine_data_api_version
engine_control_api_version
manifest_schema_version
supported_model_profiles
~~~

---

## 9. Distributed Local LLM Pool

### 9.1 目的

複数のlocal LLM nodeを一つのlogical resourceとして利用し、throughput、可用性、業務分担を向上させる。

### 9.2 分散Mode

| Mode | 内容 | 1request高速化 | 優先 |
|---|---|---:|---:|
| Request Sharding | request単位でnodeへ分散 | × | Phase 1 |
| Task Decomposition | documentやjobを分割 | 条件付き | Phase 2 |
| Multi-Model Collaboration | 作成、検証、統合を別modelで実行 | × | Phase 2 |
| Model Parallelism | 一つのmodelを複数GPU/nodeへ分割 | ○ | 将来 |

複数PCのVRAMは自動的に合算されない。Model Parallelismにはengine対応と高速networkが必要である。

### 9.3 Node Agent

Node Agentが報告する項目:

- node_id
- site_id、network_zone
- CPU architecture
- GPU type/count
- VRAM total/free
- system memory
- loaded models
- model capabilities
- concurrency
- queue depth
- TTFT、tokens/sec
- health
- software/config version
- last_seen_at

本文をheartbeatへ含めない。

### 9.4 Scheduler

選択条件:

1. tenant/project policy
2. data classとnetwork zone
3. Logical Model
4. capability
5. model load状態
6. context length
7. free VRAM
8. queue、concurrency
9. TTFT、tokens/sec
10. node health
11. affinity/KV cache
12. cost/priority

Policy条件をperformanceより優先する。

### 9.5 Sticky Routing

同一conversation、同一model、同一configで有効なKV cacheがある場合は同じnodeを優先できる。

- Sticky Routingは最適化であり、正しい会話記憶の条件にしない。
- node障害時はConversation Memoryからcontextを再構築する。
- sticky期限とcache generationを検証する。

### 9.6 Task Decomposition

~~~text
Job
├── Task 1 -> Node A
├── Task 2 -> Node B
├── Task 3 -> Node C
└── Aggregation -> Review Node
~~~

- task入力と結果のdata classを継承する。
- aggregation promptへ必要最小限の結果だけを渡す。
- task単位のrequest ID、parent job IDを記録する。
- 一部失敗、retry、timeout、cancelを明示する。
- 同じtaskを二重確定しない。

### 9.7 Multi-Model Collaboration

例:

- generator model
- factual checker
- security/legal checker
- final synthesizer

異なるmodelの回答を単純結合せず、役割、入力、出力schema、採用条件をworkflowとして定義する。

### 9.8 Network要件

| 利用 | Network |
|---|---|
| Request Sharding | 通常の顧客LANで利用可能 |
| Task Decomposition | 通常LAN、data sizeに依存 |
| Multi-Model Collaboration | 通常LAN、本文転送policyに注意 |
| Multi-node Model Parallelism | 高速Ethernet、RDMA、InfiniBand等を個別設計 |

Internet越しのTensor Parallelismを標準機能にしない。

---

## 10. Conversation Memory

### 10.1 基本方針

LLM nodeは会話の正本を保持しない。Conversation Memory Serviceが顧客環境内で会話履歴を管理し、推論ごとにContext Builderが必要なcontextを作成する。

### 10.2 Mode

| Mode | 本文保存 | 用途 |
|---|---:|---|
| Stateless API | 0日 | API callerがcontextを毎回送る |
| Managed Conversation | policy期間 | Lykuroが会話を管理 |
| Session Only | session終了まで | 一時的な対話 |
| Structured Memory | 明示保存 | 確定した設定・事実 |

Managed ConversationとStructured Memoryは明示的opt-inとする。

### 10.3 処理Flow

1. clientがconversation_idとmessageを送る。
2. Gatewayがtenant、project、user、conversation accessを検証する。
3. Conversation Memoryがversionと過去messageを取得する。
4. Context Builderがmodel別token budgetを計算する。
5. system、policy、memory、summary、recent message、今回入力を組み立てる。
6. Routerがnodeを選択する。
7. inference結果をstreamする。
8. 正常確定後にmessageを保存する。
9. summary/memory更新jobを起動する。
10. retentionとdeletion scheduleを更新する。

### 10.4 Context構成順

~~~text
System Instruction
+ Security / Tenant Policy
+ Developer Instruction
+ Explicit Structured Memory
+ Conversation Summary
+ Relevant Past Messages
+ Recent Messages
+ Current User Message
~~~

優先順位:

1. security/system policy
2. current message
3. recent conversation
4. explicit structured memory
5. summary
6. older relevant messages

### 10.5 Token Budget

~~~text
available_input_tokens
= model_context_window
- reserved_output_tokens
- system_policy_tokens
- safety_margin_tokens
~~~

- model変更時は新しいtokenizerで再計算する。
- token上限超過時は古いmessageからsummaryへ置換する。
- system/security instructionをtruncateしない。
- summaryの生成model、version、source rangeを記録する。
- summaryだけを唯一の監査証跡にしない。

### 10.6 Conversation concurrency

- message sequenceを単調増加させる。
- conversation versionでoptimistic lockingを行う。
- 同一Idempotency-Keyの二重保存を防止する。
- 同時branchを許可する場合はparent_message_idを持つ。
- streaming途中の回答はpendingとして扱い、完了・中断を記録する。

### 10.7 KV Cache

KV CacheはGPU/RAM上の一時的なperformance cacheである。

| 項目 | Conversation Memory | KV Cache |
|---|---|---|
| 正本 | ○ | × |
| 永続性 | policyに従う | 一時 |
| node障害後 | 復旧可能 | 原則消失 |
| node間共有 | DBから再構築 | 標準では不可 |
| 内容 | message/summary | token計算状態 |

---

## 11. 保存期間・削除

### 11.1 標準保存期間

| Data | 標準 | 備考 |
|---|---:|---|
| Stateless API prompt/response | 0日 | 保存しない |
| Managed Conversation message | 30日 | opt-in時のみ |
| Conversation summary | 30日 | 元会話と連動 |
| Attachment | 30日 | 元会話と連動 |
| Structured Memory | 明示期限 | 無期限を既定にしない |
| KV Cache | 5〜30分 | 最大1時間を初期上限 |
| Error diagnostics | 30日 | 本文・secretなし |
| Audit metadata | 365日 | request ID、actor、result |
| Usage metadata | 契約・請求要件 | 本文なし |
| Config/policy history | 契約期間＋1年 | 変更証跡 |
| License history | 契約期間＋1年 | model利用証跡 |

### 11.2 選択可能なRetention

~~~text
0日
1日
7日
30日（標準）
90日
365日
契約指定
~~~

### 11.3 Data Class別初期値

| Class | Conversation保存 |
|---|---|
| public | tenant policy |
| internal | 30日 |
| confidential | 0〜30日、30日超は追加承認 |
| restricted | 原則0日 |

### 11.4 削除

- user/administrator削除時はactive DBから即時にlogical deleteし、非同期でphysical deleteする。
- message、summary、embedding、attachment、cache、derived artifactを連動削除する。
- active storageから24時間以内のphysical deletionを目標とする。
- backupからは最大30日以内にexpirationさせる。
- legal hold対象は専用flagと承認・auditを必須とする。
- deletion jobの成功、失敗、retryを監査する。
- Control Planeへ本文削除要求の本文を送らない。

### 11.5 Retention変更

- 短縮は既存dataへ適用し、期限超過dataを削除jobへ送る。
- 延長は変更後に作成されたdataを基本とし、既存data延長は明示承認とする。
- 変更者、旧値、新値、理由、effective_atを記録する。

---

## 12. Security・Privacy

### 12.1 Strict Local Mode

顧客環境外へ送信しないもの:

- prompt、response
- system/developer message
- conversation summary
- structured memory
- attachment
- RAG result
- tool input/output
- embedding vectorと元本文

### 12.2 Control Planeへ送信可能な項目

allowlist:

~~~text
tenant_id
gateway_id
node_id
logical_model_id
software_version
config_version
request_count
input_tokens
output_tokens
latency_ms
result_code
health_status
capacity_bucket
timestamp
~~~

### 12.3 Internal通信

- Gateway、Model Manager、Coordinator、Node Agent、Native Engine間をmTLSまたは相互認証する。
- internal endpointをpublic networkへ公開しない。
- service identityごとにscopeを分離する。
- certificate rotationを停止なしで行う。

### 12.4 Model artifact

- signed manifest
- SHA-256または承認hash
- weight format validation
- license review
- malware/archive traversal検査
- read-only mount
- model pathを外部入力から直接使用しない

### 12.5 Secret

- secret manager/Vault/Kubernetes Secretを利用する。
- DBにはcredential referenceだけを保存する。
- log、metric、trace、diagnoseへsecretを含めない。
- connector credentialをtenant間で共有しない。

### 12.6 Fail Closed

次が不明な場合は拒否する。

- tenant/project
- actorとpermission
- conversation access
- data class
- model approval
- routing permission
- config signature
- Runtime endpoint identity

### 12.7 既存Gateway認証・認可の継承

Platform外部利用者の認証は既存Gatewayだけが行う。

| 主体 | 既存Gateway方式 | Platformへの伝播 |
|---|---|---|
| 社内application | Virtual Key + network policy | tenant/project/scopeの内部token |
| End User | OIDC/JWT | actor ID、role、tenant/project scope |
| 高機密連携 | mTLS + JWTまたはKey | verified service identity |
| Control Agent | 端末identity + signed token | control scopeのみ |
| Platform service | mTLS service identity | 最小権限service scope |

PlatformはVirtual KeyまたはJWT原文を保存しない。Gatewayが発行する短寿命の内部identityまたは相互TLS identityを検証する。

### 12.8 RBAC

既存Gatewayの企業roleを拡張し、同義role体系を作らない。

| 操作 | Tenant Owner | AI Admin | Security Auditor | Project Manager | Developer | Billing Viewer |
|---|---:|---:|---:|---:|---:|---:|
| Model/Runtime参照 | ○ | ○ | ○ | 担当範囲 | 許可範囲 | × |
| Model登録・審査依頼 | ○ | ○ | 参照 | × | × | × |
| Model承認・停止 | ○ | ○ | 参照 | × | × | × |
| Native Engine load/unload | ○ | ○ | 参照 | × | × | × |
| Node/Pool変更 | ○ | ○ | 参照 | × | × | × |
| Retention変更 | ○ | ○ | 参照 | × | × | × |
| Conversation本文閲覧 | tenant policy | 明示権限時のみ | 既定不可 | 担当範囲 | 自分の範囲 | × |
| Audit export | ○ | ○ | ○ | 担当範囲 | × | × |
| Usage/Billing参照 | ○ | ○ | ○ | 担当範囲 | 自分の範囲 | ○ |

すべてserver-sideでtenant scopeとroleを検証する。UI非表示だけで権限制御しない。

### 12.9 Policy評価順

1. Gatewayが認証、tenant/project、rate limit、budgetを検証する。
2. Gateway Policyがdata class、local-only、allowed model/capabilityを確定する。
3. Platform RouterがGatewayの許可範囲内でdeployment/nodeを選択する。
4. Engineがlocal capacity、input limit、generation parameterを検証する。
5. どの層も上位Policyを緩和しない。

外部CloudへのfallbackはGateway Policyで明示的に許可され、本書のStrict Local制約と契約条件を満たす別Edition以外では禁止する。

### 12.10 Virtual Key

Virtual Keyは既存Gateway機能を利用する。Platform側にはkey原文またはkey hash tableを重複作成しない。Gatewayから次の解決済み属性だけを受け取る。

- tenant/project
- allowed logical models
- capability scope
- RPS/concurrency/token上限
- environment
- expiration/revocation state
- actor/service識別子

key失効はGatewayで即時適用し、Platformが独自cacheした認証結果で失効を回避しない。

---

## 13. 外部・内部API

### 13.1 Gateway API

次の外部APIは実装済みGatewayが提供する。Platformは同じpublic pathを新設せず、Gateway内部adapterのroute先になる。

| Method | Path | 用途 |
|---|---|---|
| GET | /v1/models | 許可Logical Model |
| POST | /v1/chat/completions | Stateless/Conversation chat |
| POST | /v1/responses | Gateway実装状況とfeature flagに従う |
| POST | /v1/embeddings | 対応deploymentのみ |

Conversation利用時の論理header:

~~~text
X-Lykuro-Conversation-ID
X-Lykuro-Conversation-Version
X-Lykuro-Retention-Policy
~~~

client指定値よりtenant policyを優先する。

Gateway APIのrequest/response/error/SSE互換性はLYK-PLG-BD-001と既存contractを正本とする。Platform内部のphysical model名、endpoint、credential、node IP、Engine build pathを外部へ返さない。

### 13.1.1 Gateway→Platform Error Mapping

| Platform code | Gateway HTTP | 説明 |
|---|---:|---|
| platform_invalid_request | 400 | schema、parameter、context不正 |
| conversation_conflict | 409 | conversation version競合 |
| model_not_allowed | 403 | policy/model不許可 |
| model_not_available | 503 | deployment/node/Engine利用不可 |
| capacity_exhausted | 429 | queue、VRAM、concurrency超過 |
| inference_timeout | 504 | deadline超過 |
| platform_not_ready | 503 | config、migration、dependency未準備 |
| internal_contract_error | 502 | Gateway↔PlatformまたはPlatform↔Engine契約不一致 |

stream開始後はHTTP statusを変更できないため、既存GatewayのSSE error形式へ変換して終了する。自動再試行で二重生成しない。

### 13.2 Model Manager API

| Method | Path | 用途 |
|---|---|---|
| GET | /api/enterprise/models | model一覧 |
| POST | /api/enterprise/models | model登録 |
| POST | /api/enterprise/models/{id}/review | 審査 |
| POST | /api/enterprise/models/{id}/approve | 承認 |
| POST | /api/enterprise/models/{id}/deployments | 配置 |
| POST | /api/enterprise/models/{id}/suspend | 停止 |
| GET | /api/enterprise/runtime-endpoints | Runtime一覧 |
| POST | /api/enterprise/runtime-endpoints | Runtime登録 |
| POST | /api/enterprise/runtime-endpoints/{id}/test | 接続試験 |

### 13.3 Distributed Pool API

| Method | Path | 用途 |
|---|---|---|
| GET | /api/enterprise/llm-nodes | node一覧 |
| GET | /api/enterprise/llm-pools | pool一覧 |
| POST | /api/enterprise/llm-pools | pool作成 |
| PATCH | /api/enterprise/llm-pools/{id} | scheduler policy |
| POST | /api/enterprise/llm-pools/{id}/drain | 新規受付停止 |
| GET | /api/enterprise/llm-pools/{id}/jobs | job一覧 |

### 13.4 Conversation API

| Method | Path | 用途 |
|---|---|---|
| POST | /api/conversations | 会話作成 |
| GET | /api/conversations/{id} | metadata |
| GET | /api/conversations/{id}/messages | 履歴 |
| DELETE | /api/conversations/{id} | 連動削除 |
| POST | /api/conversations/{id}/export | audit付きexport |
| GET | /api/enterprise/retention-policies | policy一覧 |
| POST | /api/enterprise/retention-policies | policy作成 |

### 13.5 Node Agent API

| Method | Path | 用途 |
|---|---|---|
| POST | /api/node-agent/v1/register | node登録 |
| POST | /api/node-agent/v1/heartbeat | capacity/health |
| POST | /api/node-agent/v1/model-inventory | model報告 |
| GET | /api/node-agent/v1/jobs | job取得 |
| POST | /api/node-agent/v1/jobs/{id}/ack | job結果 |

### 13.6 Platform Control Agent API

既存Gateway Control Agentを拡張できる場合は同じdevice identityと通信frameworkを再利用する。別Agentを作る場合も同義登録を重複させず、gateway_deployment_idへ紐づける。

| Method | Path | 用途 |
|---|---|---|
| POST | /api/platform-agent/v1/register | Platform add-on登録 |
| GET | /api/platform-agent/v1/config | signed config取得 |
| POST | /api/platform-agent/v1/config-ack | staging/active結果 |
| POST | /api/platform-agent/v1/heartbeat | component health/version |
| POST | /api/platform-agent/v1/usage | 本文なしusage metadata |
| GET | /api/platform-agent/v1/releases | compatible release確認 |

### 13.7 API共通要件

- tenant scope
- server-side RBAC
- request ID
- Idempotency-Key
- optimistic lock
- pagination
- rate limit
- audit
- uniform error
- secret/content redaction

---

## 14. 論理データモデル

既存entityがある場合は拡張し、同義tableを重複作成しない。

### 14.0 既存Gateway entityの参照

次は既存Gateway/app.lykuro.aiのentityを外部keyまたは既存repository経由で参照し、新設しない。

| Existing entity | Platform利用 |
|---|---|
| tenant/organization | 全Platform dataのscope |
| user/role | 管理操作とconversation access |
| project | model、budget、data class、conversation scope |
| virtual_key | Gatewayで認証し解決済みscopeだけ受領 |
| policy/config version | route、retention、model許可の正本 |
| request_audit | Platform eventをrequest_idで関連付け |
| usage | token、GPU、route metadataを既存集計へ連携 |
| gateway_deployment | Platform deploymentとの互換versionを関連付け |

### 14.1 model_catalog

~~~text
id                  UUID/ULID PK
tenant_id           FK NOT NULL
logical_name        VARCHAR NOT NULL
display_name        VARCHAR NOT NULL
family              VARCHAR NOT NULL
version             VARCHAR NOT NULL
artifact_format     VARCHAR NULL
artifact_digest     VARCHAR NULL
capabilities_json   JSON/JSONB NOT NULL
license_review_id   FK NULL
approval_status     ENUM
status              ENUM
created_by          FK NOT NULL
created_at          TIMESTAMP NOT NULL
updated_at          TIMESTAMP NOT NULL
~~~

### 14.2 runtime_endpoints

~~~text
id                  UUID/ULID PK
tenant_id           FK NOT NULL
name                VARCHAR NOT NULL
connector_type      VARCHAR NOT NULL
base_url            VARCHAR NOT NULL
network_zone        VARCHAR NULL
credential_ref      VARCHAR NULL
management_mode     ENUM
status              ENUM
last_health_at      TIMESTAMP NULL
created_by          FK NOT NULL
created_at          TIMESTAMP NOT NULL
updated_at          TIMESTAMP NOT NULL
~~~

### 14.3 model_deployments

~~~text
id                  UUID/ULID PK
tenant_id           FK NOT NULL
model_id            FK NOT NULL
runtime_endpoint_id FK NULL
node_pool_id        FK NULL
backend_type        ENUM(native_engine, external_connector) NOT NULL
deployment_config   JSON/JSONB NOT NULL
status              ENUM
loaded_at           TIMESTAMP NULL
last_error_code     VARCHAR NULL
created_at          TIMESTAMP NOT NULL
updated_at          TIMESTAMP NOT NULL
~~~

### 14.4 llm_nodes

~~~text
id                  UUID/ULID PK
tenant_id           FK NOT NULL
site_id             VARCHAR NULL
network_zone        VARCHAR NULL
node_identity       VARCHAR NOT NULL
hardware_json       JSON/JSONB NOT NULL
capacity_json       JSON/JSONB NOT NULL
health_status       ENUM
software_version    VARCHAR NULL
config_version      VARCHAR NULL
last_seen_at        TIMESTAMP NULL
created_at          TIMESTAMP NOT NULL
~~~

### 14.5 llm_pools

~~~text
id                  UUID/ULID PK
tenant_id           FK NOT NULL
name                VARCHAR NOT NULL
pool_type           ENUM(request, task, collaboration, model_parallel)
scheduler_policy    JSON/JSONB NOT NULL
allowed_classes     JSON/JSONB NOT NULL
status              ENUM
created_at          TIMESTAMP NOT NULL
updated_at          TIMESTAMP NOT NULL
~~~

### 14.6 inference_jobs

~~~text
id                  UUID/ULID PK
tenant_id           FK NOT NULL
parent_job_id       FK NULL
conversation_id     FK NULL
logical_model_id    FK NOT NULL
selected_node_id    FK NULL
job_type            ENUM(inference, task, aggregate, review)
status              ENUM
attempt             INTEGER NOT NULL
request_id          VARCHAR NOT NULL
input_ref           VARCHAR NULL
output_ref          VARCHAR NULL
error_code          VARCHAR NULL
created_at          TIMESTAMP NOT NULL
completed_at        TIMESTAMP NULL
~~~

input_ref/output_refは顧客環境内の暗号化storage参照とし、Control Planeへ送信しない。

### 14.7 conversations

~~~text
id                  UUID/ULID PK
tenant_id           FK NOT NULL
project_id          FK NOT NULL
owner_id            FK NOT NULL
title               VARCHAR NULL
retention_policy_id FK NOT NULL
data_class          ENUM NOT NULL
version             INTEGER NOT NULL
status              ENUM(active, archived, deleting, deleted)
expires_at          TIMESTAMP NULL
created_at          TIMESTAMP NOT NULL
updated_at          TIMESTAMP NOT NULL
~~~

### 14.8 messages

~~~text
id                  UUID/ULID PK
tenant_id           FK NOT NULL
conversation_id     FK NOT NULL
parent_message_id   FK NULL
sequence            BIGINT NOT NULL
role                ENUM(system, developer, user, assistant, tool)
content_ciphertext  BLOB/TEXT NOT NULL
content_hash        VARCHAR NOT NULL
token_count         INTEGER NULL
model_id            FK NULL
request_id          VARCHAR NOT NULL
status              ENUM(pending, complete, interrupted, deleted)
expires_at          TIMESTAMP NULL
created_at          TIMESTAMP NOT NULL
~~~

### 14.9 conversation_summaries

~~~text
id                  UUID/ULID PK
tenant_id           FK NOT NULL
conversation_id     FK NOT NULL
source_from_seq     BIGINT NOT NULL
source_to_seq       BIGINT NOT NULL
summary_ciphertext  BLOB/TEXT NOT NULL
generator_model_id  FK NOT NULL
generator_version   VARCHAR NOT NULL
expires_at          TIMESTAMP NOT NULL
created_at          TIMESTAMP NOT NULL
~~~

### 14.10 structured_memory

~~~text
id                  UUID/ULID PK
tenant_id           FK NOT NULL
project_id          FK NOT NULL
owner_id            FK NULL
memory_type         VARCHAR NOT NULL
value_ciphertext    BLOB/TEXT NOT NULL
source_message_id   FK NULL
confirmed_by        FK NULL
expires_at          TIMESTAMP NOT NULL
status              ENUM(active, revoked, deleted)
created_at          TIMESTAMP NOT NULL
~~~

### 14.11 retention_policies

~~~text
id                  UUID/ULID PK
tenant_id           FK NOT NULL
name                VARCHAR NOT NULL
message_days        INTEGER NOT NULL
summary_days        INTEGER NOT NULL
attachment_days     INTEGER NOT NULL
kv_cache_minutes    INTEGER NOT NULL
audit_days          INTEGER NOT NULL
backup_max_days     INTEGER NOT NULL
allowed_classes     JSON/JSONB NOT NULL
status              ENUM
created_by          FK NOT NULL
created_at          TIMESTAMP NOT NULL
updated_at          TIMESTAMP NOT NULL
~~~

### 14.12 deletion_jobs

~~~text
id                  UUID/ULID PK
tenant_id           FK NOT NULL
resource_type       VARCHAR NOT NULL
resource_id         VARCHAR NOT NULL
reason              VARCHAR NOT NULL
status              ENUM(queued, running, completed, failed)
attempt             INTEGER NOT NULL
scheduled_at        TIMESTAMP NOT NULL
completed_at        TIMESTAMP NULL
error_code          VARCHAR NULL
created_at          TIMESTAMP NOT NULL
~~~

### 14.13 Data制約

- 全tableにtenant scopeを強制する。
- unique keyへtenant_idを含める。
- 本文を平文columnへ保存しない。
- vector、summary、attachmentへ同じretentionを伝播する。
- soft deleteだけで完了としない。
- node heartbeatへ本文を含めない。
- Runtime credential原文を保存しない。
- Gateway既存entityをPlatform migrationで再作成しない。
- Gateway request_idをPlatform、Engine、Memory、Auditでend-to-end維持する。
- GatewayとPlatformのtenant_id型・生成規則を統一する。
- API、config、manifest、schemaのversionをdeploymentへ記録する。

### 14.14 platform_deployments

~~~text
id                                  UUID/ULID PK
tenant_id                           FK NOT NULL
gateway_deployment_id               FK NOT NULL
edition                             ENUM
platform_version                    VARCHAR NOT NULL
gateway_contract_version            VARCHAR NOT NULL
engine_data_api_version             VARCHAR NULL
engine_control_api_version          VARCHAR NULL
manifest_schema_version             VARCHAR NOT NULL
status                              ENUM(draft, installing, online, degraded, updating, offline, revoked)
last_seen_at                        TIMESTAMP NULL
created_at                          TIMESTAMP NOT NULL
updated_at                          TIMESTAMP NOT NULL
version                             INTEGER NOT NULL
~~~

Gateway deploymentとPlatform deploymentは1対1または明示した1対多関係とし、tenantを跨ぐ関連付けを禁止する。

---

## 15. app.lykuro.ai画面仕様

既存Gateway管理画面、企業navigation、認証、design systemを再利用する。「Private Gateway」と別に同義の導入画面を作らず、Gateway詳細から「Native LLM Platform」tabまたは配下navigationへ遷移させる。

### 15.0 統合navigation

~~~text
Enterprise
└── Private LLM
    ├── Gateway（既存画面）
    ├── Models
    ├── Runtime Endpoints
    ├── Native Engine
    ├── Nodes / Pools
    ├── Conversations / Retention
    ├── Audit
    └── Usage
~~~

Gateway status、version、last seen、licenseは既存画面の情報を利用し、Platform画面へ複製保存しない。

### 15.1 Model Manager

- model一覧
- version、digest、format、capability
- license/approval
- deployment、Runtime、node
- health、capacity
- suspend、rollback

### 15.2 Runtime Endpoint

- endpoint name
- connector type
- network zone
- management mode
- health
- last test
- credential reference状態

base URLやcredentialの閲覧権限を制限する。

### 15.3 LLM Node・Pool

- node status、GPU、VRAM、model、queue
- pool type、scheduler policy
- drain、resume
- job、error、failover
- site/network zone filter

### 15.4 Conversation・Retention

- retention policy一覧
- data class別保存期間
- conversation memory ON/OFF
- active conversation数
- expiry予定件数
- deletion job結果
- export/delete audit

Security Auditorへ本文を既定表示しない。

### 15.5 Native Engine

Native Editionだけに表示:

- engine version
- supported model/format
- loaded model
- GPU/VRAM
- queue、TTFT、tokens/sec
- update、rollback

実装済みEngineから取得できない項目を推測表示しない。capability APIまたはAs-Built matrixに基づいて表示し、未対応は明示する。

### 15.6 Platform Overview

- Gateway、Platform Controller、Model Manager、Memory、Coordinator、Engineの状態
- Gateway contract、Platform、Engine API、configのversion整合性
- active logical model、deployment、node、pool
- Strict Local、Conversation Memory、Retentionの有効状態
- pending update、degraded reason、last successful sync
- diagnose、upgrade、rollbackへの導線

個人userへ表示せず、企業userでも既存RBACに従う。

### 15.7 Platform状態

~~~mermaid
stateDiagram-v2
    [*] --> draft
    draft --> installing
    installing --> registered
    installing --> failed
    registered --> online
    online --> degraded
    degraded --> online
    online --> updating
    updating --> online
    updating --> rollback
    rollback --> online
    online --> offline
    offline --> online
    online --> revoked
~~~

状態はGateway状態を上書きしない。Gateway online、Platform degraded、Engine unavailableのようにcomponent別状態と集約状態を分離表示する。

---

## 16. 配布・導入

配布単位はLykuro Native LLM Platformとする。ただし既存Gateway artifactを再ビルドまたはforkせず、互換性が確認された既存releaseを統合manifestで参照する。

導入方式:

| 方式 | 対象 | 方針 |
|---|---|---|
| Existing Gateway Add-on | Gateway導入済み顧客 | Platform componentとGateway Adapterだけを追加 |
| Unified Fresh Install | 新規顧客 | 既存Gateway releaseとPlatformを一つのsigned bundleで導入 |
| Kubernetes | Enterprise | 独立Deployment/Service、NetworkPolicy、Rolling/Blue-Green |
| Air-Gapped | 閉域 | OCI tar、config、license、SBOM、署名をoffline import |

### 16.1 Connector Edition package

含める:

- 互換性確認済みLykuro Gateway artifactまたは既存Gateway参照
- Gateway Platform Adapter
- Platform Controller / Inference Orchestrator
- Lykuro Model Manager
- Runtime Connector
- Control Agent
- CLI
- config schema
- signature、checksum、SBOM
- documentation

含めない:

- third-party Runtime
- third-party Runtime image
- model weight
- GPU driver
- Python/Node runtime

### 16.2 Native Runtime Edition package

追加で含める:

- Lykuro Native Inference Engine
- Lykuro-owned model architecture module
- approved low-level dependency notice
- engine SBOM

model weightは別の承認済みartifactとして配布・importする。

### 16.3 Distributed Edition package

追加で含める:

- Distributed Coordinator
- Node Agent
- node registration CLI
- network/security checklist

### 16.4 Runtime Endpoint導入Flow

1. 顧客がRuntimeを用意する。
2. 顧客networkでendpointとcertificateを設定する。
3. app.lykuro.aiまたはlocal consoleでendpoint metadataを登録する。
4. Control Agentが署名済みconfigを取得する。
5. Model Managerが接続、TLS、capabilityをtestする。
6. AI Administratorがmodel mappingを承認する。
7. Gateway routeを有効化する。

Lykuroインストーラーは顧客Runtimeを変更しない。

### 16.5 統合package構成

~~~text
lykuro-native-llm-platform/
├── lykuro-platform
├── lykuro-gateway-adapter
├── compose/
│   └── docker-compose.yml
├── helm/
│   └── lykuro-native-llm-platform/
├── config/
│   ├── platform.example.yaml
│   ├── model-manager.example.yaml
│   └── retention.example.yaml
├── contracts/
│   ├── gateway-platform-v1.*
│   └── platform-engine-v1.*
├── compatibility/
│   └── matrix.json
├── images/
│   └── offline-images.tar
├── manifest.json
├── checksums.sha256
├── signature.sig
├── sbom/
└── docs/
    ├── INSTALL.md
    ├── UPGRADE.md
    ├── ROLLBACK.md
    └── SECURITY.md
~~~

Native Editionは実装済みNative Engineの署名済みartifactとengine SBOMを追加する。model weightは原則として別artifactにする。

### 16.6 統合manifest

~~~json
{
  "schema_version": "2",
  "product": "lykuro-native-llm-platform",
  "platform_version": "2.0.0",
  "edition": "native",
  "gateway": {
    "required_version": ">=1.1.0 <2.0.0",
    "contract_version": "gateway-platform-v1"
  },
  "engine": {
    "required_version": "as-built-compatible-range",
    "data_api": "v1",
    "control_api": "v1"
  },
  "strict_local_mode": true,
  "files": []
}
~~~

production値はrepository調査と実際のreleaseから生成し、例示versionをそのまま固定しない。manifestへsecret、token、credential、本文を含めない。

### 16.7 Installer・CLI再利用

既存lykuro-gateway CLIを後方互換で拡張するか、同じframeworkでlykuro-platform CLIを追加する。認証、package検証、診断、更新処理を重複実装しない。

~~~text
lykuro-platform precheck
lykuro-platform install --attach-gateway <gateway-id>
lykuro-platform register
lykuro-platform status
lykuro-platform health
lykuro-platform models list
lykuro-platform nodes list
lykuro-platform config validate
lykuro-platform diagnose
lykuro-platform upgrade
lykuro-platform rollback
lykuro-platform uninstall
~~~

絶対条件:

- 顧客hostへNode.js、npm、nvmを要求しない。
- sudoを前提にしない。rootlessまたは管理者が明示実行する方式を用意する。
- OS package、GPU driver、Docker、Kubernetesを無断導入・更新しない。
- curl URL | shを企業向け正式導入手順にしない。
- precheckはread-onlyとする。
- 再実行してもGateway再登録、重複resource、設定破壊を起こさない。
- uninstallは既存Gateway、顧客Runtime、model weight、auditを既定で削除しない。

### 16.8 Precheck

- 既存Gateway version、health、config、contract能力
- Platform DB/schemaとmigration状態
- Engine version、Data/Control API、capabilities
- OS、kernel、CPU architecture、memory、disk
- GPU、driver、CUDA/ROCm、VRAMとcertified profile
- container runtimeまたはKubernetes/Helm
- port、DNS、proxy、NTP、TLS/mTLS
- secret mount、write permission、backup領域
- Control Plane HTTPS 443 outboundまたはoffline mode
- third-party Runtime endpointは設定時だけconnectivity検査

不整合を検出した場合は変更せず、component、actual、required、remediationを報告する。

### 16.9 初回登録・設定配信

1. app.lykuro.aiの既存Gateway詳細からPlatform追加packageと1回限りtokenを発行する。
2. installerがpackage、manifest、署名、checksum、tenant/gateway bindingを検証する。
3. 既存Gateway identityを再利用してPlatform deploymentを登録する。
4. Platform service identityとmTLS certificateを発行または顧客PKIから設定する。
5. signed configをstagingへ取得する。
6. schema、Gateway contract、Engine API、model、networkを検査する。
7. success時だけatomicにactiveへ切り替える。
8. Gateway routeをfeature flagでcanary有効化する。
9. smoke inference、stream、cancel、audit、usageを確認する。
10. Last Known Goodを保存する。

install token原文をDB、log、shell history、diagnoseへ残さない。

### 16.10 更新・Rollback

- Gateway、Platform、Engineを独立versionとして扱う。
- 統合manifestで許可組合せを検証する。
- Gateway更新をPlatform更新へ暗黙に含めない。
- Engine更新前にdrainし、model load/smoke/readiness後にtrafficを切り替える。
- DB migrationはexpand/migrate/contractを基本とし、rolling中N/N-1を維持する。
- Gateway Adapterはfeature flagで旧routeへ戻せるようにする。
- health、contract、error rate、TTFT、stream failureが閾値を超えたら自動rollback対象とする。
- rollbackで顧客model weight、conversation、auditを削除しない。

### 16.11 統合設定Schema

~~~yaml
schema_version: "2"
deployment:
  platform_id: plf_example
  gateway_id: gw_example
  edition: native

security:
  strict_local_mode: true
  content_logging: false
  internal_mtls_required: true

gateway_contract:
  version: gateway-platform-v1
  endpoint_ref: internal://gateway-adapter

platform:
  inference_orchestrator:
    max_queue: 256
    default_deadline_seconds: 120

model_manager:
  approval_required: true
  external_runtime_lifecycle: disabled

native_engine:
  data_endpoint_ref: mtls://native-engine-data
  control_endpoint_ref: mtls://native-engine-control
  required_api_version: v1

conversation:
  mode: stateless
  default_retention_days: 0
  kv_cache_minutes: 15

control_plane:
  metadata_allowlist_only: true
~~~

secret値を設定へ含めず、secret manager、Kubernetes Secret、file mountの参照だけを保持する。unknown key、型、range、version、certificate、path、permissionを検証する。

### 16.12 Filesystem

~~~text
/opt/lykuro/platform/          executable/deployment files
/etc/lykuro/platform/          non-secret config
/var/lib/lykuro/platform/      state、Last Known Good、job metadata
/var/log/lykuro/platform/      content-free logs
/var/lib/lykuro/conversations/ encrypted local content（opt-in時のみ）
/run/secrets/                  secret mount
~~~

既存Gateway directoryと共有しない。rootless時はXDG準拠directoryへ配置する。model weightはread-onlyの承認済みmountとする。

---

## 17. Audit・Observability

### 17.1 Audit event

- Gateway→Platform route有効化、無効化、rollback
- Gateway/Platform/Engine contract変更
- model登録、審査、承認、停止
- Runtime endpoint登録、test、credential rotation
- deployment、load、unload、rollback
- node登録、drain、失効
- pool/scheduler変更
- conversation作成、export、delete
- retention変更
- deletion job
- inference route、failover、policy deny

既存Gatewayのrequest_idとaudit pipelineを再利用する。Platform用に別の独立audit正本を作らず、local detail eventと既存audit recordを相関できるようにする。

### 17.2 Metrics

| Category | Metrics |
|---|---|
| Gateway | RPS、4xx/5xx、latency、stream |
| Model Manager | deployment、health、approval |
| Connector | upstream latency、timeout、circuit |
| Native Engine | TTFT、tokens/sec、queue、KV cache、VRAM |
| Distributed | node health、assignment、retry、failover |
| Memory | message、summary、context tokens、expiry |
| Deletion | queued、completed、failed、age |

metric labelへ本文、user入力、secret、conversation titleを含めない。

### 17.3 Trace

- request_id
- parent_job_id
- node_id
- logical/physical model ID
- policy/config version
- conversation_idは必要時のみhash/pseudonymous form

traceへ本文を含めない。

---

## 18. 障害・例外

| 事象 | 処理 |
|---|---|
| Runtime endpoint停止 | circuit open、許可nodeへfailover |
| 全node停止 | 503 model_unavailable |
| Node heartbeat期限超過 | unhealthy、job再配置 |
| GPU OOM | node隔離、queue抑制、503/429 |
| Native model load失敗 | availableへ切替禁止、直前model維持 |
| Conversation DB停止 | managed conversation拒否、statelessを勝手に継続しない |
| Summary生成失敗 | 原文を勝手に削除せずretry |
| Retention job失敗 | alert、retry、期限超過report |
| KV cache消失 | Memoryからcontext再構築 |
| Control Plane停止 | Last Known Goodで継続 |
| Signature不正 | config/model適用拒否 |
| Aggregation一部失敗 | workflow policyに従いpartial/fail |
| Gateway↔Platform contract不一致 | route有効化拒否、旧routeまたはLast Known Good維持 |
| Platform process停止 | Gatewayがbounded 503、circuit open、alert |
| Gateway Adapter stream切断 | Engine cancel、buffer解放、二重retry禁止 |
| Engine API version不一致 | deployment unavailable、load/route禁止 |
| Platform DB migration失敗 | 新version起動拒否、自動rollback |
| Usage送信停止 | 本文なし暗号化bounded queue、上限alert |
| Disk full | 新規保存停止、Statelessへ黙って切替せず明示error |

会話Memory取得失敗時に、履歴なしで回答を続けて利用者へ誤認させない。

### 18.1 Backup・Recovery

| Data | Backup | 復旧方針 |
|---|---|---|
| Platform config | 暗号化、2世代以上 | Last Known Goodへ復帰 |
| Model catalog/approval | 暗号化DB backup | tenant scopeとsignature再検証 |
| Conversation本文 | opt-in時だけ顧客領域 | retentionとbackup expirationを継承 |
| Audit/Usage | 既存Gateway方針を継承 | request_id相関を維持 |
| Model weight | 通常backup対象外 | artifact digestから再mount/import |
| Secret | 一般backupへ含めない | 顧客PKI/secret managerから再発行 |

- RPO/RTOはGateway、Platform metadata、Conversation、Engineで分けて契約する。
- 四半期ごとにconfig、DB、conversation deletion、Engine reload、Gateway route rollbackを訓練する。
- backup restore後も期限切れconversationを復活させず、retention jobを再実行する。
- diagnose bundleに本文、token、KV Cache、weight、credentialを含めない。

---

## 19. 非機能要件

### 19.1 Availability

- GatewayとModel Managerを分離して障害範囲を限定する。
- node failureをpoolから隔離する。
- Conversation Memoryをbackupし、RPO/RTOを契約で定義する。
- Native Engine updateはBlue/GreenまたはRollingを利用する。

### 19.2 Performance

- Gateway追加latencyとRuntime inference時間を分離計測する。
- Context Builderのtoken計算時間を計測する。
- Request Shardingでpool throughputを拡張する。
- sticky routingはcache hit改善として評価する。
- queue、TTFT、tokens/secをmodel/node別に測定する。

### 19.3 Scale

- Model Managerはtenantとmodel数に対して水平拡張可能にする。
- Conversation messageをappend主体とする。
- heartbeatとinference auditを主要transaction DBから分離できるようにする。
- Distributed Coordinatorのsingle point of failureをEnterpriseで排除する。

### 19.4 Compatibility

- Gateway↔Platform contractをversion化する。
- Connector contractをversion化する。
- Native Engine internal APIをversion化する。
- config、model manifest、conversation schemaをversion化する。
- rolling update中のN/N-1互換を定義する。

### 19.5 SLO目標

| 項目 | 初期目標 | 備考 |
|---|---:|---|
| Gateway可用性 | 既存Gateway SLAを継承 | Platform障害を分離計測 |
| Platform Orchestrator可用性 | 99.9%/月 | Enterpriseは契約値 |
| Gateway→Platform追加遅延 | p95 50ms以下 | 推論時間、Context Buildを除く |
| Context Build | p95 100ms以下 | 保存量・tokenizer profile別 |
| 設定反映 | 5分以内 | online mode |
| Engine route failover | 30秒以内を初期目標 | stream途中の透明再送はしない |
| Retention physical delete | active storage 24時間以内 | backup最大30日 |
| 監査RPO | 24時間以内 | Enterpriseは契約値 |

数値は実環境benchmarkと契約で確定する。未測定値を達成済みとしない。

### 19.6 Capacity・Backpressure

- Gateway rate limit、Platform queue、Engine admissionの三段階でboundedにする。
- request数だけでなくinput/output token、active stream、KV/VRAMを制限する。
- queue上限超過は429とRetry-After候補を返す。
- Engine OOMを通常のcapacity制御に使わない。
- conversation/attachment bodyを無制限queueへ保持しない。
- tenant/project priorityはGateway上限を超えない。

### 19.7 推奨repository境界

既存repository構成を優先し、次の論理境界を保つ。

~~~text
lykuro-private-llm-gateway/          # 既存。再実装・fork禁止
├── existing gateway source
└── platform-adapter/                # 必要な最小拡張

lykuro-native-llm-platform/
├── apps/
│   ├── platform-controller/
│   ├── inference-orchestrator/
│   ├── model-manager/
│   ├── conversation-memory/
│   ├── distributed-coordinator/
│   └── node-agent/
├── packages/
│   ├── domain/
│   ├── gateway-contract/
│   ├── engine-contract/
│   ├── runtime-connectors/
│   └── config-schema/
├── deploy/
│   ├── compose/
│   ├── helm/
│   ├── installer/
│   └── offline/
└── docs/

lykuro-native-inference-engine/      # 既存。別project
└── existing engine source
~~~

monorepoの場合もmodule boundary、owner、build、release、API versionを分離する。Gateway/Platform/Engineを同一binaryへ結合しない。

### 19.8 運用責任分界

| 対象 | Lykuro | 顧客 |
|---|---|---|
| Gateway/Platform/Engine software | release、署名、脆弱性修正、互換表 | 承認済みversionの導入・変更管理 |
| OS/Kernel/Container | certified要件提示 | provision、patch、hardening |
| GPU/Driver/CUDA | certified matrix提示 | hardware調達、driver運用 |
| Network/DNS/Proxy/NTP | port/通信要件提示 | 顧客network設定 |
| IdP/PKI/Secret | integration機能 | identity、certificate、rotation |
| Model artifact | manifest検証、対応profile | license確認・承認・保管 |
| Conversation/Retention | 機能、削除job、audit | policy、legal hold、backup承認 |
| Monitoring/Incident | metric、runbook、support | 監視接続、一次対応、連絡 |

契約で異なる場合は個別責任分界表を優先する。

---

## 20. 実装Phase

本章はPlatform統合の作業Phaseである。GatewayおよびNative Engineの新規開発Phaseではない。

### Phase 0: Existing System Assessment・Contract Freeze

- Gateway repository、release、OpenAI API、auth、Policy、Audit、Installerのmapping
- Native Engine repository、release、Data/Control API、capability、certified profileのAs-Built
- app.lykuro.ai、tenant/project/role、billing/usageのmapping
- Gateway↔PlatformおよびPlatform↔Engine contract確定
- security boundary、data flow、feature flag、migration/test plan
- reuse/extend/new/verify/out-of-scope一覧

終了条件: Gateway再実装とEngine再実装が対象外として明記され、実際のAPIと本書の差分がreview済み。

### Phase 1: Gateway Platform Adapter

- 既存Gateway内部adapter/interfaceへPlatform backendを追加
- request identity、policy context、deadline伝播
- stream、cancel、error、usage正規化
- mTLS/service identity
- feature flag、canary、旧route rollback
- Gateway regression test

終了条件: 既存OpenAI SDK互換、Virtual Key、RBAC、Policy、Audit、Usageを破壊せずPlatformへroute可能。

### Phase 2: Model Manager・Native Engine Integration

- model catalog、license、approval
- Native Engine deployment mapping
- Data API/Control API adapter
- signed model load、status、capacity、drain/resume
- app.lykuro.ai Model/Engine画面
- compatibility matrix、health、rollback

終了条件: Model ManagerだけがEngine lifecycleを管理し、Gateway経由でGenerate/Stream/Cancelが動作する。

### Phase 3: External Runtime Connector

- Generic OpenAI-Compatible Connector
- endpoint allowlist、TLS/mTLS、credential reference
- health/capability、circuit breaker、error normalization
- management mode制限

終了条件: 第三者Runtimeを同梱・変更せず、顧客管理endpointへ安全に接続できる。

### Phase 4: Conversation Memory・Retention

- conversation/message、Context Builder
- 30日retention、Stateless 0日
- encryption、version、idempotency
- summary、structured memory
- deletion cascade、backup expiration
- export/delete、RBAC/audit

終了条件: node変更後も会話を再構築でき、本文がControl Planeへ送信されず、期限削除が動作する。

### Phase 5: Distributed Request Pool

- Node Agent、Coordinator
- node health/capacity
- request sharding、sticky routing
- failover、drain、duplicate job防止

終了条件: 複数nodeへpolicy準拠で分散し、node停止から復旧できる。

### Phase 6: Task・Multi-Model

- task decomposition
- parent/child job、aggregation
- generator/reviewer workflow
- partial failure、cancel、idempotency

終了条件: task追跡、再試行、結果統合が再現可能。

### Phase 7: Production Hardening

- unified package、offline package、SBOM、signature
- Blue/Green/Rolling、backup、restore、rollback
- HA、SLO、capacity、soak、chaos/security test
- monitoring、diagnose、runbook、support handover

終了条件: Gateway回帰、Platform E2E、Strict Local、tenant isolation、recoveryの受入testに合格。

Native Engineの新機能が必要な場合は、本Platform Phaseへ混在させず、LYK-NIE-SD-001の変更要求として別管理する。

---

## 21. Test仕様

### 21.0 Gateway Integration・Contract

- 既存Gateway OpenAI SDK compatibility
- Virtual Key、OIDC/JWT、mTLSの回帰
- tenant/project/actor scope伝播
- data class、allowed model、local-onlyの伝播
- Gateway→Platform mTLSと未認証access拒否
- Generate、Stream、Cancel、client disconnect
- error mapping、Retry-After、SSE error
- request_id/trace_idのend-to-end相関
- audit、usage、billing pipeline回帰
- feature flag OFF時の既存route
- Platform障害時のbounded failureとrollback

### 21.1 Model Manager

- model/Runtimeのtenant分離
- unapproved modelをdeploy不可
- invalid license reviewを拒否
- Runtime credentialを返さない
- management modeを超える操作を拒否
- endpoint SSRFを防止
- model alias rollback

### 21.2 Connector

- stream/non-stream変換
- timeout/cancel
- error normalization
- circuit breaker
- TLS/mTLS
- malformed response
- content redaction

### 21.3 Native Engine

- 既存Engine artifact/APIのAs-Built適合性
- GatewayからControl APIを呼べないこと
- Model Managerだけがload/unload/drain/resume可能
- digest/署名不正modelをload不可
- unsupported architecture/formatを拒否
- known input/output reference test
- streaming order
- cancel/timeout
- OOM recovery
- unload時resource解放
- queue fairness
- KV cache isolation

### 21.4 Distributed

- healthy nodeへ分散
- data class/network zone違反を拒否
- node停止時failover
- duplicate job防止
- task partial failure
- sticky cache miss時のcontext再構築
- different model capability選択
- Control Plane断中の動作

### 21.5 Conversation Memory

- node変更後も会話継続
- model変更時のre-tokenize
- context window超過時summary
- system/security policy非truncate
- concurrent message version conflict
- streaming中断message
- cross-tenant conversation access拒否
- deleted conversation取得不可

### 21.6 Retention

- Stateless API本文が保存されない
- 30日期限dataが削除queueへ入る
- message削除でsummary/vector/attachmentも削除
- restrictedが原則0日
- retention短縮が既存dataへ反映
- backup expiration記録
- legal holdの権限/audit
- deletion job failure alert

### 21.7 Security

- IDOR、tenant spoofing
- RBAC bypass
- SSRF、DNS rebinding
- secret/log leakage
- prompt/responseのControl Plane非送信
- internal APIの無認証access拒否
- package/model/config改ざん
- node identity spoofing
- model path traversal

### 21.8 Regression

- existing Personal/Enterprise分岐
- existing API key
- existing Virtual Key/RBAC/Policy/Rate Limit
- existing billing/usage
- existing audit
- existing OpenAI SDK compatibility
- existing Gateway package/install
- existing Gateway CLI/Control Agent/config rollback
- existing Native Engine golden/correctness/performance baseline

### 21.9 Package・Upgrade・Recovery

- Existing Gateway Add-on install
- Unified Fresh Install
- Node.js/npm/nvmなしのinstall
- sudoなし/rootless precheck
- checksum、signature、SBOM、tenant binding
- incompatible Gateway/Engine version拒否
- migration、rolling update、canary
- Gateway Adapter rollback
- Engine drain/update/smoke/rollback
- Air-Gapped外部通信なしのinstall/update
- uninstallがGateway、model、audit、conversationを削除しないこと

---

## 22. 受入基準

| ID | 項目 | 合格条件 |
|---|---|---|
| AT-00 | Gateway再利用 | 既存Gatewayをfork・複製せず、既存releaseと内部adapterで統合している |
| AT-G01 | Gateway回帰 | OpenAI API、Virtual Key、RBAC、Policy、Rate Limit、Audit、Usageが回帰testに合格 |
| AT-G02 | Contract | Gateway↔Platform contractがversion化され、Generate/Stream/Cancel/errorが適合 |
| AT-G03 | Control分離 | Gateway identityではNative Engine Control APIを実行できない |
| AT-G04 | Engine再利用 | Native Engineを再開発せず、As-Built APIとcompatibility matrixで統合している |
| AT-01 | 非内蔵 | 第三者Runtimeのbinary/image/sourceがpackageとSBOMに存在しない |
| AT-02 | Connector | 顧客管理endpointへprotocol接続できる |
| AT-03 | Model Manager | model、license、approval、deploymentを管理できる |
| AT-04 | Tenant分離 | 他tenantのmodel、Runtime、node、conversationへaccess不可 |
| AT-05 | Strict Local | 本文、summary、memoryが顧客環境外へ送信されない |
| AT-06 | Stateless | 通常APIの本文保存が0日 |
| AT-07 | Conversation | node切替後もcontextを再構築して継続できる |
| AT-08 | Retention | 30日標準、連動削除、backup最大30日が動作 |
| AT-09 | Distributed | 複数nodeへのrequest分散とfailoverが動作 |
| AT-10 | Sticky | KV cache消失時も正しいcontextで継続できる |
| AT-11 | Native独立 | Native Engineが第三者推論Runtimeを含まない |
| AT-12 | Native security | 署名・未承認modelを拒否する |
| AT-13 | Audit | 重要操作とroute/jobにrequest IDが存在 |
| AT-14 | RBAC | 権限なしuserが管理操作不可 |
| AT-15 | Regression | 既存app.lykuro.aiとGateway機能を破壊しない |
| AT-16 | Unified Package | Add-on/Fresh Installの署名、version、tenant bindingを検証できる |
| AT-17 | Installer | Node.js/npm/nvmおよびsudoを必須とせず、precheckがread-onlyである |
| AT-18 | Update | Gateway/Platform/Engineを独立更新し、不整合拒否とrollbackが動作する |
| AT-19 | Observability | request_idでGateway、Platform、Engine、Memory、Auditを相関できる |
| AT-20 | Control Plane非送信 | packet/log/trace検査で本文、summary、memory、tool入出力が送信されない |

---

## 23. Definition of Done

- 既存repository調査とmapping reportがある。
- GatewayとNative EngineのAs-Built report、release、version、contractが記録されている。
- Gateway機能のreuse/extend/new/out-of-scope対応表がある。
- Gatewayを再実装、fork、複製していない。
- Native Engine coreをPlatform作業として再実装していない。
- 本書の上書き対象が関連文書へ反映されている。
- 第三者Runtimeをpackageへ含めていない。
- Model Manager、Connector、Memory、Distributed、Native Engineの責務が分離されている。
- DB migrationとrollback/compatibility方針がある。
- OpenAPIまたは既存API contractが更新されている。
- Gateway↔PlatformおよびPlatform↔Engine contract testがある。
- unit、integration、E2E、security、retention testがある。
- lint、typecheck、build、regression testが成功している。
- Strict Local通信capture test結果がある。
- tenant分離test結果がある。
- deletion cascadeとbackup expirationのtest結果がある。
- SBOM、signature、secret scan、vulnerability scanがある。
- performance未達、未検証、既知問題が明記されている。
- unified manifestとGateway/Platform/Engine compatibility matrixがある。
- Add-on install、fresh install、upgrade、rollback、uninstall手順がある。
- Gateway既存routeへ戻すfeature flagまたはrollbackが検証されている。

---

## 24. 要決定事項

| ID | 論点 | 推奨初期値 |
|---|---|---|
| D-01 | Connector protocol | Generic OpenAI-Compatible |
| D-02 | Third-party lifecycle操作 | 標準禁止 |
| D-03 | Conversation retention | 30日 |
| D-04 | Stateless retention | 0日 |
| D-05 | Restricted retention | 原則0日 |
| D-06 | Native対応model | 実装済みEngine As-Built/certified profileから選択 |
| D-07 | Native対応hardware | 実装済みEngine certified matrixに限定 |
| D-08 | Native weight format | 実装済みEngine manifest対応形式に限定 |
| D-09 | Distributed初期Mode | Request Sharding |
| D-10 | Model Parallelism | 将来・個別設計 |
| D-11 | Structured Memory期限 | tenant policy、無期限禁止 |
| D-12 | Backup本文保持 | 最大30日 |
| D-13 | Gateway↔Platform transport | 既存実装に適合。新規ならmTLS gRPCを推奨 |
| D-14 | Gateway変更方式 | Adapter + feature flag、fork禁止 |
| D-15 | 統合install | Gateway導入済みはAdd-on、新規はUnified bundle |
| D-16 | Gateway/Engine version | As-Built後にcompatibility matrixで確定 |

未決事項を推測で固定せず、configuration、feature flag、versioned contractで分離する。

---

## 25. Claude Codeの最終報告形式

~~~markdown
## 既存構成の調査結果

## Gateway As-Built・再利用範囲

## Native Engine As-Built・対応能力

## 本仕様との対応表

## Reuse・Extend・New・Verify・Out of Scope

## 実装対象Phase・Work Package

## 変更ファイル

## DB Migration

## API・Schema

## Gateway↔Platform Contract

## Platform↔Engine Data API・Control API

## Model Manager・Connector

## Conversation Memory・Retention

## Distributed Pool

## Native Inference Engine

## Security・Strict Local

## 実行したTestと結果

## Package・Deployment

## Compatibility Matrix・Upgrade・Rollback

## 未実装・未検証・既知問題
~~~

実行していないtestを成功と記載してはならない。GPU、KMS、Kubernetes、高速network、顧客IdP等がなく検証できない項目は、未検証理由、必要環境、再現可能な検証手順を報告する。

GatewayまたはNative Engineを再実装した場合は、なぜ既存実装を再利用できなかったか、承認されたADR、互換性とrollbackを必ず報告する。承認がない場合は仕様違反とする。

---

## 改訂履歴

| 版 | 日付 | 内容 |
|---|---|---|
| v1.0 | 2026-08-07 | Model Manager、第三者Runtime非内蔵、Native Engine、分散LLM、Conversation Memory、Retentionを統合 |
| v2.0 | 2026-08-07 | 実装済みPrivate LLM Gatewayを製品入口として統合。Gateway再実装禁止、Gateway↔Platform contract、既存Native EngineのAs-Built統合、統合配布・工程・試験・受入基準を追加 |
