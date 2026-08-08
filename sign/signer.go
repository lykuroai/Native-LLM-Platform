// Package privategw implements the Private LLM Gateway distribution pipeline
// (docs/20260806/ LYK-PLG-ADD-001 §10): package building, Ed25519 signing and
// S3-compatible storage with short-lived download URLs.
//
// 既存の SaaS 本体(internal/gateway)・MCPゲートウェイとの名前衝突回避のため
// パッケージ名は privategw とする。
package sign

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

// Signer signs package manifests with an Ed25519 private key loaded from a
// PKCS#8 PEM file (Q-4: PoC は secret file、Enterprise 前に KMS へ移行予定)。
type Signer struct {
	priv ed25519.PrivateKey
}

// NewSignerFromFile loads an Ed25519 private key (PKCS#8 PEM) from path.
// 鍵の生成例: openssl genpkey -algorithm ed25519 -out pgw_signing.pem
func NewSignerFromFile(path string) (*Signer, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("signing key %s: no PEM block found", path)
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse signing key: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("signing key %s: not an Ed25519 key", path)
	}
	return &Signer{priv: priv}, nil
}

// Sign returns the Ed25519 signature of data.
func (s *Signer) Sign(data []byte) []byte {
	return ed25519.Sign(s.priv, data)
}

// PublicKey returns the corresponding verification key.
func (s *Signer) PublicKey() ed25519.PublicKey {
	return s.priv.Public().(ed25519.PublicKey)
}

// PublicKeyPEM returns the verification key as PKIX PEM (配布物へ同梱し、
// インストーラーが署名検証に使う)。
func (s *Signer) PublicKeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(s.PublicKey())
	if err != nil {
		return nil, fmt.Errorf("marshal public key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}
