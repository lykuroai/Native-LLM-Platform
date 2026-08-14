package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lykuroai/Native-LLM-Platform/gwcore"
	"github.com/lykuroai/Native-LLM-Platform/token"
	"github.com/stretchr/testify/require"
)

func TestRunAdminToken(t *testing.T) {
	tests := []struct {
		name     string
		withOut  bool
		preExist bool // -out 先が既に存在する
		wantCode int
	}{
		{name: "no out flag", withOut: false, wantCode: 0},
		{name: "out flag writes token file", withOut: true, wantCode: 0},
		{name: "out flag refuses existing file", withOut: true, preExist: true, wantCode: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("LYKURO_DATA_DIR", dataDir)

			var args []string
			outPath := filepath.Join(t.TempDir(), "admin-token.txt")
			if tt.withOut {
				args = []string{"-out", outPath}
			}
			if tt.preExist {
				require.NoError(t, os.WriteFile(outPath, []byte("old\n"), 0o600))
			}

			code := runAdminToken(args)
			require.Equal(t, tt.wantCode, code)
			if tt.wantCode != 0 {
				// 失敗時は既存ファイルを壊さない
				b, err := os.ReadFile(outPath)
				require.NoError(t, err)
				require.Equal(t, "old\n", string(b))
				return
			}

			hashBytes, err := os.ReadFile(filepath.Join(dataDir, gwcore.AdminTokenHashFile))
			require.NoError(t, err)
			hash := strings.TrimSpace(string(hashBytes))
			require.NotEmpty(t, hash)

			if tt.withOut {
				b, err := os.ReadFile(outPath)
				require.NoError(t, err)
				plain := strings.TrimSpace(string(b))
				require.True(t, strings.HasPrefix(plain, gwcore.AdminTokenPrefix))
				require.Equal(t, hash, token.HashToken(plain))
				info, err := os.Stat(outPath)
				require.NoError(t, err)
				require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			}
		})
	}
}
