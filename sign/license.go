package sign

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// License is the offline-capable gateway license (docs/20260806/ BD §15,
// ADD §15)。署名は Signature を除いた正規形JSONに対する Ed25519。
type License struct {
	SchemaVersion string    `json:"schema_version"`
	TenantID      string    `json:"tenant_id"`
	GatewayID     string    `json:"gateway_id"`
	Edition       string    `json:"edition"`
	IssuedAt      time.Time `json:"issued_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	// GraceDays is the offline grace period after expiry (BD §15:
	// 期限内失効を即時停止にしない)。
	GraceDays int    `json:"grace_days"`
	Signature string `json:"signature,omitempty"`
}

// LicenseState is the verification outcome.
type LicenseState string

const (
	LicenseValid   LicenseState = "valid"
	LicenseInGrace LicenseState = "in_grace"
	LicenseExpired LicenseState = "expired"
)

// licenseSigningBytes returns the canonical bytes covered by the signature.
func licenseSigningBytes(l *License) ([]byte, error) {
	unsigned := *l
	unsigned.Signature = ""
	raw, err := json.Marshal(unsigned)
	if err != nil {
		return nil, fmt.Errorf("license marshal: %w", err)
	}
	return CanonicalJSON(raw)
}

// IssueLicense signs a license document and returns its JSON.
func IssueLicense(signer *Signer, tenantID, gatewayID, edition string, expiresAt time.Time, graceDays int, now time.Time) ([]byte, *License, error) {
	if signer == nil {
		return nil, nil, fmt.Errorf("license: signer not configured")
	}
	if graceDays < 0 {
		graceDays = 0
	}
	l := &License{
		SchemaVersion: "1",
		TenantID:      tenantID,
		GatewayID:     gatewayID,
		Edition:       edition,
		IssuedAt:      now.UTC().Truncate(time.Second),
		ExpiresAt:     expiresAt.UTC().Truncate(time.Second),
		GraceDays:     graceDays,
	}
	payload, err := licenseSigningBytes(l)
	if err != nil {
		return nil, nil, err
	}
	l.Signature = base64.StdEncoding.EncodeToString(signer.Sign(payload))
	out, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("license marshal: %w", err)
	}
	return out, l, nil
}

// VerifyLicense checks signature, identity binding and expiry.
// gatewayID が空でなければ一致を要求する(他Gatewayのライセンス流用防止)。
func VerifyLicense(raw []byte, pub ed25519.PublicKey, gatewayID string, now time.Time) (*License, LicenseState, error) {
	var l License
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, LicenseExpired, fmt.Errorf("license: parse: %w", err)
	}
	if l.SchemaVersion != "1" {
		return nil, LicenseExpired, fmt.Errorf("license: unsupported schema_version %q", l.SchemaVersion)
	}
	if l.Signature == "" {
		return nil, LicenseExpired, fmt.Errorf("license: unsigned")
	}
	sig, err := base64.StdEncoding.DecodeString(l.Signature)
	if err != nil {
		return nil, LicenseExpired, fmt.Errorf("license: decode signature: %w", err)
	}
	payload, err := licenseSigningBytes(&l)
	if err != nil {
		return nil, LicenseExpired, err
	}
	if !ed25519.Verify(pub, payload, sig) {
		return nil, LicenseExpired, fmt.Errorf("license: signature mismatch")
	}
	if gatewayID != "" && l.GatewayID != gatewayID {
		return nil, LicenseExpired, fmt.Errorf("license: issued for a different gateway")
	}
	switch {
	case now.Before(l.ExpiresAt):
		return &l, LicenseValid, nil
	case now.Before(l.ExpiresAt.Add(time.Duration(l.GraceDays) * 24 * time.Hour)):
		return &l, LicenseInGrace, nil
	default:
		return &l, LicenseExpired, fmt.Errorf("license: expired at %s (grace %dd exceeded)",
			l.ExpiresAt.Format(time.RFC3339), l.GraceDays)
	}
}
