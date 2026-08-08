# Private LLM Gateway 受入テスト計画・実施状況

| 項目 | 内容 |
|---|---|
| 対象 | LYK-PLG-BD-001 §27 (AT-01〜15) / LYK-PLG-ADD-001 §22 (AT-01〜12) |
| 作成日 | 2026-08-07 |
| 実装ブランチ | `feat/private-gateway` (PR #160〜#165) |

凡例: ✅ 自動テストで実証済 / 🔶 部分的に実証(残作業あり) / ⬜ 実環境が必要(手順のみ)

## BD §27 受入基準の実施状況

| ID | 項目 | 状況 | 根拠・残作業 |
|---|---|---|---|
| AT-01 | API互換(Chat/Streaming/Models/Embeddings) | ✅ | httptestモック(`gwcore/server_test.go`)に加え、実Runtime(Ollama qwen2.5:0.5b/all-minilm)+公式OpenAI Python SDKで models/chat/stream/embeddings 疎通済(2026-08-07、M-1実施。usage sniff・監査記録も確認) |
| AT-02 | 企業限定(個人userから利用不可) | ✅ | `ENTERPRISE_ENABLED` ゲート + `requireMembership`(非メンバー403)。vitest で画面出し分け検証 |
| AT-03 | Tenant分離 | ✅ | integration: 他tenantからの GET/遷移/失効/claim/パッケージ参照が全て不可(`private_gateway_repository_test.go` ほか) |
| AT-04 | Strict Local(本文が外部へ出ない) | ✅ | `TestStrictLocalNeverUsesCloud`・`TestHybridPolicyPrecedence`(4態様で cloud hit 0)。Agent送信は許可リストのみ(サーバー側検証) |
| AT-05 | 認証・認可(revoked key/未許可model拒否) | ✅ | `TestAuthFailures`・`TestPolicyDenials`・失効Gateway のAgent認証403 |
| AT-06 | 本文非保存 | ✅ | 監査JSONLに本文・鍵が無いことを直接検証(`TestChatCompletionProxy`)。`content_logged:false` 固定 |
| AT-07 | 署名済みパッケージの安全なDL | ✅ | MinIO integration(presign往復)・改ざん3態様検知・期限/失効拒否・DL監査 |
| AT-08 | Node.js/npmなし導入 | 🔶 | 単一静的バイナリ+distroless。**実機(顧客相当環境)での導入試験は未実施** → M-2 |
| AT-09 | 設定(署名検証・世代・LKG) | ✅ | Agent E2E 8件 + `-force`巻き戻し/drift 防止テスト。決定性はビルド側も実証 |
| AT-10 | 障害復旧(Runtime障害・制御断・更新失敗) | 🔶 | Runtime障害(fallback/503)・制御断(LKG継続・usage復元)は自動テスト済。**更新失敗→rollback は CLI upgrade 未実装のため対象外**(K8s は Rolling+probes) |
| AT-11 | Audit(request_id付き記録) | ✅ | 全経路で request_id 付き JSONL + enterprise_audit_logs + admin_audit_logs(ライセンス) |
| AT-12 | Performance(同時数・p95・TTFT) | ⬜ | 未実施 → M-3(k6、合意値の設定が前提) |
| AT-13 | License(承認・期限・証跡) | ✅ | 発行=admin限定+監査、検証3状態+改ざん/流用拒否、期限超過起動拒否は実バイナリで確認済 |
| AT-14 | Offline(外部通信なし起動・更新) | 🔶 | config import(署名検証・外部通信なし)+Air-Gapped起動は実バイナリ確認済。**OCIイメージtar(offline-images.tar)は未実装** |
| AT-15 | Operations(監視・backup・責任分界) | 🔶 | runbook 作成済(SaaS側リポジトリ doragogonet/lykuro の docs/runbooks/private_gateway_operations.md)。/metrics 実装済(2026-08-07、LYKURO_METRICS_ENABLED)。顧客との責任分界は契約作業 |

## 実環境での手動検証手順(マージ後・PoC前)

### M-1: 実 Runtime + OpenAI SDK 疎通(AT-01) — ✅ 実施済(2026-08-07)
1. Ollama を起動し任意モデルを pull(`ollama pull qwen3:8b` 等)
2. gateway.yaml: `endpoint: "http://localhost:11434"` / `physical_model: qwen3:8b`
3. `private-gateway genkey` でキー発行 → yaml に hash 追記 → `serve`
4. OpenAI SDK(python/node)で `base_url=http://<gw>:8443/v1` を指定し models / chat(stream=true) / embeddings を実行
5. audit.jsonl の usage(tokens)・latency を確認

### M-2: 導入試験(AT-08)
1. Node.js/npm の無い Linux VM(amd64)を用意
2. `docker build -f Dockerfile.private-gateway` → `docker save` で搬入、または `GOOS=linux go build` した単一バイナリを配置
3. `precheck` → `config validate` → `serve` を非root で実行し、sudo を要求しないことを確認

### M-3: 性能(AT-12)
- k6 スクリプト整備済(2026-08-07): `test/loadtest/private_gateway/gateway_load.js`(手順は同階層 README.md。Runtime直接→Gateway経由の差分で追加遅延を算出)
- 合意値(同時数・Gateway p95 200ms・TTFT)を先に決めること(BD §20.1)。実行は顧客相当環境で(未実施)

### M-4: 制御プレーン結合(AT-09/13 のE2E)
1. ステージングで migration 034/035 適用、`PGW_PACKAGE_S3_BUCKET`/`PGW_SIGNING_KEY_FILE` 構成
2. 画面から Gateway作成→パッケージ生成→DL→検証(署名/checksums)→install token 発行
3. 顧客相当環境で `register` → heartbeat で online 化 → 設定投入(PUT config)→ Agent 適用を確認
4. admin コンソールからライセンス発行 → `LYKURO_LICENSE_FILE` 配置 → 再起動で「license valid」ログ

## 既知の未実装(受入対象外として合意すべき項目)
- `offline-images.tar`(/metrics・CLI `upgrade`/`rollback` は 2026-08-07 実装済)
- HA(Helm は PVC 使用時 1 replica)・R(m)自動選択・MCP Gateway 実接続
- usage の at-least-once 二重計上の厳密化(参考値集計のため許容)
