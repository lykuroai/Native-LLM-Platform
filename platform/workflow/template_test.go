package workflow

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenderTemplate(t *testing.T) {
	vars := map[string]string{
		"inputs.q":         "hello",
		"steps.a.output":   "world",
		"run.id":           "wrun_x",
		"inputs.injected":  "{{inputs.q}}", // 展開値は再展開されない
		"inputs.question1": "q1",
	}
	cases := []struct {
		name    string
		tpl     string
		want    string
		wantErr string
	}{
		{"plain", "no refs", "no refs", ""},
		{"single", "{{inputs.q}}", "hello", ""},
		{"spaces", "{{ inputs.q }}", "hello", ""},
		{"multi", "A:{{inputs.q}} B:{{steps.a.output}} R:{{run.id}}", "A:hello B:world R:wrun_x", ""},
		{"no recursive expansion", "{{inputs.injected}}", "{{inputs.q}}", ""},
		{"undefined", "{{inputs.nope}}", "", "undefined template reference"},
		{"unclosed", "{{inputs.q", "", "malformed"},
		{"invalid path chars", "{{inputs.q; rm -rf}}", "", "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RenderTemplate(tc.tpl, vars)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestTemplateRefs(t *testing.T) {
	refs := TemplateRefs("{{inputs.a}} x {{steps.s1.output}} {{run.id}}")
	require.Equal(t, []string{"inputs.a", "steps.s1.output", "run.id"}, refs)
}
