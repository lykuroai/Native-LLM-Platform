# Changelog

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
