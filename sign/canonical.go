package sign

import (
	"encoding/json"
	"fmt"
)

// CanonicalJSON re-marshals JSON deterministically (Goのmapキー整列に依存)。
// 設定署名は必ずこの正規形に対して行う: PostgreSQL JSONB がキー順・空白を
// 保存しないため、原文バイトへの署名は往復で壊れる。署名側(制御プレーン)と
// 検証側(Agent)の両方が同じ正規化を通すことで一致させる。
func CanonicalJSON(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("canonical json: %w", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonical json: %w", err)
	}
	return out, nil
}
