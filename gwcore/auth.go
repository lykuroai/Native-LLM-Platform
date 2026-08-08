package gwcore

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lykuroai/Native-LLM-Platform/token"
)

// VirtualKeyPrefix is the customer-gateway key prefix. Lykuro SaaS の APIキー
// (lk_)・install token(lkgwt_) と接頭辞を分け、取り違えを検証段階で弾く。
const VirtualKeyPrefix = "lkpgw_"

// GenerateVirtualKey returns (plaintext, sha256hex). 原文は発行時のみ表示し、
// 設定にはハッシュだけを書く(BD §10.4)。
func GenerateVirtualKey() (plain, hash string, err error) {
	seg, err := token.RandomTokenSegment(44)
	if err != nil {
		return "", "", fmt.Errorf("generate key: %w", err)
	}
	plain = VirtualKeyPrefix + seg
	return plain, HashKey(plain), nil
}

// HashKey returns the storage hash for a plaintext key.
func HashKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Authenticator validates bearer keys against the config (BD §10.1)。
type Authenticator struct {
	byHash map[string]*VirtualKeyDef
	rl     *rateLimiter
}

func NewAuthenticator(cfg *Config) *Authenticator {
	byHash := make(map[string]*VirtualKeyDef, len(cfg.Auth.VirtualKeys))
	for i := range cfg.Auth.VirtualKeys {
		k := &cfg.Auth.VirtualKeys[i]
		byHash[k.KeyHash] = k
	}
	return &Authenticator{byHash: byHash, rl: newRateLimiter()}
}

// Authenticate resolves the Authorization header to a virtual key.
// 失敗理由は呼出側でコード化する(鍵原文はログへ出さない)。
func (a *Authenticator) Authenticate(authorization string) (*VirtualKeyDef, bool) {
	token := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
	if token == "" || !strings.HasPrefix(token, VirtualKeyPrefix) {
		return nil, false
	}
	hash := HashKey(token)
	k, ok := a.byHash[hash]
	if !ok || k.Disabled {
		return nil, false
	}
	// map lookup 後の等価確認は constant-time 比較で行う
	if subtle.ConstantTimeCompare([]byte(hash), []byte(k.KeyHash)) != 1 {
		return nil, false
	}
	return k, true
}

// AllowsModel reports whether the key may use the logical model.
func (k *VirtualKeyDef) AllowsModel(logical string) bool {
	if len(k.AllowedModels) == 0 {
		return true
	}
	for _, m := range k.AllowedModels {
		if m == logical {
			return true
		}
	}
	return false
}

// CheckRate consumes one request slot (fixed window per minute)。
func (a *Authenticator) CheckRate(k *VirtualKeyDef) bool {
	if k.RPMLimit <= 0 {
		return true
	}
	return a.rl.allow(k.ID, k.RPMLimit)
}

// rateLimiter is a per-key fixed-window RPM counter.
// 単一プロセスMVP用。HA構成(§20.2)では共有ストアへ置換する。
type rateLimiter struct {
	mu      sync.Mutex
	windows map[string]*rateWindow
	now     func() time.Time
}

type rateWindow struct {
	start time.Time
	count int
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{windows: map[string]*rateWindow{}, now: time.Now}
}

func (r *rateLimiter) allow(keyID string, limit int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	w := r.windows[keyID]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &rateWindow{start: now}
		r.windows[keyID] = w
	}
	if w.count >= limit {
		return false
	}
	w.count++
	return true
}
