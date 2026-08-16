# Changelog

## v0.9.0 (2026-08-16)

- feat(workflow): マルチRuntime連鎖推論 / Workflow Orchestrator W1 を追加(LYK-NLP-MRCI-001 v1.1 を LYK-NLP-MRCI-002 で本リポジトリへ縮退適用)
  - Flow 定義(JSON)の Draft / Validation / 不変 Version 公開 / Alias / Suspend / Retire。永続化はローカルファイルのみ(`<workflows.data_dir>/flows/`、DBなし)
  - Sequential 連鎖実行・Safe Template Engine(許可変数のみ・再帰展開なし)・Retry(Timeout は二重推論防止のため非 failover)・承認済み Pool への Fallback・Fail Closed
  - データプレーン API: `POST /v1/workflows/{alias}/runs`(sync/stream)、Run/Steps 照会、SSE Event(Last-Event-ID 再開)、Cancel、`Idempotency-Key` 対応
  - OpenAI 互換: `model: "flow:{alias}"` で `/v1/chat/completions` から公開 Flow を実行(必須 Input 1 つの Flow のみ)
  - 管理プレーン: admin listener に `/api/workflows*` と管理画面「Workflows」「Workflow Runs」タブを追加
  - Zero-Retention 維持: Step Input/Output 本文はメモリのみ(Run メタデータ・Event・監査に本文なし、`content_logged: false`)
- feat(platform): `platform.pools[]`(Deployment の論理集合)と `platform.workflows` 設定節を追加
- feat(contract): `contract.Request` に `PoolID` を追加(候補 Deployment の Pool 絞り込み。追加のみ・ゼロ値で従来動作の後方互換変更)
- **検証強化**: Deployment ID の catalog 全体一意を明示的に強制(pool registry の従来からの暗黙前提)。logical_name の `flow:` prefix を予約として拒否

## v0.8.1 (2026-08-15)

- fix(cli): 設定ファイルの既定パスを `/etc/lykuro/gateway/gateway.yaml` からカレントディレクトリの `gateway.yaml` に変更。root 権限のない環境で `-config` 未指定の `init` が `mkdir /etc/lykuro: permission denied` で失敗する問題を修正(優先順位は従来どおり `LYKURO_CONFIG_PATH` > `-config` > 既定)

## v0.8.0 (2026-08-15)

- feat(cli): `admin-token -out <file>` を追加。管理画面トークンの原文を画面表示せず指定ファイルへ保存できる(0600・既存ファイルへは上書き拒否)
- **変更**: `LYKURO_DATA_DIR` 未設定時の既定を `/var/lib/lykuro/gateway` からカレントディレクトリに変更。従来の場所に状態がある既存環境は `LYKURO_DATA_DIR=/var/lib/lykuro/gateway` を明示してください(Helm は明示済みで影響なし。docker-compose 例には明示を追加)

## v0.7.2 (2026-08-15)

- docs(readme): Virtual Key の保存仕様を明記(原文は一度きり表示・gateway.yaml に SHA-256 ハッシュのみ保存・アトミック置換 0600・DBなし)

## v0.7.1 (2026-08-13)

- docs(readme): 社内ローカルLLM を安全に外部公開する構成(Virtual Key 認証・レート制限・Zero-Retention・リバースプロキシでの TLS 終端)を追加

## v0.7.0 (2026-08-13)

### 破壊的変更

- **ライセンス変更**: Apache License 2.0 → [PolyForm Noncommercial License 1.0.0](LICENSE)
  - 非商用利用(個人・研究・教育・評価・非営利組織)は引き続き無料
  - 商用利用には別途商用ライセンスが必要
  - `sign` / `token` / `platform/contract` を import する外部利用者にも適用される
  - v0.6.0 以前のバージョンは Apache-2.0 のまま利用可能(遡及しない)
