package sign

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLicenseIssueAndVerify(t *testing.T) {
	signer := newTestSigner(t)
	now := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	expires := now.AddDate(1, 0, 0)

	raw, lic, err := IssueLicense(signer, "tn-1", "gw-1", "enterprise", expires, 14, now)
	require.NoError(t, err)
	assert.Equal(t, "enterprise", lic.Edition)

	got, state, err := VerifyLicense(raw, signer.PublicKey(), "gw-1", now)
	require.NoError(t, err)
	assert.Equal(t, LicenseValid, state)
	assert.Equal(t, "tn-1", got.TenantID)
}

func TestLicenseGraceAndExpiry(t *testing.T) {
	signer := newTestSigner(t)
	now := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)
	expires := now.Add(24 * time.Hour)
	raw, _, err := IssueLicense(signer, "tn-1", "gw-1", "poc", expires, 14, now)
	require.NoError(t, err)

	// 期限内
	_, state, err := VerifyLicense(raw, signer.PublicKey(), "gw-1", expires.Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, LicenseValid, state)

	// 期限超過だが grace 内(BD §15: 即時停止しない)
	_, state, err = VerifyLicense(raw, signer.PublicKey(), "gw-1", expires.Add(3*24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, LicenseInGrace, state)

	// grace 超過は expired(Fail Closed 対象)
	_, state, verr := VerifyLicense(raw, signer.PublicKey(), "gw-1", expires.Add(15*24*time.Hour))
	assert.Equal(t, LicenseExpired, state)
	assert.ErrorContains(t, verr, "expired")
}

func TestLicenseRejections(t *testing.T) {
	signer := newTestSigner(t)
	now := time.Now()
	raw, _, err := IssueLicense(signer, "tn-1", "gw-1", "poc", now.AddDate(1, 0, 0), 14, now)
	require.NoError(t, err)

	// 改ざん(有効期限の書換)
	tampered := strings.Replace(string(raw), "gw-1", "gw-2", 1)
	_, _, verr := VerifyLicense([]byte(tampered), signer.PublicKey(), "gw-2", now)
	assert.ErrorContains(t, verr, "signature mismatch")

	// 別Gatewayへの流用
	_, _, verr = VerifyLicense(raw, signer.PublicKey(), "gw-other", now)
	assert.ErrorContains(t, verr, "different gateway")

	// 別鍵
	other := newTestSigner(t)
	_, _, verr = VerifyLicense(raw, other.PublicKey(), "gw-1", now)
	assert.ErrorContains(t, verr, "signature mismatch")

	// 未署名
	_, _, verr = VerifyLicense([]byte(`{"schema_version":"1","gateway_id":"gw-1"}`), signer.PublicKey(), "gw-1", now)
	assert.ErrorContains(t, verr, "unsigned")
}
