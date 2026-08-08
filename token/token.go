package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

// AgentTokenPrefix marks long-lived agent credentials (BD §13.1)。
// install token(lkgwt_)・SaaS APIキー(lk_)・顧客Gateway Virtual Key(lkpgw_)
// と接頭辞を分ける。
const AgentTokenPrefix = "lkpga_"

// tokenAlphabet excludes visually ambiguous characters (I/l/0/O/1)。
const tokenAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// RandomTokenSegment returns n uniformly distributed characters.
// crypto/rand.Int の棄却サンプリングを使う(バイト剰余方式は 256 mod 57 = 28
// の剰余バイアスが出る。internal/auth/apikey.go と同方式に統一)。
func RandomTokenSegment(n int) (string, error) {
	maxIdx := big.NewInt(int64(len(tokenAlphabet)))
	buf := make([]byte, n)
	for i := range buf {
		idx, err := rand.Int(rand.Reader, maxIdx)
		if err != nil {
			return "", fmt.Errorf("random token: %w", err)
		}
		buf[i] = tokenAlphabet[idx.Int64()]
	}
	return string(buf), nil
}

// GenerateAgentToken returns (plaintext, sha256hex)。原文は register 応答で
// のみ返し、DB にはハッシュだけを保存する。
func GenerateAgentToken() (plain, hash string, err error) {
	seg, err := RandomTokenSegment(48)
	if err != nil {
		return "", "", fmt.Errorf("generate agent token: %w", err)
	}
	plain = AgentTokenPrefix + seg
	return plain, HashToken(plain), nil
}

// HashToken returns the storage hash for any pgw token (install/agent).
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// BearerToken extracts the credential from an Authorization header.
func BearerToken(authorization string) string {
	return strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
}
