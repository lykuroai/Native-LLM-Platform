package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func newStore(t *testing.T) (*FlowStore, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenFlowStore(dir, 10)
	require.NoError(t, err)
	return s, dir
}

func TestFlowLifecycle(t *testing.T) {
	s, dir := newStore(t)
	raw := validDef(t, nil)

	meta, err := s.Create(raw, "two-runtime-follow-up", "Chain")
	require.NoError(t, err)
	require.Equal(t, FlowDraft, meta.Status)
	require.Equal(t, 1, meta.DraftRevision)

	// alias 一意
	_, err = s.Create(raw, "two-runtime-follow-up", "Chain 2")
	require.ErrorIs(t, err, ErrAliasTaken)

	// 未公開 alias は解決不可
	_, _, err = s.ResolveAlias("two-runtime-follow-up")
	require.ErrorIs(t, err, ErrFlowNotFound)

	// draft 更新は revision 一致が必須
	_, err = s.UpdateDraft(meta.FlowID, 99, raw)
	require.ErrorIs(t, err, ErrRevisionConflict)
	d, err := s.UpdateDraft(meta.FlowID, 1, raw)
	require.NoError(t, err)
	require.Equal(t, 2, d.Revision)

	// publish
	_, err = s.Publish(meta.FlowID, 1)
	require.ErrorIs(t, err, ErrRevisionConflict)
	fv, err := s.Publish(meta.FlowID, 2)
	require.NoError(t, err)
	require.Equal(t, 1, fv.Version)
	require.NotEmpty(t, fv.Checksum)

	id, ver, err := s.ResolveAlias("two-runtime-follow-up")
	require.NoError(t, err)
	require.Equal(t, meta.FlowID, id)
	require.Equal(t, 1, ver)

	// 公開 Version は checksum 検証つきで取得できる
	got, err := s.GetVersion(meta.FlowID, 1)
	require.NoError(t, err)
	require.Equal(t, fv.Checksum, got.Checksum)

	// 再起動相当: 別インスタンスで index が復元される
	s2, err := OpenFlowStore(dir, 10)
	require.NoError(t, err)
	_, ver, err = s2.ResolveAlias("two-runtime-follow-up")
	require.NoError(t, err)
	require.Equal(t, 1, ver)
}

func TestPublishedVersionIsImmutable(t *testing.T) {
	s, dir := newStore(t)
	meta, err := s.Create(validDef(t, nil), "immutable-check", "x")
	require.NoError(t, err)
	_, err = s.Publish(meta.FlowID, 1)
	require.NoError(t, err)

	// ファイル改変は checksum 検証で拒否される(Fail Closed)
	path := filepath.Join(dir, "flows", meta.FlowID, "v1.json")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var fv FlowVersion
	require.NoError(t, json.Unmarshal(raw, &fv))
	var def map[string]any
	require.NoError(t, json.Unmarshal(fv.Definition, &def))
	def["name"] = "tampered"
	fv.Definition, _ = json.Marshal(def)
	tampered, _ := json.Marshal(fv)
	require.NoError(t, os.WriteFile(path, tampered, 0o600))

	_, err = s.GetVersion(meta.FlowID, 1)
	require.ErrorContains(t, err, "checksum")
}

func TestSuspendRetire(t *testing.T) {
	s, _ := newStore(t)
	meta, err := s.Create(validDef(t, nil), "lifecycle", "x")
	require.NoError(t, err)
	_, err = s.Publish(meta.FlowID, 1)
	require.NoError(t, err)

	// suspend で新規実行(alias 解決)を止める
	_, err = s.SetStatus(meta.FlowID, FlowSuspended)
	require.NoError(t, err)
	_, _, err = s.ResolveAlias("lifecycle")
	require.ErrorIs(t, err, ErrFlowNotFound)

	// resume
	_, err = s.SetStatus(meta.FlowID, FlowPublished)
	require.NoError(t, err)
	_, _, err = s.ResolveAlias("lifecycle")
	require.NoError(t, err)

	// retire は不可逆・過去 Version 参照は保持
	_, err = s.SetStatus(meta.FlowID, FlowRetired)
	require.NoError(t, err)
	_, err = s.SetStatus(meta.FlowID, FlowPublished)
	require.ErrorIs(t, err, ErrFlowRetired)
	_, err = s.UpdateDraft(meta.FlowID, 1, validDef(t, nil))
	require.ErrorIs(t, err, ErrFlowRetired)
	_, err = s.GetVersion(meta.FlowID, 1)
	require.NoError(t, err)
}

func TestMaxFlows(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenFlowStore(dir, 1)
	require.NoError(t, err)
	_, err = s.Create(validDef(t, nil), "one", "x")
	require.NoError(t, err)
	_, err = s.Create(validDef(t, nil), "two", "x")
	require.ErrorIs(t, err, ErrTooManyFlows)
}
