#!/usr/bin/env bash
# Lykuro Private Gateway installer (macOS / Linux, amd64 / arm64)。
# GitHub Releases からバイナリを取得し checksums.txt で検証して配置する
# **だけ** — サービス定義・常駐化は行わない(設計原則: 常駐化は利用者の
# 流儀に委ねる)。
#
#   curl -fsSL https://raw.githubusercontent.com/lykuroai/Native-LLM-Platform/main/deploy/install.sh | bash
#
# Env overrides:
#   LYKURO_VERSION  取得するリリースタグ(既定: 最新リリース)
#   LYKURO_PREFIX   配置先ディレクトリ(既定: /usr/local/bin)
set -euo pipefail

# スクリプト全体を関数に包む: `curl | bash` 実行時、bash は main 呼出し前に
# 全文をパースして入力を消費するため、途中失敗で curl (23) Failed writing
# body に化けず、本当のエラーがそのまま表示される。
main() {

REPO="lykuroai/Native-LLM-Platform"
PREFIX="${LYKURO_PREFIX:-/usr/local/bin}"
VERSION="${LYKURO_VERSION:-}"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux)  os=linux ;;
  *) echo "unsupported OS: $(uname -s) (Windows は deploy/install.bat を使ってください)" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac

if [ -z "$VERSION" ]; then
  # GitHub API は未認証だと rate limit (403) になりやすいため使わない。
  # /releases/latest のリダイレクト先 URL からタグを取り出す。
  location="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
    "https://github.com/${REPO}/releases/latest")" || location=""
  VERSION="${location##*/}"
  case "$VERSION" in
    v*) ;;
    *) echo "failed to resolve latest release (got: ${location:-no response}). Set LYKURO_VERSION=vX.Y.Z and retry." >&2; exit 1 ;;
  esac
fi

ASSET="private-gateway_${VERSION}_${os}_${arch}"
BASE="https://github.com/${REPO}/releases/download/${VERSION}"

tmpdir="$(mktemp -d)"; trap 'rm -rf "$tmpdir"' EXIT
echo "==> downloading ${ASSET} (${VERSION})"
curl -fsSL -o "${tmpdir}/${ASSET}" "${BASE}/${ASSET}"
curl -fsSL -o "${tmpdir}/checksums.txt" "${BASE}/checksums.txt"

echo "==> verifying checksum"
want="$(grep -E " ${ASSET}\$" "${tmpdir}/checksums.txt" | awk '{print $1}')"
[ -n "$want" ] || { echo "checksums.txt has no entry for ${ASSET}" >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "${tmpdir}/${ASSET}" | awk '{print $1}')"
else
  got="$(shasum -a 256 "${tmpdir}/${ASSET}" | awk '{print $1}')"
fi
[ "$got" = "$want" ] || { echo "checksum mismatch: $got != $want" >&2; exit 1; }

DEST="${PREFIX}/private-gateway"
echo "==> installing to ${DEST}"
chmod +x "${tmpdir}/${ASSET}"
if mkdir -p "$PREFIX" 2>/dev/null && [ -w "$PREFIX" ]; then
  mv "${tmpdir}/${ASSET}" "$DEST"
else
  sudo mkdir -p "$PREFIX"
  sudo mv "${tmpdir}/${ASSET}" "$DEST"
fi

"$DEST" version
cat <<'EOF'

Get started:
  private-gateway init     # 稼働中 Runtime を検出して gateway.yaml を自動生成
  private-gateway serve    # ゲートウェイ起動
(常駐化は行いません — systemd / launchd 等はお使いの流儀でどうぞ)
EOF
if ! command -v private-gateway >/dev/null 2>&1; then
  echo "NOTE: $PREFIX is not on your PATH — add it or run $DEST directly." >&2
fi

}
main "$@"
