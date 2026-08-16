// Safe Template Engine (LYK-NLP-MRCI-001 §6.3 / MRCI-002 §4)。
//
// 許可された変数参照 {{path}} の単一パス置換のみを行う。任意コード実行・
// Shell・ファイル/環境変数参照・再帰展開は文法上存在しない。展開値は
// 再スキャンしない(無制限再帰展開の禁止)。
package workflow

import (
	"fmt"
	"regexp"
	"strings"
)

// refPattern matches {{ path }} where path is dotted identifiers。
var refPattern = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_]+(?:\.[A-Za-z0-9_]+)*)\s*\}\}`)

// looseBrace detects `{{` that did not match refPattern(構文エラーを
// 黙って素通しにしない — Fail Closed)。
var looseBrace = regexp.MustCompile(`\{\{`)

// TemplateRefs returns the variable paths referenced by tpl, in order。
func TemplateRefs(tpl string) []string {
	var refs []string
	for _, m := range refPattern.FindAllStringSubmatch(tpl, -1) {
		refs = append(refs, m[1])
	}
	return refs
}

// CheckTemplateSyntax rejects malformed {{...}} sequences。
func CheckTemplateSyntax(tpl string) error {
	stripped := refPattern.ReplaceAllString(tpl, "")
	if looseBrace.MatchString(stripped) {
		return fmt.Errorf("malformed template reference (unclosed or invalid {{...}})")
	}
	return nil
}

// RenderTemplate substitutes each {{path}} with vars[path]。未定義参照は
// エラー(黙って空文字にしない)。展開値は再展開されない。
func RenderTemplate(tpl string, vars map[string]string) (string, error) {
	if err := CheckTemplateSyntax(tpl); err != nil {
		return "", err
	}
	var missing []string
	out := refPattern.ReplaceAllStringFunc(tpl, func(m string) string {
		path := refPattern.FindStringSubmatch(m)[1]
		v, ok := vars[path]
		if !ok {
			missing = append(missing, path)
			return ""
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("undefined template reference: %s", strings.Join(missing, ", "))
	}
	return out, nil
}
