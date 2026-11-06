// Package distill holds the response-parsing logic shared by every runner's reply
// (the runner implementations themselves live in internal/platform/*).
package distill

import (
	"encoding/json"
	"errors"
	"strings"
)

// ErrNoJSONArray is returned by ParseJSONArray when a reply contains NO parseable JSON
// array at all — the prose_drift signal (the model conversed/refused instead of
// emitting the required array), as distinct from an explicit empty "[]" (a legit quiet
// result, returned as an empty slice). Exported + a sentinel so callers can classify it
// with errors.Is and surface actionable "the model may be too weak" guidance rather
// than a generic parse error. (mine() wraps this into its own errNoArray for the
// per-lens drift bookkeeping; the reviewer/preview paths match ErrNoJSONArray directly.)
var ErrNoJSONArray = errors.New("no JSON array found in reply")

// ErrTruncatedJSONArray is returned when the reply clearly BEGAN the required array but
// never closed it — an unterminated '[' whose text holds at least one plausible element.
//
// It must never be conflated with ErrNoJSONArray. Drift means "the model chose to converse
// instead of extracting", so the worker deliberately advances the watermark (a below-floor
// model would otherwise wedge the queue forever). Truncation is the opposite: the model DID
// extract, and the reply was cut off — by an output-token cap, a killed child, or a dropped
// stream. Treating that as drift advanced the watermark over turns whose observations were
// sitting right there in the truncated text, discarding them permanently: the watermark
// counts raw records, so those turns are never offered again.
//
// Reproduced before this existed: a reply containing two complete observation objects and a
// third cut mid-string returned ErrNoJSONArray, so the mine "succeeded" as drift and all
// three were lost. Retrying is safe and bounded — mine failures back off 5m/10m/20m… capped
// at 6h and NEVER drop raw turns, so a genuinely repeatable truncation just delays that
// session's delta instead of silently emptying it.
var ErrTruncatedJSONArray = errors.New("reply began a JSON array but never closed it (truncated output)")

// ParseJSONArray extracts the intended JSON array from a model reply. Real models
// wrap output in prose and/or a ```json fence, may emit a stray "[]" or
// "[the user]" in the prose BEFORE the real array, and may put an example array
// inside a leading object. So we search a series of candidate strings (each
// fenced block in document order, then the whole reply — see fencedBlocks for why
// the blocks are scanned on their own) and, within each:
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
// It takes the LAST qualifying array, not the first. Models routinely restate the
// prompt's own ```json example before answering ("The format you want is: … Here are the
// observations for THIS session: …"), and every shipped extract/review prompt contains
// such an example. First-wins therefore wrote the EXAMPLE into L1 as if the user had
// really done those things, and silently discarded the real answer — fabricated evidence
// in a personal growth archive, the worst failure mode this tool has. A model's genuine
// answer always FOLLOWS anything it echoes, so last-wins is the correct tiebreak; for a
// well-behaved single-array reply the two are identical.
//
// Last-wins alone cannot see an answer that never closed: a reply that echoes the example and is
// then cut off mid-array has the EXAMPLE as its last complete array. So a winner is returned only
// if no unclosed array opening an element follows it in the reply (truncatedArray on the rest);
// otherwise the reply is ErrTruncatedJSONArray. The check is skipped when the winner came only from
// a fenced block, whose offsets are not the reply's.
func ParseJSONArray[T any](reply string) ([]T, error) {
	candidates := append(fencedBlocks(reply), reply)
	whole := len(candidates) - 1 // the whole reply is always the last candidate
	sawEmpty := false
	// Tier 1: top-level arrays only (the strict, correct shape). Tier 2: any array at any depth
	// (object-wrapped result fallback), only if tier 1 found nothing.
	for _, topOnly := range []bool{true, false} {
		var best []T
		found, bestEnd := false, -1 // bestEnd: where the winner ends in the WHOLE reply, -1 if fenced only
		for ci, c := range candidates {
			for _, span := range arraySpans(c) {
				if topOnly && !span.topLevel {
					continue
				}
				if arr, ok, empty := decodeArray[T](span.text); ok {
					if !empty {
						best, found, bestEnd = arr, true, -1 // keep scanning: later wins
						if ci == whole {
							bestEnd = span.start + len(span.text)
						}
					} else if !markdownCheckbox(c, span.start) {
						sawEmpty = true
					}
				}
			}
		}
		if found {
			if bestEnd >= 0 && truncatedArray(reply[bestEnd:]) {
				return nil, ErrTruncatedJSONArray
			}
			return best, nil
		}
	}
	// No usable array. Before treating this as either a quiet session or drift — BOTH of which
	// advance the watermark — check whether the reply was cut off mid-array. That is a retryable
	// output failure, and the observations it was in the middle of listing must not be thrown away.
	//
	// This check must come BEFORE the sawEmpty return, not after it. Below it, sawEmpty shadowed
	// truncation for the most likely shape in the field: every shipped extract prompt prints a
	// literal `[]` while instructing the model to return one for a quiet session, and models
	// routinely restate that instruction before answering (the same echoing the LAST-wins tiebreak
	// defends against). A reply that echoes `[]` in prose and is then cut off mid-array set
	// sawEmpty, returned "quiet session", and the watermark advanced past turns whose observations
	// were sitting in the truncated text — permanent loss, through the one path the truncation fix
	// did not cover. A balanced `[]` is weak evidence of intent; an unclosed array mid-element is
	// strong evidence of a cut-off reply, so truncation outranks it.
	if truncatedArray(reply) {
		return nil, ErrTruncatedJSONArray
	}
	if sawEmpty {
		return []T{}, nil
	}
	return nil, ErrNoJSONArray
}

// truncatedArray reports whether s contains an UNCLOSED '[' that plausibly held array
// elements — i.e. the reply began the required array and was cut off.
//
// Deliberately conservative, because a false positive turns a genuinely drifting model into
// an endlessly backing-off session. Two guards:
//
//   - the '[' must be unbalanced (matchBracket finds no closing ']'), so any complete array
//     anywhere in the reply is unaffected;
//   - its next non-space character must be '{' or '"' — an element in progress. This is what
//     keeps prose out: a markdown checkbox ("- [ ] todo"), an unmatched bracket in a sentence,
//     or `[the "x" message` has no such element and stays ErrNoJSONArray, i.e. drift. (It was
//     "a '{' or '"' anywhere after it", which read `[the "x" message` as truncation; after a
//     complete array — see ParseJSONArray — that would stall review on trailing prose.) A '['
//     that fails this guard is skipped, not decisive, so a real cut-off array later still counts.
//
// String literals are respected via the same scan arraySpans uses, so a '[' inside a quoted
// string is not mistaken for the start of an array.
func truncatedArray(s string) bool {
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
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
			if end := matchBracket(s, i); end > i {
				i = end // balanced: skip it whole, it is not the truncation
				continue
			}
			// Unclosed. Only truncation if an element opens right after it.
			if rest := strings.TrimLeft(s[i+1:], " \t\r\n"); rest != "" && (rest[0] == '{' || rest[0] == '"') {
				return true
			}
		}
	}
	return false
}

func decodeArray[T any](span string) (arr []T, ok bool, empty bool) {
	if err := json.Unmarshal([]byte(span), &arr); err != nil {
		return nil, false, false
	}
	return arr, true, len(arr) == 0
}

// arraySpan is a balanced [...] found in a string, flagged topLevel if its '['
// sits outside any enclosing object. start is the byte index of that '[' in the
// scanned string, so callers can inspect the surrounding text (see markdownCheckbox).
type arraySpan struct {
	text     string
	topLevel bool
	start    int
}

// markdownCheckbox reports whether the '[' at index start begins a markdown task-list
// checkbox ("- [ ] item") rather than a JSON array.
//
// It matters because "[ ]" is valid JSON for an empty array. A model that drifts into a
// prose checklist ("- [ ] nothing to report") therefore parsed as an EXPLICIT empty array,
// i.e. "the lens looked and found nothing" — so the reply was recorded as a genuinely quiet
// session and the #57 drift signal was lost. The data outcome is the same either way (both
// advance the watermark), but the user never learns their model is below the lens's floor,
// which is the whole reason that signal exists.
//
// The test is deliberately narrow: the '[' must be at the start of its line apart from
// whitespace and a single list bullet (-, *, + or "1.").
func markdownCheckbox(s string, start int) bool {
	lineStart := strings.LastIndexByte(s[:start], '\n') + 1
	prefix := strings.TrimSpace(s[lineStart:start])
	switch prefix {
	case "-", "*", "+":
		return true
	}
	// An ordered-list marker: digits followed by '.' or ')'.
	if n := len(prefix); n >= 2 && (prefix[n-1] == '.' || prefix[n-1] == ')') {
		return strings.IndexFunc(prefix[:n-1], func(r rune) bool { return r < '0' || r > '9' }) < 0
	}
	return false
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
				spans = append(spans, arraySpan{text: s[i : end+1], topLevel: curly == 0, start: i})
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

// fencedBlocks returns the contents of every ```...``` code block in s, in document order, whatever
// the tag. It is load-bearing, not a redundant first try: one stray straight quote in a preamble
// (`27" monitor`) flips string parity for the whole-reply scan, hiding every array after it, and
// only a per-block scan still finds the answer. Document order is what lets ParseJSONArray's
// last-wins hold across blocks. Tagged json used to be ordered FIRST, which under last-wins made it
// LOSE to an earlier ```text example; ordering it last instead would return an echoed ```json
// example over an untagged answer (#133).
func fencedBlocks(s string) []string {
	var blocks []string
	for {
		start := strings.Index(s, "```")
		if start < 0 {
			break
		}
		rest := s[start+3:]
		nl := strings.IndexByte(rest, '\n') // skip the tag line
		if nl < 0 {
			break
		}
		body := rest[nl+1:]
		end := strings.Index(body, "```")
		if end < 0 {
			break
		}
		blocks = append(blocks, body[:end])
		s = body[end+3:]
	}
	return blocks
}
