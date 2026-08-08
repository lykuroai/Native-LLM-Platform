# Private LLM Gateway 性能試験(M-3 / AT-12)

BD §20.1 の目標「**Gateway 追加遅延** p95 200ms 以下(MVP)/ 100ms 以下(Enterprise)」を検証する。
推論時間・model load・顧客 network は追加遅延と**分離して測定する**(同 §20.1)。

## 前提

- 合意値(RPS・同時接続・p95・TTFT)を顧客/社内で先に確定すること(acceptance_test_plan M-3)
- k6 v0.50+
- 測定系と Gateway/Runtime は同一ネットワーク(WAN 越しは追加遅延に顧客 network が混入する)

## 手順

1. **Runtime 直接(ベースライン)**

   ```bash
   k6 run -e BASE_URL=http://<runtime>:8000 -e API_KEY=<runtime-key> \
          -e MODEL=<physical-model> -e RPS=50 gateway_load.js
   ```

   `gateway_latency_ms` の p50/p95 を記録する(この値は Runtime+network の素の時間)。

2. **Gateway 経由**

   ```bash
   k6 run -e BASE_URL=http://<gateway>:8443 -e API_KEY=lkpgw_... \
          -e MODEL=<logical-model> -e RPS=50 -e P95_MS=200 gateway_load.js
   ```

3. **追加遅延 = (2) − (1)** を分位点ごとに算出し、合意値と比較する。
   `/metrics`(`LYKURO_METRICS_ENABLED=true`)の `lykuro_pgw_request_duration_seconds` でも裏取りできる。

## 環境変数

| 変数 | 既定 | 意味 |
|---|---|---|
| `BASE_URL` | `http://127.0.0.1:8443` | 測定対象 |
| `API_KEY` | (空) | Virtual Key(`lkpgw_`)または Runtime キー |
| `MODEL` | `qwen-local` | logical / physical model |
| `RPS` | 50 | completions シナリオの到着率 |
| `DURATION` | 5m | 各シナリオの継続時間 |
| `P95_MS` | 200 | しきい値(Enterprise は 100) |
| `VUS` | 100 | 同時接続の合意値(BD §20.1: tenantあたり100) |
| `STREAM_VUS` | 10 | streaming 常時接続数(通常 RPS と別管理、BD §20.2) |

## 注意

- streaming の `ttft_ms` は k6 の TTFB 近似。厳密な TTFT(初回トークン)は
  Gateway の `/metrics` `lykuro_pgw_first_token_latency_seconds` を参照する
- 429/503 が出る場合は Virtual Key の `rpm_limit`・Runtime の並列上限を確認
  (過負荷時 429 は BD §20.2 の設計どおり)
- 本試験は顧客相当環境での実施が受入条件(ローカル mock での実行は smoke のみ)
