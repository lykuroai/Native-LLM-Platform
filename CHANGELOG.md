# Changelog

## v0.7.1 (2026-08-13)

- docs(readme): 社内ローカルLLM を安全に外部公開する構成(Virtual Key 認証・レート制限・Zero-Retention・リバースプロキシでの TLS 終端)を追加

## v0.7.0 (2026-08-13)

### 破壊的変更

- **ライセンス変更**: Apache License 2.0 → [PolyForm Noncommercial License 1.0.0](LICENSE)
  - 非商用利用(個人・研究・教育・評価・非営利組織)は引き続き無料
  - 商用利用には別途商用ライセンスが必要
  - `sign` / `token` / `platform/contract` を import する外部利用者にも適用される
  - v0.6.0 以前のバージョンは Apache-2.0 のまま利用可能(遡及しない)
