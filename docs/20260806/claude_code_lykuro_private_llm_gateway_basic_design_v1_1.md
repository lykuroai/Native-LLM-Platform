# Lykuro Private LLM Gateway 基本設計書

| 項目 | 内容 |
|---|---|
| 文書番号 | LYK-PLG-BD-001 |
| 版 | v1.1 Claude Code Edition |
| 作成日 | 2026-08-06 |
| 作成 | 株式会社eビジネスソリューション / Lykuro.ai |
| 機密区分 | 社内・提案先限定 |
| 対象 | app.lykuro.ai企業版、企業オンプレミス、顧客VPC、専用GPU環境 |
| 実装担当 | Claude Codeおよび開発担当者 |
| 関連文書 | LYK-PLG-ADD-001「Private LLM Gateway 導入・配布機能 追加仕様書」 |

---

## 0. Claude Codeへの実装指示

本書は、Lykuro.ai企業版へPrivate LLM Gatewayを追加するための、実装可能な基本設計書である。

Claude Codeは、本書の記載をそのまま新規プロジェクトへ置き換えて実装してはならない。既存リポジトリの構成、認証、テナント、課金、監査、API規約、画面部品へ適合させること。

### 0.1 実装開始前の必須調査

コード変更前に、次を調査して報告する。

1. フロントエンド、バックエンド、ワーカー、DB、キャッシュ、メッセージ基盤の技術構成
2. app.lykuro.aiの個人・企業ユーザー判定方法
3. tenant、organization、user、role、project、API keyの既存モデル
4. 既存認証、認可、監査、課金、秘密情報管理の実装
5. APIルーティング、レスポンス、エラー、ページネーション、Idempotencyの規約
6. DBマイグレーション、テスト、CI/CD、feature flagの方式
7. OCI Registry、オブジェクトストレージ、署名鍵、KMSの既存利用有無
8. 本書の論理名と既存実装の対応表

調査結果を提示するまでは、大規模なファイル追加、認証置換、DB再設計を行わない。

### 0.2 優先順位

要件が衝突した場合は、次の順で判断する。

1. 顧客データの非送信、テナント分離、認証、署名検証
2. 既存app.lykuro.aiとの後方互換性
3. 本書の受入基準
4. 関連追加仕様書
5. 論理的な画面名、APIパス、クラス名

論理名は既存規約へ変更できるが、セキュリティ要件を弱めてはならない。

### 0.3 絶対条件

- Strict Local Modeを既定ONとする。
- プロンプト、回答、system/developer message、添付、RAG結果、tool入出力本文をLykuro制御プレーンへ送信しない。
- 機密区分、認証、モデル許可、設定署名が不明な場合はFail Closedとする。
- 可用性を理由に未承認クラウドLLMへ自動送信しない。
- 個人ユーザーへPrivate LLM Gateway管理機能を公開しない。
- 企業ユーザーでも許可ロール以外の作成、ダウンロード、更新、失効を拒否する。
- 権限は画面表示だけでなく、すべてサーバー側で検証する。
- 顧客OSへNode.js、npm、nvmの導入を要求しない。
- インストーラーがsudoを前提にせず、OSパッケージを無断追加・更新しない。
- curl URL | shを正式な企業導入手順にしない。
- 長期秘密鍵、顧客パスワード、Registryパスワードを配布物、イメージ、ログへ平文保存しない。
- パッケージ、設定、OCIイメージの完全性を検証する。
- DB変更はマイグレーション化し、後方互換性または復帰方法を用意する。
- 既存の認証、課金、監査、デザインシステムを無断で置換しない。

### 0.4 実装単位

変更は少なくとも次の単位でレビュー可能にする。

1. DB・ドメインモデル
2. 制御プレーン管理API
3. app.lykuro.ai企業管理画面
4. Package Builder
5. Gateway Web API
6. Control Agent
7. インストーラー・CLI
8. 監査・可観測性
9. テスト・文書

---

## 1. 目的

企業が保有するローカルLLMを、既存アプリケーション、AIエージェント、MCPクライアント、社内業務システムから利用できるOpenAI互換APIとして安全に提供する。

本サービスは単なる推論プロキシではない。認証、RBAC、仮想キー、データ分類、モデル許可、監査、利用量、品質評価、ライセンス、運用を統合する。

### 1.1 提供価値

- confidential/restrictedデータの処理を顧客ローカルLLMへ固定できる。
- vLLM、Ollama、TGI等の実行基盤差異を統一APIで隠蔽できる。
- 部署、プロジェクト、用途単位の仮想キー、上限、監査を提供できる。
- Lykuro.aiの自動モデル選択、R(m)評価、MCP Gatewayと接続できる。
- 顧客別の専用配布物、更新、ライセンス、監査証跡を管理できる。

### 1.2 サービスの本体

顧客環境へインストールされる本体は、常時稼働するWeb APIサービスである。

- アプリケーション利用: HTTPSのOpenAI互換API
- 管理者利用: app.lykuro.ai企業管理画面
- 導入・保守: lykuro-gateway CLI

CLIは推論要求を日常的に入力するためのものではない。install、status、logs、diagnose、upgrade、rollback等の運用に使用する。

---

## 2. 対象範囲

### 2.1 対象

| 領域 | 対象 |
|---|---|
| Gateway API | Models、Chat Completions、Embeddings、Responses段階対応 |
| Gateway制御 | 認証、認可、レート制限、ポリシー、ルーティング、監査 |
| Runtime連携 | vLLM、Ollama、TGI、OpenAI互換Runtime |
| 制御プレーン | tenant、Gateway、model、policy、license、config管理 |
| 配布 | 顧客別署名済みCompose、Helm、オフラインパッケージ |
| 運用 | ヘルス、メトリクス、診断、更新、ロールバック |
| app.lykuro.ai | 企業限定Gateway管理、配布、監査、利用量表示 |
| 連携 | 自動モデル選択、R(m)、MCP Gateway |

### 2.2 対象外

- 基盤モデルの新規学習
- ファインチューニングサービス本体
- 顧客RAG文書の整備と評価
- GPUドライバー、OS、Kubernetesの自動構築
- 無認証の一般消費者向け公開API
- 標準契約におけるGatewayソースコード販売
- 顧客アプリケーションの個別改修

---

## 3. 用語

| 用語 | 定義 |
|---|---|
| Control Plane | app.lykuro.ai側でtenant、設定、ポリシー、配布、ライセンス、利用量メタデータを管理する機能 |
| Data Plane | 顧客環境内でAPI要求、本文、推論、ローカル監査を処理するGateway |
| Control Agent | 顧客側で設定取得、署名検証、ヘルス報告、更新制御を行うコンポーネント |
| Logical Model | アプリケーションが指定する安定したモデル名 |
| Physical Model | 実際のRuntime、モデルバージョン、endpoint |
| Virtual Key | tenant、project、model、上限等を持つGateway用APIキー |
| Strict Local Mode | 本文を顧客環境外へ送信しない既定モード |
| Last Known Good | 署名・構文・接続検証を通過した直前の有効設定 |
| Package Builder | 顧客別配布物を非同期生成し署名する制御プレーン機能 |

---

## 4. ビジネス要求

| ID | 要求 | 優先度 |
|---|---|---:|
| BR-01 | 企業ローカルLLMを安全なAPI商品として導入・運用できる | 必須 |
| BR-02 | 顧客ごとの環境、設定、キー、監査、利用量を分離できる | 必須 |
| BR-03 | 初期費用、月額、GPU費、従量費を分離して扱える | 必須 |
| BR-04 | Runtimeとモデル差異をAPI利用者から隠蔽できる | 必須 |
| BR-05 | 顧客環境からのInbound接続なしでオンライン管理できる | 必須 |
| BR-06 | 閉域・オフライン環境へ導入できる | 必須 |
| BR-07 | セキュリティ質問票、SLA、監査へ必要な証跡を出力できる | 必須 |
| BR-08 | 自動モデル選択、品質評価、MCP連携を段階導入できる | 推奨 |

### 4.1 課金責務

Gatewayは請求書を発行せず、本文を除く利用量メタデータを生成する。既存app.lykuro.aiの課金・請求機能が集計と請求を担当する。

計測候補:

- request_count
- input_tokens
- output_tokens
- gpu_seconds
- model_id
- tenant_id
- project_id
- result_code

---

## 5. 動作モード・提供形態

### 5.1 動作モード

| モード | 本文経路 | 用途 | 既定 |
|---|---|---|---:|
| Strict Local | 顧客環境内で完結 | 機密、個人情報、基幹業務 | ON |
| Hybrid | ポリシー判定後にLocalまたは承認Cloud | 品質・コスト最適化 | OFF |
| Managed Relay | Lykuro中継から顧客LLMへ接続 | 特別な接続要件 | OFF |
| External Partner | WAF等を経由し許可先へ提供 | 取引先API | OFF |

Hybrid、Managed Relay、External Partnerはfeature flagと個別承認を必要とする。MVPはStrict Localを対象とする。

### 5.2 Edition

| Edition | 提供物 | 主な用途 |
|---|---|---|
| PoC | Docker Compose、CLI、単一構成 | 小規模検証 |
| Enterprise | 署名済みOCI、Helm、HA、監視連携 | Kubernetes本番 |
| Air-Gapped | OCI image tar、オフライン設定・ライセンス | 閉域環境 |

### 5.3 配置モデル

| 配置 | 構成 | 推奨 |
|---|---|---:|
| 顧客オンプレミス | 顧客サーバーへGatewayとLLMを配置 | 初期商品 |
| 顧客VPC | 顧客AWS等の専用ネットワークへ配置 | 初期商品 |
| Lykuro専用環境 | 顧客単位の専用VPC/GPU | 個別契約 |
| 共有マルチテナント | 共有Gatewayと論理分離Runtime | MVP対象外 |

### 5.4 ソースコード方針

- 標準契約ではソースコードを配布しない。
- 署名済みOCIイメージ、設定、Compose/Helm、CLI、SBOM、手順書を提供する。
- OEM、ソースライセンス、ソースエスクローは別契約とする。

### 5.5 外部パートナー向けAPI

Public APIという名称を使用する場合も、無認証の一般公開を意味しない。外部パートナーへ提供する場合は、個別審査、利用契約、WAF、IP許可、Rate Limitに加え、mTLSまたはOAuth 2.0/OIDCを必須とする。

---

## 6. 全体アーキテクチャ

~~~mermaid
flowchart TD
    A["app.lykuro.ai"] --> B["Control Plane API"]
    B --> C["Package Builder"]
    C --> D["Signed Distribution"]
    D --> E["Customer Admin"]
    E --> F["Private LLM Gateway"]
    F --> G["Local LLM Runtime"]
    F -. "Metadata only / HTTPS 443" .-> B
~~~

### 6.1 基本方針

- Control PlaneとData Planeを分離する。
- Gatewayは顧客ネットワーク内で本文を処理する。
- 顧客側からLykuroへのHTTPS 443 Outboundのみを既定とする。
- Lykuroから顧客ネットワークへのInbound接続を要求しない。
- Control Planeは既存app.lykuro.aiの日本AWSリージョン構成を維持する。国内完結契約では、許可されたmetadataも指定地域外へ転送しない。
- 設定は署名済みパッケージとして配布する。
- 制御断時は有効なLast Known Goodで継続する。

### 6.2 信頼境界

| 境界 | 保護対象 | 主な対策 |
|---|---|---|
| 利用者 → Gateway | APIキー、JWT、入力、添付 | TLS、OIDC/JWT、Virtual Key、IP制限、Rate Limit |
| Gateway → Runtime | prompt、system指示、推論結果 | 顧客内ネットワーク、mTLS、Service Account |
| Gateway → Control Plane | 利用量、稼働、設定要求 | 本文除外、許可リスト、TLS、端末認証 |
| Admin → app.lykuro.ai | 設定、パッケージ、監査 | 企業認証、RBAC、短期URL、監査 |
| Package Builder → 配布領域 | OCI、設定、ライセンス | SHA-256、署名、暗号化、期限 |

### 6.3 制御プレーンへ送信可能な既定項目

許可リスト方式とし、未登録フィールドを送信しない。

~~~text
tenant_id
gateway_id
project_id
logical_model_id
physical_model_id
request_count
input_tokens
output_tokens
latency_ms
result_code
policy_decision
software_version
config_version
health_status
timestamp
~~~

本文、本文ハッシュの原文復元情報、添付、tool入出力本文は含めない。

---

## 7. コンポーネント設計

| コンポーネント | 配置 | 責務 |
|---|---|---|
| Enterprise Admin UI | Control Plane | Gateway、package、model、policy、audit、usage管理 |
| Gateway Management API | Control Plane | RBAC、tenant分離、状態管理、管理操作 |
| Package Builder | Control Plane | 顧客別パッケージ生成、署名、SBOM、保管 |
| Config Service | Control Plane | 設定世代、署名、配布、適用結果管理 |
| License Service | Control Plane | online/offlineライセンス発行・失効 |
| Edge/API Gateway | Data Plane | HTTP、stream、認証、Rate Limit、エラー正規化 |
| Auth Service | Data Plane | Virtual Key、JWT、mTLS、RBAC |
| Policy Engine | Data Plane | データ分類、経路、モデル、保持、マスキング |
| Model Router | Data Plane | Logical ModelからPhysical Modelを選択 |
| Inference Adapter | Data Plane | Runtime固有形式への変換 |
| Audit Service | Data Plane | 本文なし監査、顧客ログ連携 |
| Control Agent | Data Plane | 登録、設定取得、署名検証、heartbeat、更新 |
| lykuro-gateway CLI | Data Plane | 導入、状態、診断、更新、復帰 |

### 7.1 Gatewayプロセス分割

MVPでは単一バイナリまたは少数サービスでもよいが、責務境界をコード上で分離する。

~~~text
HTTP Layer
  -> Authentication
  -> Tenant / Project Resolution
  -> Policy Evaluation
  -> Model Routing
  -> Runtime Adapter
  -> Audit / Usage
~~~

### 7.2 Inference Adapter

共通インターフェース例:

~~~go
type InferenceAdapter interface {
    ListModels(ctx context.Context) ([]Model, error)
    Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
    ChatStream(ctx context.Context, req ChatRequest) (Stream, error)
    Embed(ctx context.Context, req EmbeddingRequest) (EmbeddingResponse, error)
    Health(ctx context.Context) error
}
~~~

実際の言語と型は既存リポジトリへ合わせる。

MVP対象:

- OpenAI Compatible Adapter
- vLLM Adapter
- Ollama Adapter

TGIと追加Runtimeは同じ契約で拡張可能にする。

---

## 8. リクエスト処理

### 8.1 標準フロー

1. Request IDを確定する。
2. Virtual Key、OIDC/JWTまたはmTLSを検証する。
3. tenant、project、actor、scope、利用上限を解決する。
4. request schema、サイズ、禁止形式を検証する。
5. データ分類、PII、機密区分を判定する。
6. Policy Engineがlocal-only、hybrid、denyを返す。
7. Logical Modelを許可済みPhysical Modelへ解決する。
8. Runtime Adapterへ変換して推論する。
9. streaming出力を必要な粒度で検査する。
10. ローカル監査と利用量を記録する。
11. 本文を除くメタデータだけを非同期送信する。

### 8.2 ルーティング優先順位

| 優先 | 条件 | 動作 |
|---:|---|---|
| 1 | restricted、confidential、local-only | 許可Localのみ。利用不可なら拒否 |
| 2 | モデル固定ポリシー | 指定Logical Modelへ固定 |
| 3 | 能力要件 | JSON、tools、vision、context等で候補を限定 |
| 4 | 品質・速度・コスト | R(m)、負荷、予算で選択 |
| 5 | 障害時 | 許可済み候補のみへ切替 |

外部LLMへの暗黙フォールバックは禁止する。

### 8.3 Idempotency

Gateway作成、package生成、設定適用等の管理操作はIdempotency-Keyへ対応する。重複要求で同一リソースを二重作成しない。

推論APIは、呼出元が明示的に再送制御できるようrequest_idを返す。stream開始後の自動再試行は行わない。

---

## 9. Gateway Web API

### 9.1 エンドポイント

| Method | Path | MVP | 認証 |
|---|---|---:|---|
| GET | /v1/models | 対応 | 必須 |
| POST | /v1/chat/completions | 対応 | 必須 |
| POST | /v1/embeddings | 対応 | 必須 |
| POST | /v1/responses | Phase 2 | 必須 |
| POST | /v1/rerank | Phase 2 | 必須 |
| GET | /healthz | 対応 | 不要・最小情報 |
| GET | /readyz | 対応 | 内部または最小情報 |
| GET | /metrics | 対応 | 管理ネットワーク限定 |

### 9.2 リクエストヘッダー

| Header | 必須 | 内容 |
|---|---:|---|
| Authorization | ○ | Bearer Virtual KeyまたはOIDC/JWT |
| Content-Type | POST時○ | application/json |
| X-Request-ID | 任意 | 未指定時Gatewayが生成 |
| X-Lykuro-Project | 推奨 | project識別 |
| X-Data-Class | 条件 | public/internal/confidential/restricted |
| X-Routing-Mode | 条件 | local-only/hybrid。Policyが上書き可能 |

X-Lykuro-Tenantを外部入力として受ける場合も、認証主体に紐づくtenantと一致検証する。header値だけでtenantを信頼しない。

### 9.3 Chat例

~~~bash
curl https://gateway.customer.example/v1/chat/completions \
  -H "Authorization: Bearer <virtual-key>" \
  -H "Content-Type: application/json" \
  -H "X-Lykuro-Project: legal-review" \
  -d '{
    "model": "company-llm-safe",
    "messages": [{"role": "user", "content": "契約条項を要約してください"}],
    "temperature": 0.2,
    "stream": true
  }'
~~~

### 9.4 エラー形式

~~~json
{
  "error": {
    "message": "The requested model is not allowed by policy.",
    "type": "policy_error",
    "code": "policy_denied",
    "request_id": "req_example"
  }
}
~~~

| HTTP | code | 条件 |
|---:|---|---|
| 400 | invalid_request | schema、サイズ、形式不正 |
| 401 | authentication_failed | key、JWT、証明書不正 |
| 403 | policy_denied | model、用途、機密区分が不許可 |
| 409 | config_not_ready | 有効な署名済み設定がない |
| 429 | rate_limit_exceeded | RPS、同時数、token上限 |
| 503 | model_unavailable | Runtime未ロード、GPU障害 |
| 504 | inference_timeout | 推論時間超過 |

### 9.5 OpenAI互換範囲

- 互換対象SDKとバージョンをCIで固定する。
- 未対応parameterを黙って無視せず、互換性方針に従い明示する。
- stream=trueはSSEの終了、切断、error eventをテストする。
- modelにはLogical Modelのみを公開する。
- Runtime内部のendpoint、credential、物理モデル名を返さない。

---

## 10. 認証・認可

### 10.1 認証方式

| 主体 | 方式 | 用途 |
|---|---|---|
| 社内アプリ | Virtual Key + IP制限 | Server-to-Server |
| ユーザー | OIDC/JWT | ユーザー単位監査 |
| 高機密連携 | mTLS + JWTまたはKey | 基幹・外部パートナー |
| Control Agent | 端末認証情報 + 署名token | 設定・heartbeat |

### 10.2 RBAC

| ロール | 主な権限 |
|---|---|
| Tenant Owner | 契約、企業設定、全Gateway、全監査、承認 |
| AI Administrator | Gateway、model、endpoint、policy、key、package |
| Security Auditor | audit、設定履歴、access履歴の参照・出力 |
| Project Manager | 担当projectのkey、budget、user、model |
| Developer | 許可key/model利用、自分の利用量 |
| Billing Viewer | usage、budget、billing参照。本文不可 |

### 10.3 管理操作権限

| 操作 | Owner | AI Admin | Auditor | Billing | Developer |
|---|---:|---:|---:|---:|---:|
| Gateway参照 | ○ | ○ | ○ | ○ | ○ |
| Gateway作成・更新 | ○ | ○ | × | × | × |
| Package生成・DL | ○ | ○ | × | × | × |
| Policy変更 | ○ | ○ | 参照 | × | × |
| Key発行・失効 | ○ | ○ | 参照 | × | × |
| Audit出力 | ○ | ○ | ○ | × | × |
| Gateway失効 | ○ | ○ | × | × | × |

### 10.4 Virtual Key

- 作成時のみ平文表示し、保存時は強いハッシュで保持する。
- tenant、project、environment、allowed_models、scope、RPS、token上限、期限を持つ。
- 本番と開発を分離し、ローテーションの重複期間を設定できる。
- 標準ローテーション周期は90日とし、契約・用途により短縮できる。
- 失効はGatewayへ短時間で反映し、制御断時の失効ポリシーを契約で定める。
- key原文を監査、診断、例外、analyticsへ送らない。

---

## 11. ポリシー・データ保護

### 11.1 データ分類

| 分類 | 例 | 許可経路 | 本文ログ |
|---|---|---|---|
| public | 公開済み情報 | Local / 承認Cloud | 顧客設定 |
| internal | 社内一般情報 | Local / 国内承認Cloud | 原則OFF |
| confidential | 契約、顧客、営業秘密 | Localのみ | 原則禁止 |
| restricted | 要配慮情報、認証情報、重要機密 | 専用Localのみ | 禁止 |

### 11.2 Policy入力

- tenant、project、actor、role
- data_class
- requested_model
- required_capabilities
- routing_mode
- request_size
- IP、network zone
- budget、rate limit
- license状態
- gateway/config version

### 11.3 Policy出力

~~~json
{
  "decision": "allow",
  "routing": "local-only",
  "allowed_models": ["company-llm-safe"],
  "content_logging": false,
  "max_output_tokens": 4096,
  "reason_code": "confidential_local_policy"
}
~~~

### 11.4 Strict Local Mode

顧客環境内へ保持する本文:

- promptとresponse
- system/developer message
- 添付
- RAG検索結果
- tool callのargumentsとresult
- embedding元文書

Control Planeへ送信しない。Gatewayの通常ログ、metric label、trace attributeにも載せない。

### 11.5 本文保存

既定は0日・保存なしとする。有効化には次を必須とする。

- 顧客管理の保存先
- 暗号化と鍵管理
- 保存期間と削除
- 閲覧RBAC
- 監査
- 契約上の目的と同意

### 11.6 保存期間

| データ | 既定 | 保存先 | 保持期間 |
|---|---:|---|---|
| Prompt本文 | 保存しない | 顧客が明示した顧客領域のみ | 0日または顧客設定 |
| Response本文 | 保存しない | 同上 | 0日または顧客設定 |
| Audit metadata | 保存する | 顧客監査DB | 365日、契約変更可 |
| Usage metadata | 保存する | 顧客DBおよびControl Plane | 請求・契約要件 |
| Config履歴 | 保存する | Control Plane | 契約期間＋1年を基準 |
| Error診断情報 | 最小限 | 顧客log基盤 | 30〜90日 |

法令、契約、顧客ポリシーがより厳しい場合は、その短い保持期間または長い証跡要件を適用する。

---

## 12. モデル・Runtime管理

### 12.1 モデル登録項目

| 分類 | 主な項目 |
|---|---|
| 識別 | model_id、display_name、provider、family、version、digest |
| 配置 | runtime、endpoint、region、network_zone、GPU、replica |
| 能力 | context、stream、JSON、tools、vision、embedding |
| 品質 | evaluation_set、R(m)、stance_score、evaluated_at |
| 運用 | status、health、concurrency、timeout、fallback_group |
| ライセンス | commercial_use、API提供、再配布、AUP、承認者 |
| データ | retention、training_use、region、allowed_class |

### 12.2 モデル承認

1. AI Administratorがmodel card、license、能力、digestを登録する。
2. Security/Legal Reviewerが商用利用、API提供、データ条件を確認する。
3. 検証環境で品質、性能、脆弱性、prompt injection、出力安全性を評価する。
4. 承認者がtenant、project、data classごとの利用範囲を承認する。
5. 承認済みversionを署名し本番へ昇格する。
6. 更新は別versionとして登録し、canary後にLogical Model aliasを切り替える。

未確認ライセンスのモデルを本番公開しない。

### 12.3 自動モデル選択

MVPでは固定Logical Modelとfallback groupを実装する。自動モデル選択は段階導入とし、次の能力を条件に含める。

- function calling
- JSON mode
- context length
- embedding/vision
- R(m)と業務スタンス別評価
- latencyとqueue
- GPU costとbudget

自動選択でもPolicyの経路制限が常に優先する。

---

## 13. Control Agent・設定配信

### 13.1 初回登録

1. app.lykuro.aiで1回限りのinstall tokenを発行する。
2. 顧客管理者がCLIへ対話入力またはsecret fileで渡す。
3. Agentがgateway_id、package digest、公開鍵、software versionを送る。
4. Control Planeがtenant、gateway、token、package、期限を検証する。
5. 端末認証情報と初期署名済み設定を発行する。
6. tokenを使用済みにする。

token原文はDB、shell history、ログへ残さない。

### 13.2 設定適用

- configはversion、schema_version、issued_at、expires_at、digest、signatureを持つ。
- Agentは署名とhashを検証する。
- stagingでschema、参照、Policy、Runtime到達性を確認する。
- 成功後にatomicにactiveへ切り替える。
- 直前2世代以上をLast Known Goodとして保持する。
- 失敗時は現在設定を維持し、本文なしのerror codeを報告する。

### 13.3 制御プレーン断

- 有効なLast Known Goodで推論を継続する。
- license grace period内は契約条件に従い継続する。
- 本文を復旧待ちqueueへ入れない。
- 利用量メタデータだけを上限付き暗号化queueへ保存できる。
- 有効設定が存在しない場合はconfig_not_readyで拒否する。

### 13.4 設定schema

~~~yaml
schema_version: "1"
gateway:
  strict_local_mode: true
  log_content: false
auth:
  virtual_keys_enabled: true
policy:
  default_routing: local-only
models:
  - logical_name: company-llm-safe
    runtime: vllm
    endpoint_ref: local-vllm
~~~

実際のsecret値は含めず、secret manager参照名だけを保持する。

---

## 14. app.lykuro.ai管理画面

既存の企業管理ナビゲーションへPrivate LLM Gatewayを追加する。個人ユーザーには表示しない。

### 14.1 論理ルート

~~~text
/enterprise/private-gateways
/enterprise/private-gateways/new
/enterprise/private-gateways/:gatewayId
/enterprise/private-gateways/:gatewayId/install
/enterprise/private-gateways/:gatewayId/audit
~~~

実際のパスは既存規約へ合わせる。

### 14.2 画面

| 画面 | 主要内容 | 主な操作 |
|---|---|---|
| Gateway一覧 | name、edition、type、status、version、last_seen | 作成、詳細 |
| Gateway作成 | edition、deployment、arch、Strict Local | 確認、作成 |
| Gateway詳細 | health、config、model、usage、error、license | 更新、失効 |
| インストール | package、digest、期限、手順 | 生成、DL、token発行 |
| Model管理 | 能力、配置、license、評価、status | 登録、承認、停止 |
| Policy | data class、routing、model、retention | 作成、配布、復帰 |
| Virtual Key | owner、scope、limit、expires、last_used | 発行、rotate、revoke |
| Audit | actor、action、decision、request_id、result | 検索、出力 |
| Usage | request、token、GPU、予算 | 絞込、通知 |

### 14.3 Gateway状態

~~~mermaid
stateDiagram-v2
    [*] --> draft
    draft --> building
    building --> package_ready
    package_ready --> registered
    registered --> online
    online --> offline
    offline --> online
    online --> updating
    updating --> online
    updating --> degraded
    online --> revoked
~~~

build_failed、degraded、revokedは明示的なエラー理由と復旧操作を表示する。

### 14.4 オフライン管理

Air-Gapped Editionはapp.lykuro.aiへの常時接続を要求しない。顧客環境内の限定管理画面またはCLIで、次だけを扱えるようにする。

- health、version、license、config generationの確認
- 署名済みconfigとupdate archiveのimport
- local auditの検索・export
- diagnose archiveの生成
- rollback

ローカル管理画面は管理networkに限定し、認証なしで公開しない。tenant契約、package発行、online billing等のControl Plane機能を複製しない。

---

## 15. 制御プレーン管理API

パスは論理例であり、既存API規約へ適合させる。

### 15.1 管理API

| Method | Path | 用途 |
|---|---|---|
| GET | /api/enterprise/private-gateways | tenant内一覧 |
| POST | /api/enterprise/private-gateways | Gateway作成 |
| GET | /api/enterprise/private-gateways/{id} | 詳細 |
| PATCH | /api/enterprise/private-gateways/{id} | 設定変更 |
| POST | /api/enterprise/private-gateways/{id}/packages | package生成 |
| POST | /api/enterprise/private-gateways/{id}/packages/{packageId}/download | 短期DL URL |
| POST | /api/enterprise/private-gateways/{id}/install-token | 1回限りtoken |
| POST | /api/enterprise/private-gateways/{id}/revoke | 失効 |
| GET | /api/enterprise/private-gateways/{id}/audit | audit |

### 15.2 Agent API

| Method | Path | 用途 |
|---|---|---|
| POST | /api/gateway-agent/v1/register | 初回登録 |
| GET | /api/gateway-agent/v1/config | 最新設定確認 |
| POST | /api/gateway-agent/v1/config-ack | 適用結果 |
| POST | /api/gateway-agent/v1/heartbeat | health |
| POST | /api/gateway-agent/v1/usage | 本文なし利用量 |
| GET | /api/gateway-agent/v1/releases | 更新確認 |

### 15.3 API共通要件

- server-side tenant scope
- RBAC
- Idempotency-Key
- request_id
- audit
- pagination
- optimistic lockingまたはversion検証
- rate limit
- secret redaction
- uniform error response

短期DL URLは既定15分、最大60分とし、tenant、gateway、package、userを再検証してから発行する。

---

## 16. 配布パッケージ

### 16.1 内容

~~~text
lykuro-private-gateway/
├── install.sh
├── lykuro-gateway
├── compose/
│   └── docker-compose.yml
├── helm/
│   └── lykuro-private-gateway/
├── config/
│   ├── gateway.example.yaml
│   └── policy.example.yaml
├── images/
│   └── offline-images.tar
├── license/
│   └── offline-license.jwt
├── checksums.sha256
├── manifest.json
├── signature.sig
├── sbom/
└── docs/
    ├── INSTALL.md
    ├── UPGRADE.md
    └── SECURITY.md
~~~

offline専用ファイルはonline packageへ含めない。

### 16.2 manifest

~~~json
{
  "schema_version": "1",
  "product": "lykuro-private-llm-gateway",
  "gateway_id": "gw_example",
  "tenant_id": "tn_example",
  "version": "1.0.0",
  "edition": "enterprise",
  "deployment_type": "kubernetes",
  "cpu_arch": "amd64",
  "strict_local_mode": true,
  "created_at": "2026-08-06T00:00:00Z",
  "expires_at": "2026-08-07T00:00:00Z",
  "files": []
}
~~~

manifestへsecretを含めない。

### 16.3 完全性

- 各fileのSHA-256を生成する。
- manifestとchecksumsをLykuro署名鍵で署名する。
- OCI imageを署名し、digestを固定する。
- CycloneDXまたはSPDXのSBOMを同梱する。
- 署名鍵はKMS/HSMまたは既存Secret Managerで管理する。
- インストール前に全検証を完了し、不一致なら停止する。

### 16.4 ダウンロード

- packageはtenant/gatewayへ固定する。
- 期限切れ、失効、他tenantのpackageをDLできない。
- UI操作、URL発行、実DLを監査する。
- 直接のstorage URLを永続保存しない。

---

## 17. インストーラー・CLI

### 17.1 基本方針

- Gateway本体とCLIは静的バイナリまたはコンテナとして提供する。
- 顧客ホストにNode.js/npm/nvmを要求しない。
- precheck、確認、install、verifyを分離する。
- sudoがない場合はrootless可否を判定する。
- OS変更が必要な場合は自動実行せず、管理者向け手順を出す。
- 再実行しても二重登録・設定破壊しない。

### 17.2 CLI

~~~text
lykuro-gateway precheck
lykuro-gateway install
lykuro-gateway register
lykuro-gateway status
lykuro-gateway health
lykuro-gateway logs
lykuro-gateway diagnose
lykuro-gateway config validate
lykuro-gateway config import
lykuro-gateway upgrade
lykuro-gateway rollback
lykuro-gateway uninstall
~~~

### 17.3 precheck

- OS、kernel、CPU arch
- disk、memory、CPU
- Docker/PodmanまたはKubernetes/Helm
- Runtime endpoint
- DNS、proxy、NTP
- HTTPS 443 outbound
- port競合
- write権限
- TLS certificate
- secret保存先
- offline image容量

read-onlyで実行し、OSを変更しない。

### 17.4 install

~~~bash
tar -xzf lykuro-private-gateway-<version>.tar.gz
cd lykuro-private-gateway
./lykuro-gateway precheck
./lykuro-gateway install
~~~

正式手順では、利用者が先に署名とchecksumを検証できるようにする。

### 17.5 配置

~~~text
/opt/lykuro/gateway/       executable and deployment files
/etc/lykuro/gateway/       non-secret configuration
/var/lib/lykuro/gateway/   state and Last Known Good
/var/log/lykuro/gateway/   local logs
~~~

rootlessはXDG準拠のuser directoryへ配置する。

### 17.6 uninstall

- 既定ではaudit、config backup、model weightsを削除しない。
- --purgeは対象と影響を表示して確認を要求する。
- 共有Docker、Kubernetes、Registry、顧客modelを削除しない。
- Control Plane側のrevokeとローカル停止を別操作として記録する。

---

## 18. 論理データモデル

既存entityを優先し、重複するtenant、user、role、project、api key、billing entityを新設しない。

### 18.1 既存または共通entity

| Entity | Key | 主な属性 |
|---|---|---|
| tenant | tenant_id | name、plan、region、data_mode、status |
| project | project_id | tenant_id、name、budget、data_class、owner |
| virtual_key | key_id | project_id、key_hash、scope、limits、expires、status |
| model | model_id | family、version、capabilities、license、status |
| endpoint | endpoint_id | model_id、runtime、zone、health、capacity |
| policy | policy_id | tenant_id、version、rules、signature、effective_at |
| request_audit | request_id | actor、model、decision、tokens、latency、result |
| usage_daily | composite | tenant/date/model、request、tokens、gpu_seconds |

### 18.2 gateway_deployments

~~~text
id                     UUID/ULID PK
tenant_id              FK NOT NULL
name                   VARCHAR NOT NULL
edition                ENUM(poc, enterprise, air_gapped)
deployment_type        ENUM(compose, kubernetes, offline_compose, offline_kubernetes)
cpu_arch               ENUM(amd64, arm64)
strict_local_mode      BOOLEAN NOT NULL DEFAULT TRUE
status                 ENUM
current_version        VARCHAR NULL
current_config_version VARCHAR NULL
last_seen_at           TIMESTAMP NULL
license_expires_at     TIMESTAMP NULL
created_by             FK NOT NULL
created_at             TIMESTAMP NOT NULL
updated_at             TIMESTAMP NOT NULL
version                INTEGER NOT NULL
~~~

### 18.3 gateway_packages

~~~text
id                 UUID/ULID PK
tenant_id          FK NOT NULL
gateway_id         FK NOT NULL
package_type       ENUM(online, offline, helm, compose)
software_version   VARCHAR NOT NULL
status             ENUM(queued, building, ready, failed, expired, revoked)
object_key         VARCHAR NULL
sha256             VARCHAR NULL
signature_ref      VARCHAR NULL
sbom_ref           VARCHAR NULL
expires_at         TIMESTAMP NULL
build_error_code   VARCHAR NULL
created_by         FK NOT NULL
created_at         TIMESTAMP NOT NULL
~~~

### 18.4 gateway_install_tokens

~~~text
id              UUID/ULID PK
tenant_id       FK NOT NULL
gateway_id      FK NOT NULL
token_hash      VARCHAR NOT NULL
expires_at      TIMESTAMP NOT NULL
used_at         TIMESTAMP NULL
revoked_at      TIMESTAMP NULL
created_by      FK NOT NULL
created_at      TIMESTAMP NOT NULL
~~~

### 18.5 gateway_config_versions

~~~text
id              UUID/ULID PK
tenant_id       FK NOT NULL
gateway_id      FK NOT NULL
version         INTEGER NOT NULL
schema_version  VARCHAR NOT NULL
config_json     JSON/JSONB NOT NULL
sha256          VARCHAR NOT NULL
signature_ref   VARCHAR NOT NULL
status          ENUM(draft, active, superseded, rejected)
created_by      FK NOT NULL
created_at      TIMESTAMP NOT NULL
~~~

### 18.6 gateway_heartbeats

大量データとなるため、既存時系列・監視基盤がある場合はDBテーブルを新設しない。

~~~text
gateway_id
tenant_id
timestamp
software_version
config_version
health_status
runtime_status
queue_depth
error_code
~~~

### 18.7 制約

- すべてのgateway関連tableにtenant scopeを持たせる。
- unique制約はtenant_idを含める。
- install tokenはhashのみ保存する。
- config_jsonへsecret、本文、Registry passwordを保存しない。
- package object keyを外部入力から直接指定させない。
- tenant横断joinをrepository/service層で禁止または検出する。

---

## 19. 監査・可観測性

### 19.1 監査イベント

- Gateway作成、更新、失効
- package生成、URL発行、DL
- install token発行、使用、失敗
- Agent登録、設定取得、署名検証、適用
- key発行、rotate、失効
- model登録、承認、更新、停止
- policy作成、変更、配布、rollback
- 推論開始、完了、中断、拒否
- software upgrade、rollback

### 19.2 監査record

~~~json
{
  "request_id": "req_01J_example",
  "tenant_id": "tn_ebs",
  "project_id": "prj_legal",
  "actor_id": "svc_contract-review",
  "logical_model": "company-llm-safe",
  "physical_model": "qwen-local-v3",
  "data_class": "confidential",
  "routing_decision": "local-only",
  "policy_version": "pol_42",
  "input_tokens": 1842,
  "output_tokens": 516,
  "latency_ms": 4820,
  "result": "success",
  "content_logged": false
}
~~~

### 19.3 Metrics

| Category | Metrics |
|---|---|
| Gateway | RPS、concurrency、p50/p95/p99、4xx、5xx |
| LLM | TTFT、tokens/sec、queue、load、timeout |
| GPU | utilization、VRAM、temperature、power、ECC |
| Policy | allow/deny、classification failure、routing |
| Auth | failure、revoked key、abnormal IP |
| Control | last sync、signature error、config generation |

metric labelへuser入力、prompt、key、high-cardinality request_idを載せない。

### 19.4 Logs

- structured JSON
- timestamp、level、service、gateway_id、request_id、code
- secret redaction
- content logging default OFF
- diagnose archiveでもsecretと本文を除去

---

## 20. 非機能設計

### 20.1 目標

| 項目 | MVP | Enterprise |
|---|---:|---:|
| Gateway可用性 | 99.9%/月 | 99.95%以上/月 |
| Gateway追加遅延 | p95 200ms以下 | p95 100ms以下 |
| 同時接続 | tenantあたり100 | 契約値・水平拡張 |
| 設定反映 | 5分以内 | 1分以内 |
| 監査RPO | 24時間以内 | 1時間以内 |
| RTO | 8時間以内 | 2時間以内 |
| Critical対応 | 72時間以内 | 24時間以内 |

推論時間、model load時間、顧客network、GPU性能はGateway追加遅延と分離して測定する。

### 20.2 Scalability

- Gatewayを原則statelessにし水平拡張する。
- streaming connectionと通常RPSを別管理する。
- Runtimeごとにqueue、concurrency、token上限を持つ。
- GPU OOM対策としてcontext、batch、max outputをprofileで制限する。
- オンプレミスではpriority queueと429で過負荷を制御する。

### 20.3 セキュリティ基準

- TLS 1.2以上、推奨1.3
- 高機密経路mTLS
- at-rest暗号化
- secret manager/Vault
- non-root、read-only filesystem、最小権限
- signed OCI、SBOM、vulnerability scan
- network Default Deny
- dependency/secret/container/IaC scan
- prompt injection、model extraction、data exfiltration、DoS test

---

## 21. 障害・例外

| 事象 | 処理 | 応答・状態 |
|---|---|---|
| Runtime未ロード | 許可済みLocal候補へ切替 | 503 + Retry-After |
| GPU OOM | 新規受付抑制、Runtime回復 | 429または503 |
| Control Plane断 | Last Known Goodで継続 | 管理警告 |
| 設定署名不正 | 適用拒否、重大alert | 現設定維持 |
| PII分類失敗 | Fail ClosedまたはLocal固定 | 403/503 |
| Audit DB障害 | 暗号化local buffer、上限後は契約方針 | degraded |
| Cloud LLM障害 | 許可候補のみ。Local制約を維持 | 503/504 |
| Package改ざん | install停止 | non-zero exit |
| Upgrade失敗 | 直前versionへ自動復帰 | degraded後online |
| License期限超過 | 契約に従いFail Closedまたは限定運転 | 403/503 |

---

## 22. 更新・バックアップ・復旧

### 22.1 更新

- Compose: pull/import、互換検査、restart、health確認
- Kubernetes: RollingまたはBlue/Green
- Air-Gapped: update archiveの署名検証後import
- DB/config schemaの前方・後方互換をrelease noteへ明記

更新前にversion、disk、schema、migration、signature、SBOM、旧imageを検査する。

### 22.2 自動ロールバック

次の場合は直前versionへの復帰対象とする。

- healthz失敗
- readyzが期限内に成功しない
- config load失敗
- Adapter初期化失敗
- migration失敗

### 22.3 Backup

- config、audit、usageを暗号化backupする。
- Last Known Goodを2世代以上保持する。
- model weightは取得元、license、digestを記録する。
- 四半期ごとに設定復元、DB復元、key失効、model切替を訓練する。
- 本文保存を有効にした場合は顧客定義のbackup/deleteを適用する。

---

## 23. 環境変数・Secret

論理例:

~~~text
LYKURO_CONTROL_PLANE_URL
LYKURO_GATEWAY_ID
LYKURO_TENANT_ID
LYKURO_CONFIG_PATH
LYKURO_DATA_DIR
LYKURO_LOG_LEVEL
LYKURO_STRICT_LOCAL_MODE
LYKURO_RUNTIME_TYPE
LYKURO_RUNTIME_ENDPOINT
LYKURO_TLS_CERT_PATH
LYKURO_TLS_KEY_PATH
LYKURO_METRICS_ENABLED
LYKURO_HEARTBEAT_INTERVAL_SECONDS
LYKURO_CONFIG_POLL_INTERVAL_SECONDS
~~~

Secretは通常環境変数へ直接設定せず、secret file、Kubernetes Secret、Vault、Secret Manager参照を優先する。

---

## 24. 推奨リポジトリ構成

既存monorepoへ追加する場合の論理例:

~~~text
apps/
├── web/
├── api/
├── package-builder-worker/
└── private-gateway/
packages/
├── gateway-contracts/
├── gateway-cli/
├── gateway-policy/
└── gateway-adapters/
deploy/
├── compose/
├── helm/
├── installer/
└── offline/
docs/
└── private-gateway/
~~~

既存構成が異なる場合は無理に一致させない。API schema、config schema、release versionの互換性をCIで検証する。

---

## 25. 実装フェーズ

### Phase 0: Repository Assessment

- 既存構成調査
- 論理名mapping
- threat boundary確認
- migration、feature flag、test方針

終了条件: 実装計画と影響範囲がレビュー済み。

### Phase 1: Domain・Management API

- gateway deployment
- package/config/token entity
- tenant scoped repository
- RBAC、audit、idempotency

終了条件: unit/integration testでtenant分離を証明。

### Phase 2: Enterprise Admin UI

- enterprise-only navigation
- list/create/detail/install/audit
- permission、loading、error、empty state

終了条件: 個人・権限なしuserが画面/APIを利用不可。

### Phase 3: Package Builder

- async job
- Compose/Helm package
- checksum、signature、SBOM
- short-lived download

終了条件: 改ざん・期限・tenant越境testが成功。

### Phase 4: Gateway MVP

- /v1/models
- /v1/chat/completions + stream
- /v1/embeddings
- Virtual Key
- Policy、Router、vLLM/Ollama
- local audit、usage

終了条件: OpenAI SDK互換とStrict Local通信試験が成功。

### Phase 5: Agent・Installer・CLI

- register、config、heartbeat
- precheck/install/status/diagnose
- upgrade/rollback

終了条件: Node.js、sudoなし条件を含む導入試験が成功。

### Phase 6: Enterprise・Offline

- Helm/HA
- offline image、license、config import
- backup/DR
- monitoring integration

終了条件: 外部通信なしの起動と更新を確認。

### Phase 7: Advanced

- /v1/responses
- tools/MCP連携
- R(m)自動選択
- Hybrid mode

終了条件: Policy優先と本文経路をsecurity reviewで確認。

### 顧客導入の目安

| 段階 | 期間目安 | 成果物 | 終了条件 |
|---|---:|---|---|
| Assessment | 2〜3週 | 用途、分類、model、GPU、責任分界 | 対象業務と条件を合意 |
| PoC | 4〜6週 | 単一model、Gateway、key、基本audit | 品質・性能・security達成 |
| MVP | 8〜12週 | HA、管理、policy、billing連携、運用 | 受入と運用移管完了 |
| Enterprise | 継続 | 自動選択、MCP、複数拠点、DR、SLA | 定常運用 |

PoCは1社、1 tenant、1 project、1 local modelから開始し、Strict Local Modeとcloud fallback無効を先に検証する。

---

## 26. テスト仕様

### 26.1 Unit

- key hash/verify
- tenant scope
- policy decision
- routing priority
- config signature
- manifest checksum
- token one-time use
- error mapping
- secret redaction

### 26.2 Integration

- app auth/RBACとGateway管理API
- DB migrationとrollback/compatibility
- package buildからdownload
- Agent registerからconfig適用
- GatewayからvLLM/Ollama
- usageから既存billing integration

### 26.3 E2E

- 企業管理者が作成、package DL、install、registerできる。
- OpenAI SDKでmodels、chat、stream、embeddingsを利用できる。
- 期限切れURLとtoken再利用を拒否する。
- 設定変更を署名検証して適用する。
- update失敗からrollbackする。
- offline版が外部通信なしで起動する。

### 26.4 Security

- tenant Aからtenant Bへアクセス不可
- IDOR、権限昇格、CSRF、SSRF
- revoked key、expired JWT、invalid certificate
- package/config改ざん
- secret scan、dependency scan、container scan
- prompt/responseがControl Plane通信、log、metric、traceへ含まれない
- confidential/restrictedが外部経路へ送信されない
- headerのtenant偽装を拒否

### 26.5 Failure

- Runtime停止
- GPU OOM
- Control Plane停止
- config署名不正
- audit storage停止
- disk full
- update migration失敗
- clock skew

テスト未実行を成功扱いにしない。実環境が必要な項目は未検証理由と検証手順を報告する。

---

## 27. 受入基準

| ID | 項目 | 合格条件 |
|---|---|---|
| AT-01 | API互換 | 指定OpenAI SDKでChat/Streaming/Models/Embeddingsが利用可能 |
| AT-02 | 企業限定 | 個人userから画面・APIとも利用不可 |
| AT-03 | Tenant分離 | 他tenantのGateway、key、model、package、auditへアクセス不可 |
| AT-04 | Strict Local | confidential/restricted本文が顧客環境外へ送信されない |
| AT-05 | 認証・認可 | revoked key、expired JWT、未許可modelを拒否 |
| AT-06 | 本文非保存 | 既定でDB、log、metric、traceに本文が残らない |
| AT-07 | 配布 | 署名済みCompose/Helm packageを安全にDL可能 |
| AT-08 | 導入 | Node.js/npmなしで指定環境へ導入可能 |
| AT-09 | 設定 | 署名検証、世代管理、Last Known Goodが動作 |
| AT-10 | 障害復旧 | Runtime障害、制御断、更新失敗から復旧 |
| AT-11 | Audit | 重要操作と推論にrequest_id付き記録が存在 |
| AT-12 | Performance | 合意した同時数、Gateway p95、TTFTを達成 |
| AT-13 | License | 本番modelとGatewayに承認・期限・証跡が存在 |
| AT-14 | Offline | Air-Gapped版が外部通信なしで起動・更新可能 |
| AT-15 | Operations | monitoring、backup、連絡、脆弱性対応の責任が確定 |

---

## 28. Definition of Done

- 既存リポジトリ規約へ適合している。
- 既存認証、課金、監査、個人/企業分岐を壊していない。
- migration、API、UI、worker、Gateway、CLIがレビュー可能に分離されている。
- unit、integration、E2E、security testが追加されている。
- lint、typecheck、build、既存回帰testが成功している。
- OpenAPIまたは既存API仕様が更新されている。
- config schemaとmigration方針が文書化されている。
- checksum、signature、SBOMを生成・検証できる。
- Strict Local Modeの通信試験結果がある。
- update/rollback試験結果がある。
- environment、secret、導入、運用、復旧手順がある。
- 未実装、暫定対応、既知問題が明記されている。

---

## 29. 要決定事項

| ID | 論点 | 推奨初期値 | 決定時期 |
|---|---|---|---|
| D-01 | 初期model | Qwen等をlicense、日本語、GPUで選定 | PoC前 |
| D-02 | Runtime | 高負荷vLLM、簡易Ollama | PoC前 |
| D-03 | 本文log | OFF、顧客暗号化領域のみ例外 | 契約前 |
| D-04 | Control Plane項目 | 本書のallowlistを基準 | MVP前 |
| D-05 | SLA責任 | Gateway、GPU、NW、modelを分離 | 見積前 |
| D-06 | 外部API | 個別審査、WAF、mTLS/OAuth | 提供前 |
| D-07 | 評価 | Golden Setと評価者version固定 | MVP前 |
| D-08 | 業法・契約 | 通信形態確定後に専門家確認 | 契約前 |
| D-09 | 技術stack | 既存repo調査後に決定 | 着手前 |

未決事項を推測で固定せず、interface、config、feature flagで分離する。

---

## 30. Claude Codeの最終報告形式

~~~markdown
## 既存構成の調査結果

## 本設計との対応表

## 実装概要

## 変更ファイル

## DBマイグレーション

## API追加・変更

## 画面追加・変更

## Gateway・Adapter

## Package・Installer・CLI

## セキュリティ対策

## 実行したテストと結果

## 導入・更新・復旧手順

## 未決事項・未検証項目・既知問題
~~~

Claude Codeは、実行していないテストを成功と報告してはならない。GPU、Kubernetes、KMS、顧客IdP等がなく検証できない場合は、未検証理由、必要環境、再現可能な検証手順を記載する。

---

## 改訂履歴

| 版 | 日付 | 内容 |
|---|---|---|
| v1.0 | 2026-08-05 | 初版 |
| v1.1 Claude Code Edition | 2026-08-06 | 既存基本設計と導入・配布仕様を統合し、実装指示、論理API、データモデル、テスト、DoDを追加 |
