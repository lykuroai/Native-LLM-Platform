package sign

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestSigner writes an ephemeral Ed25519 key as PKCS#8 PEM and loads it.
func newTestSigner(t *testing.T) *Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "signing.pem")
	require.NoError(t, os.WriteFile(path,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
	s, err := NewSignerFromFile(path)
	require.NoError(t, err)
	return s
}

func TestSignerRoundtrip(t *testing.T) {
	s := newTestSigner(t)
	msg := []byte("checksums content")
	sig := s.Sign(msg)
	assert.True(t, ed25519.Verify(s.PublicKey(), msg, sig))
	assert.False(t, ed25519.Verify(s.PublicKey(), []byte("tampered"), sig))

	pemBytes, err := s.PublicKeyPEM()
	require.NoError(t, err)
	assert.Contains(t, string(pemBytes), "PUBLIC KEY")
}

func TestNewSignerFromFileErrors(t *testing.T) {
	_, err := NewSignerFromFile(filepath.Join(t.TempDir(), "missing.pem"))
	assert.Error(t, err)

	bad := filepath.Join(t.TempDir(), "bad.pem")
	require.NoError(t, os.WriteFile(bad, []byte("not pem"), 0o600))
	_, err = NewSignerFromFile(bad)
	assert.ErrorContains(t, err, "no PEM block")
}
