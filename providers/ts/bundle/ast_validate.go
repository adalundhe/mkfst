package bundle

import (
	"errors"
	"fmt"
	"regexp"
)

// === ValidateAST ===
//
// ValidateAST scans the bundled JS for forbidden constructs that
// could escape the bridge sandbox. This is a defense-in-depth
// layer; the primary defense is that the JS runtime simply doesn't
// expose `fs`, `process`, network, etc. AST validation makes
// "user wrote `eval(maliciousString)`" fail loudly at submit time
// rather than maybe-fail at exec time depending on whether the
// hostile path executes.
//
// We intentionally do not parse the JS into a real AST — esbuild
// produces deterministic output and we run regex checks against the
// well-known shapes. False positives are possible but acceptable;
// the user can adjust their code or appeal to the operator.

// Forbidden patterns. Keep additions small and well-justified.
//
// Defense in depth: the runtime sandbox doesn't expose the Node /
// browser surfaces these patterns try to exploit, but blocking at
// submit time turns "maybe-fail at exec time depending on whether
// the path runs" into "fail loudly at submit time, with a clear
// fix message." The patterns are deliberately pessimistic — false
// positives are easier to live with than runtime escapes.
var forbiddenPatterns = []struct {
	name string
	re   *regexp.Regexp
	hint string
}{
	// Direct eval / runtime code evaluation.
	{
		name: "eval",
		re:   regexp.MustCompile(`(?:^|[^\w.$])eval\s*\(`),
		hint: "eval is forbidden — bundle JS only, no runtime evaluation",
	},
	{
		name: "Function-constructor",
		re:   regexp.MustCompile(`new\s+Function\s*\(`),
		hint: "the Function constructor is a runtime-eval primitive and is forbidden",
	},
	{
		name: "Function-constructor-call",
		re:   regexp.MustCompile(`Function\s*\(\s*['"]`),
		hint: "Function('...') is a runtime-eval primitive and is forbidden",
	},
	{
		name: "dynamic-import",
		re:   regexp.MustCompile(`(?:^|[^\w.$])import\s*\(`),
		hint: "dynamic import() is forbidden — declare imports statically",
	},
	{
		name: "Function-prototype-constructor",
		re:   regexp.MustCompile(`\.constructor\s*\(\s*['"]`),
		hint: "calling a function's .constructor with a string is a runtime-eval reach-around (e.g. (()=>{}).constructor('return process'))",
	},
	// Prototype pollution.
	{
		name: "__proto__-assignment",
		re:   regexp.MustCompile(`__proto__\s*=`),
		hint: "__proto__ assignment can mutate Object.prototype globally — forbidden",
	},
	{
		name: "Object-setPrototypeOf",
		re:   regexp.MustCompile(`Object\s*\.\s*setPrototypeOf\s*\(`),
		hint: "Object.setPrototypeOf is a prototype-pollution vector — forbidden",
	},
	{
		name: "Reflect-setPrototypeOf",
		re:   regexp.MustCompile(`Reflect\s*\.\s*setPrototypeOf\s*\(`),
		hint: "Reflect.setPrototypeOf is a prototype-pollution vector — forbidden",
	},
	// Indirect runtime-eval reach-arounds.
	{
		name: "globalThis-Function",
		re:   regexp.MustCompile(`globalThis\s*[\.\[]\s*['"]?Function['"]?`),
		hint: "indirect reference to Function via globalThis is forbidden",
	},
	{
		name: "self-Function",
		re:   regexp.MustCompile(`(?:^|[^\w.$])self\s*[\.\[]\s*['"]?Function['"]?`),
		hint: "indirect reference to Function via self is forbidden",
	},
	{
		name: "window-Function",
		re:   regexp.MustCompile(`(?:^|[^\w.$])window\s*[\.\[]\s*['"]?Function['"]?`),
		hint: "indirect reference to Function via window is forbidden",
	},
	// Dangerous Node-style escape attempts (the runtime doesn't
	// expose these, but they shouldn't appear in approved code).
	{
		name: "process-binding",
		re:   regexp.MustCompile(`(?:^|[^\w.$])process\s*\.\s*binding\s*\(`),
		hint: "process.binding is a Node escape primitive and is forbidden",
	},
	{
		name: "require-call",
		re:   regexp.MustCompile(`(?:^|[^\w.$])require\s*\(`),
		hint: "require() is forbidden — use static imports only",
	},
	// `with` is deprecated but still parseable; it lets code escape
	// lexical scope and reach unintended bindings.
	{
		name: "with-statement",
		re:   regexp.MustCompile(`(?:^|[^\w.$])with\s*\(`),
		hint: "the `with` statement is forbidden — it bypasses lexical scoping",
	},
	// Base64 + execution cloak (atob('...') passed to eval/Function/etc.)
	// We don't need to detect every variant — eval and Function are
	// already blocked. atob alone is fine. We block the chained
	// pattern when a fold-up of those calls is statically visible.
	{
		name: "atob-in-eval-context",
		// atob(...) followed within ~80 chars by eval/Function call.
		// Heuristic; false-positives possible.
		re:   regexp.MustCompile(`atob\s*\([^)]+\)[^;]{0,80}(?:eval|Function)\s*\(`),
		hint: "decoding base64 then executing it is a sandbox escape attempt",
	},
}

// ValidateAST scans the bundle for forbidden constructs. Returns
// the first violation as an error or nil.
//
// The bundled output may legitimately contain the substring "eval"
// in other contexts (function names like "evaluate", string literals
// like "evaluate-after"). The patterns are anchored to require an
// open-paren and a non-identifier prefix, which catches the call
// site without flagging unrelated identifiers.
//
// Unicode-escape bypass defense: we run the patterns against both
// the raw input AND a unicode-escape-normalized variant, so
// `eval(...)` is caught even though textually it doesn't
// match `eval`.
func ValidateAST(js []byte) error {
	if len(js) == 0 {
		return errors.New("ValidateAST: empty input")
	}
	if err := scanForbiddenPatterns(js); err != nil {
		return err
	}
	normalized := normalizeUnicodeEscapes(js)
	if err := scanForbiddenPatterns(normalized); err != nil {
		return fmt.Errorf("%w (after unicode-escape normalization)", err)
	}
	return nil
}

func scanForbiddenPatterns(js []byte) error {
	for _, p := range forbiddenPatterns {
		if loc := p.re.FindIndex(js); loc != nil {
			line, col := lineColAt(js, loc[0])
			return fmt.Errorf("%s at line %d col %d — %s", p.name, line, col, p.hint)
		}
	}
	return nil
}

// normalizeUnicodeEscapes replaces \uXXXX escapes with the actual
// character. Used so identifier patterns catch `eval(` →
// `eval(`. Only handles BMP escapes (\uXXXX); surrogate pairs and
// \u{XXXXX} long-form would be added the same way.
func normalizeUnicodeEscapes(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if i+5 < len(b) && b[i] == '\\' && b[i+1] == 'u' &&
			isHex(b[i+2]) && isHex(b[i+3]) && isHex(b[i+4]) && isHex(b[i+5]) {
			r := rune(hexNibble(b[i+2]))<<12 |
				rune(hexNibble(b[i+3]))<<8 |
				rune(hexNibble(b[i+4]))<<4 |
				rune(hexNibble(b[i+5]))
			out = append(out, []byte(string(r))...)
			i += 6
			continue
		}
		out = append(out, b[i])
		i++
	}
	return out
}

func isHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

func hexNibble(b byte) byte {
	switch {
	case b >= '0' && b <= '9':
		return b - '0'
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10
	default:
		return b - 'A' + 10
	}
}

// lineColAt returns 1-indexed (line, column) for the byte offset.
func lineColAt(b []byte, off int) (int, int) {
	if off < 0 || off > len(b) {
		return 0, 0
	}
	line, col := 1, 1
	for i := 0; i < off; i++ {
		if b[i] == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}
