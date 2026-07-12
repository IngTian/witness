// Package distill holds the response-parsing logic shared by every runner's reply
// (the runner implementations themselves live in internal/platform/*).
package distill

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ParseJSONArray extracts the intended JSON array from a model reply. Real models
// wrap output in prose and/or a ```json fence, may emit a stray "[]" or
// "[the user]" in the prose BEFORE the real array, and may put an example array
// inside a leading object. So we search a series of candidate strings (each
// fenced block — ```json first — then the whole reply) and, within each:
//
//  1. prefer a TOP-LEVEL, balanced, NON-EMPTY array. This skips an empty "[]"
//     (which, if returned, would silently drop the session's observations and
//     advance the watermark — permanent loss) and an array nested inside an
//     earlier object (a "schema example"). [fixes the empty-array + nested-array
//     data-loss bugs]
//  2. only if no top-level array exists anywhere, fall back to the first non-empty
//     array at ANY depth, so an object-wrapped result ({"observations":[...]})
//     still parses rather than being mistaken for a quiet session.
//
// A reply whose only array is an empty "[]" yields an empty slice (a genuinely
// quiet session); a reply with no array at all is an error. The worker treats
// both as "nothing to mine", but the distinction keeps the intent explicit.
func ParseJSONArray[T any](reply string) ([]T, error) {
	candidates := append(fencedBlocks(reply), reply)
	sawEmpty := false
	// Tier 1: top-level arrays only (the strict, correct shape).
	for _, c := range candidates {
		for _, span := range arraySpans(c) {
			if !span.topLevel {
				continue
			}
			if arr, ok, empty := decodeArray[T](span.text); ok {
				if !empty {
					return arr, nil
				}
				sawEmpty = true
			}
		}
	}
	// Tier 2: any array at any depth (object-wrapped result fallback).
	for _, c := range candidates {
		for _, span := range arraySpans(c) {
			if arr, ok, empty := decodeArray[T](span.text); ok {
				if !empty {
					return arr, nil
				}
				sawEmpty = true
			}
		}
	}
	if sawEmpty {
		return []T{}, nil
	}
	return nil, fmt.Errorf("no JSON array found in reply")
}

func decodeArray[T any](span string) (arr []T, ok bool, empty bool) {
	if err := json.Unmarshal([]byte(span), &arr); err != nil {
		return nil, false, false
	}
	return arr, true, len(arr) == 0
}

// arraySpan is a balanced [...] found in a string, flagged topLevel if its '['
// sits outside any enclosing object.
type arraySpan struct {
	text     string
	topLevel bool
}

// arraySpans returns every balanced [...] in s (respecting JSON string literals
// so a '[' inside a string isn't mistaken for an array). Each is flagged topLevel
// when its opening '[' is not inside an enclosing {...}. Arrays nested inside
// other arrays are not returned separately — the outer array is what callers want
// — but arrays directly inside an object ARE returned (flagged not-topLevel) so
// the tier-2 fallback can find an object-wrapped result.
func arraySpans(s string) []arraySpan {
	var spans []arraySpan
	inStr, esc := false, false
	curly := 0
	for i := 0; i < len(s); {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			i++
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			curly++
		case '}':
			if curly > 0 {
				curly--
			}
		case '[':
			if end := matchBracket(s, i); end > i {
				spans = append(spans, arraySpan{text: s[i : end+1], topLevel: curly == 0})
				i = end + 1 // jump past; the array is balanced so curly stays consistent
				continue
			}
		}
		i++
	}
	return spans
}

// matchBracket returns the index of the ']' that closes the '[' at start
// (respecting string literals and nested brackets), or -1 if unbalanced.
func matchBracket(s string, start int) int {
	depth, inStr, esc := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// fencedBlocks returns the contents of every ```...``` code block in s, with
// blocks tagged ```json (or ```jsonc) ordered first so the intended JSON wins
// over an incidental ```sh/```text block elsewhere in the reply.
func fencedBlocks(s string) []string {
	var jsonBlocks, other []string
	for {
		start := strings.Index(s, "```")
		if start < 0 {
			break
		}
		rest := s[start+3:]
		nl := strings.IndexByte(rest, '\n')
		if nl < 0 {
			break
		}
		tag := strings.ToLower(strings.TrimSpace(rest[:nl]))
		body := rest[nl+1:]
		end := strings.Index(body, "```")
		if end < 0 {
			break
		}
		block := body[:end]
		if tag == "json" || tag == "jsonc" {
			jsonBlocks = append(jsonBlocks, block)
		} else {
			other = append(other, block)
		}
		s = body[end+3:]
	}
	return append(jsonBlocks, other...)
}
