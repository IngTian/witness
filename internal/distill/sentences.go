package distill

import (
	"strings"
	"unicode"
)

// SplitSentences breaks a markdown portrait into individual sentences.
// It is deterministic: the same markdown always produces the same slice, and
// index i is sentence number i+1 in resolver input. The resolver links
// portrait sentences to their supporting traits, so this function IS the
// single definition of "a sentence" that every reader shares.
//
// Headings are skipped. Emphasis markers (**) are stripped. List item markers
// (-, *, 1.) are removed. Sentences break after [.!?] (optionally followed by
// a closing quote or paren), then whitespace, then an uppercase letter, digit,
// quote, or open paren — UNLESS the period is part of a known abbreviation
// (e.g., i.e., vs., etc.) or sits between digits (97.7%). Chinese punctuation
// (。！？) also ends a sentence. Only sentences with len(trim(s)) > 1 are
// returned, and all returned sentences are trimmed.
func SplitSentences(md string) []string {
	// Strip headings, list markers, then join into a single block.
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var kept []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip headings.
		if strings.HasPrefix(line, "#") {
			continue
		}
		// Strip list markers at the start: "- ", "* ", or digit+". ".
		line = stripListMarker(line)
		if line != "" {
			kept = append(kept, line)
		}
	}
	text := strings.Join(kept, " ")

	// Strip emphasis markers.
	text = strings.ReplaceAll(text, "**", "")

	// Split into sentences. Walk the text looking for sentence-ending punctuation.
	var sentences []string
	start := 0
	runes := []rune(text)

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		// Check if this is sentence-ending punctuation.
		if !isSentenceEnd(r) {
			continue
		}

		// Chinese punctuation splits immediately (no whitespace required).
		if r == '。' || r == '！' || r == '？' {
			sentences = append(sentences, string(runes[start:i+1]))
			start = i + 1
			continue
		}

		// English punctuation: check for optional closing quote or paren.
		j := i + 1
		for j < len(runes) && (runes[j] == '"' || runes[j] == ')') {
			j++
		}

		// Now j is the position after any closing punctuation.
		if j >= len(runes) {
			// End of text: this is the last sentence.
			sentences = append(sentences, string(runes[start:]))
			start = len(runes) // Mark that we've consumed everything.
			break
		}

		// For English punctuation, require whitespace before the next sentence.
		if !unicode.IsSpace(runes[j]) {
			continue
		}

		// Skip whitespace to find the next non-space character.
		k := j
		for k < len(runes) && unicode.IsSpace(runes[k]) {
			k++
		}
		if k >= len(runes) {
			// Only whitespace left: this is the last sentence.
			sentences = append(sentences, string(runes[start:]))
			start = len(runes) // Mark that we've consumed everything.
			break
		}

		nextChar := runes[k]
		// Check if next char starts a new sentence: uppercase, digit, quote, or '('.
		if !(unicode.IsUpper(nextChar) || unicode.IsDigit(nextChar) || nextChar == '"' || nextChar == '(' || nextChar == '\'') {
			continue
		}

		// Before accepting this as a sentence boundary, check for abbreviations
		// and decimal numbers.
		if r == '.' {
			// Check if this period is part of an abbreviation.
			if isAbbreviation(runes, i) {
				continue
			}
			// Check if this period is between digits (decimal number).
			if isBetweenDigits(runes, i) {
				continue
			}
		}

		// This is a sentence boundary. Extract the sentence.
		sentences = append(sentences, string(runes[start:j]))
		start = k
		i = k - 1 // Will be incremented by the loop.
	}

	// Handle any remaining text as the last sentence.
	if start < len(runes) {
		sentences = append(sentences, string(runes[start:]))
	}

	// Trim and filter: only keep sentences with substantial content.
	// A sentence must have more than one character before its ending punctuation
	// (so "A." is filtered, but "One." is kept). Return an empty slice (not nil)
	// if there are no sentences.
	result := []string{}
	for _, s := range sentences {
		s = strings.TrimSpace(s)
		// Check content length: remove trailing punctuation and see what remains.
		rs := []rune(s)
		if len(rs) > 0 && isSentenceEnd(rs[len(rs)-1]) {
			rs = rs[:len(rs)-1]
		}
		// Keep only if the content (before punctuation) has length > 1.
		if len([]rune(strings.TrimSpace(string(rs)))) > 1 {
			result = append(result, s)
		}
	}

	return result
}

// stripListMarker removes list item markers from the start of a line.
// Handles: "- ", "* ", and digit+". " (e.g., "1. ").
func stripListMarker(line string) string {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "- ") {
		return strings.TrimSpace(line[2:])
	}
	if strings.HasPrefix(line, "* ") {
		return strings.TrimSpace(line[2:])
	}
	// Check for numbered list: digit(s) followed by ". ".
	if len(line) > 0 && unicode.IsDigit(rune(line[0])) {
		for i := 1; i < len(line); i++ {
			if line[i] == '.' && i+1 < len(line) && line[i+1] == ' ' {
				return strings.TrimSpace(line[i+2:])
			}
			if !unicode.IsDigit(rune(line[i])) {
				break
			}
		}
	}
	return line
}

// isSentenceEnd reports whether r is a sentence-ending character.
// English: . ! ?
// Chinese: 。！？
func isSentenceEnd(r rune) bool {
	return r == '.' || r == '!' || r == '?' ||
		r == '。' || r == '！' || r == '？'
}

// isAbbreviation checks if the period at position i is part of a known
// abbreviation (e.g., i.e., vs., etc.). Returns true if it should NOT
// trigger a sentence break.
func isAbbreviation(runes []rune, i int) bool {
	// Look backward to find the start of the word containing this period.
	// Common abbreviations: "e.g.", "i.e.", "vs.", "etc."
	// We check if the period is preceded by these patterns.

	// Extract a small window around the period for matching.
	start := i - 10
	if start < 0 {
		start = 0
	}
	window := string(runes[start : i+1])

	// Check for common abbreviations ending at this position.
	abbrevs := []string{"e.g.", "i.e.", "vs.", "etc."}
	for _, abbrev := range abbrevs {
		if strings.HasSuffix(window, abbrev) {
			return true
		}
	}

	return false
}

// isBetweenDigits checks if the period at position i sits between two digits
// (e.g., "97.7"). Returns true if it should NOT trigger a sentence break.
func isBetweenDigits(runes []rune, i int) bool {
	// Check if there's a digit immediately before and after the period.
	if i > 0 && i+1 < len(runes) {
		return unicode.IsDigit(runes[i-1]) && unicode.IsDigit(runes[i+1])
	}
	return false
}
