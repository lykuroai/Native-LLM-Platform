# CLAUDE.md

このファイルは、Claude Code がこのリポジトリで作業する際の指針です。

## プロジェクト概要

**プロダクト名**: Lykuro Native LLM Platform
**実体**: 顧客環境(オンプレ/VPC/端末)で稼働する Private LLM Gateway。ローカルLLM Runtime(vLLM / Ollama / TGI / OpenAI互換)を OpenAI 互換 API として提供する**単一バイナリのCLI**。
**ライセンス**: Apache-2.0(公開リポジトリ。secret・顧客情報を絶対にコミットしない)
**由来**: 2026-08-08 に独立プロジェクト化(履歴的経緯のみ)。以後の仕様・設計判断は**本リポジトリで自己完結**して管理する。他リポジトリへの依存はない(逆に、公開パッケージを外部からimportする利用者は存在する)。

## 設計原則(変更前に必ず確認)

1. **単一バイナリ・素のCLI** — サービス定義・インストーラ・ローカル管理UIを持たない。常駐化は利用者の流儀に委ねる
2. **DBなし** — 永続化はローカルファイルのみ(資格情報・設定世代・監査JSONL・暗号化会話記憶)
3. **Zero-Retention** — プロンプト/レスポンス本文をログ・監査・メトリクスに書かない(`content_logged: false` をテストで担保)
4. **設定は署名配信が権威** — SaaS(app.lykuro.ai)が Ed25519 署名した世代のみ適用。ローカルでの設定改変経路を増やさない
5. **`sign`・`token`・`platform/contract` は安定公開API** — 外部利用者が version 指定で import する。正規形JSON・署名・ライセンス検証・トークン形式・契約バージョン(`gateway-platform-v1` 等)の破壊的変更は semver メジャーで行い、CHANGELOG に明記する
6. **ワイヤ互換** — Agent API・署名済み config スキーマは稼働中の Control Plane と噛み合うプロトコル。変更は後方互換を基本とし、契約テスト(contract_test.go / engine_test.go)の更新を伴う設計判断として扱う

## 構成

| パス | 内容 |
|------|------|
| `cmd/private-gateway/` | CLIエントリポイント(serve/genkey/register/precheck/status/diagnose/config/upgrade/version) |
| `gwcore/` | ゲートウェイ本体(認証・プロキシ・設定・メトリクス・SaaS Agent) |
| `platform/` | Platform統合(contract / enginecontract / orchestrator / memory / modelmanager / pool) |
| `sign/` | Ed25519署名・ライセンス・正規形JSON(**本体と共有**) |
| `token/` | トークン生成・ハッシュ(**本体と共有**) |
| `deploy/` | Docker Compose / Helm(任意) |

外部依存は chi / yaml.v3 / testify のみ。**依存追加は要相談**(単一バイナリ・監査容易性を優先)。

## 開発

```bash
make build   # bin/private-gateway
make test    # go test ./...
make lint    # gofmt + go vet
make dist    # 3OS 5バイナリ+checksums.txt(Windowsはzip)
```

- Go 1.26+。gofmt/go vet 必須。テーブルドリブンテスト
- リリース: `git tag vX.Y.Z && git push origin vX.Y.Z` → Actions が GitHub Releases へ公開
- コミットは Conventional Commits(公開repoのため日本語bodyは可・secretや顧客名は書かない)

## Control Plane との接続(任意機能)

env 未設定ならスタンドアロン。接続時は `LYKURO_CONTROL_PLANE_URL=https://api.lykuro.ai` +
`LYKURO_SIGNING_PUB_FILE` + `LYKURO_INSTALL_TOKEN_FILE`(初回のみ)。
Agent API(`/api/gateway-agent/v1/*`)のクライアント実装は `gwcore/agent.go`。
