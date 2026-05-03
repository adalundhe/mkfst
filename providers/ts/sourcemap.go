package ts

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// === source-map decoding ===
//
// esbuild emits standard v3 source maps. For server-side error
// translation we only need a tiny slice of the spec: parse the
// VLQ-encoded mappings into a list of (genLine, genCol → srcFile,
// srcLine, srcCol) entries, then look up an entry near a given
// (genLine, genCol) to surface the original TS position.
//
// We don't implement the full Mozilla source-map JS library — just
// enough to translate "<bundle>:42:7" mentions in stack traces back
// to "smoketest.workflow.ts:8:14".

// SourceMap is a minimal parsed form.
type SourceMap struct {
	Version  int      `json:"version"`
	Sources  []string `json:"sources"`
	Names    []string `json:"names"`
	Mappings string   `json:"mappings"`

	// Decoded entries (lazy).
	decoded [][]mappingEntry // [genLine][i] sorted by genCol
}

// mappingEntry is one position mapping.
type mappingEntry struct {
	GenCol  int
	SrcFile int
	SrcLine int
	SrcCol  int
	NameIdx int // -1 if absent
}

// Position is the resolved original location.
type Position struct {
	Source string
	Line   int // 1-indexed
	Column int // 1-indexed
	Name   string
}

// ParseInlineSourceMap extracts and parses the inline `// #
// sourceMappingURL=data:application/json;base64,...` line from
// esbuild bundle output. Returns nil if the bundle doesn't have an
// inline map.
func ParseInlineSourceMap(bundleJS []byte) (*SourceMap, error) {
	prefix := []byte("//# sourceMappingURL=data:application/json;base64,")
	idx := bytes.LastIndex(bundleJS, prefix)
	if idx < 0 {
		return nil, nil
	}
	end := bytes.IndexByte(bundleJS[idx:], '\n')
	var b64 []byte
	if end < 0 {
		b64 = bundleJS[idx+len(prefix):]
	} else {
		b64 = bundleJS[idx+len(prefix) : idx+end]
	}
	jsonBytes, err := base64.StdEncoding.DecodeString(string(b64))
	if err != nil {
		return nil, fmt.Errorf("ParseInlineSourceMap: base64: %w", err)
	}
	var sm SourceMap
	if err := json.Unmarshal(jsonBytes, &sm); err != nil {
		return nil, fmt.Errorf("ParseInlineSourceMap: json: %w", err)
	}
	if sm.Version != 3 {
		return nil, fmt.Errorf("ParseInlineSourceMap: unsupported version %d", sm.Version)
	}
	if err := sm.decode(); err != nil {
		return nil, err
	}
	return &sm, nil
}

// decode parses the VLQ mappings.
func (sm *SourceMap) decode() error {
	lines := strings.Split(sm.Mappings, ";")
	sm.decoded = make([][]mappingEntry, len(lines))
	srcIdx, srcLine, srcCol, nameIdx := 0, 0, 0, 0
	for li, line := range lines {
		if line == "" {
			continue
		}
		genCol := 0
		segs := strings.Split(line, ",")
		entries := make([]mappingEntry, 0, len(segs))
		for _, seg := range segs {
			if seg == "" {
				continue
			}
			vals, err := decodeVLQs(seg)
			if err != nil {
				return fmt.Errorf("line %d: %w", li, err)
			}
			if len(vals) == 0 {
				continue
			}
			genCol += vals[0]
			e := mappingEntry{GenCol: genCol, NameIdx: -1}
			if len(vals) >= 4 {
				srcIdx += vals[1]
				srcLine += vals[2]
				srcCol += vals[3]
				e.SrcFile = srcIdx
				e.SrcLine = srcLine
				e.SrcCol = srcCol
			} else {
				e.SrcFile = -1
			}
			if len(vals) >= 5 {
				nameIdx += vals[4]
				e.NameIdx = nameIdx
			}
			entries = append(entries, e)
		}
		sm.decoded[li] = entries
	}
	return nil
}

// Lookup returns the original Position for a (genLine, genCol),
// 1-indexed inputs. Falls back to the nearest preceding entry on
// the same line when there's no exact match.
func (sm *SourceMap) Lookup(genLine, genCol int) (Position, bool) {
	if sm == nil || genLine < 1 || genLine-1 >= len(sm.decoded) {
		return Position{}, false
	}
	entries := sm.decoded[genLine-1]
	if len(entries) == 0 {
		return Position{}, false
	}
	target := genCol - 1
	best := entries[0]
	for _, e := range entries {
		if e.GenCol > target {
			break
		}
		best = e
	}
	if best.SrcFile < 0 || best.SrcFile >= len(sm.Sources) {
		return Position{}, false
	}
	pos := Position{
		Source: sm.Sources[best.SrcFile],
		Line:   best.SrcLine + 1,
		Column: best.SrcCol + 1,
	}
	if best.NameIdx >= 0 && best.NameIdx < len(sm.Names) {
		pos.Name = sm.Names[best.NameIdx]
	}
	return pos, true
}

// === error rewriting ===

// stackLineRE matches the "filename:line:col" portion of a
// QuickJS-style stack frame.
var stackLineRE = regexp.MustCompile(`([^\s:()]+):(\d+):(\d+)`)

// RewriteStack scans an error message and rewrites every
// "<filename>:<line>:<col>" reference using the source map. Lines
// that don't correspond to known source positions are passed
// through unchanged.
func (sm *SourceMap) RewriteStack(errMsg string) string {
	if sm == nil {
		return errMsg
	}
	return stackLineRE.ReplaceAllStringFunc(errMsg, func(match string) string {
		parts := stackLineRE.FindStringSubmatch(match)
		if len(parts) != 4 {
			return match
		}
		genLine, _ := strconv.Atoi(parts[2])
		genCol, _ := strconv.Atoi(parts[3])
		pos, ok := sm.Lookup(genLine, genCol)
		if !ok {
			return match
		}
		return fmt.Sprintf("%s:%d:%d", pos.Source, pos.Line, pos.Column)
	})
}

// === VLQ decoder ===
//
// base64 VLQ encodes signed integers in groups of 5-bit chunks.

const b64Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

var b64Lookup [128]int8

func init() {
	for i := range b64Lookup {
		b64Lookup[i] = -1
	}
	for i, c := range b64Chars {
		b64Lookup[c] = int8(i)
	}
}

func decodeVLQs(s string) ([]int, error) {
	out := []int{}
	val := 0
	shift := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 128 {
			return nil, fmt.Errorf("VLQ: non-ASCII")
		}
		digit := b64Lookup[c]
		if digit < 0 {
			return nil, fmt.Errorf("VLQ: bad char %q", c)
		}
		cont := digit & 0x20
		val |= int(digit&0x1F) << shift
		if cont == 0 {
			signed := val >> 1
			if val&1 == 1 {
				signed = -signed
			}
			out = append(out, signed)
			val = 0
			shift = 0
		} else {
			shift += 5
		}
	}
	return out, nil
}
