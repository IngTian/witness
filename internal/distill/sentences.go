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
// (-, *, 1.) are removed. Block boundaries (blank line, list item, heading) are
// sentence boundaries. Sentences break after [.!?] (optionally followed by
// a closing quote/paren/curly quote), then whitespace, then an uppercase letter,
// digit, quote, open paren, Han character, or opening curly quote — UNLESS the
// period is part of a known abbreviation (e.g., i.e., vs., etc.) or sits between
// digits (97.7%). Chinese punctuation (。！？) also ends a sentence. Only sentences
// with len(trim(s)) > 1 are returned, and all returned sentences are trimmed.
func SplitSentences(md string) []string {
	// Split into blocks: blank lines, headings, and list items are block boundaries.
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var blocks []string
	var currentBlock []string

	flushBlock := func() {
		if len(currentBlock) > 0 {
			text := strings.Join(currentBlock, " ")
			if text != "" {
				blocks = append(blocks, text)
			}
			currentBlock = nil
		}
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Blank line or heading: flush current block.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			flushBlock()
			continue
		}
		// List item: flush current block, start new block with this item.
		if isListItem(trimmed) {
			flushBlock()
			line = stripListMarker(trimmed)
			if line != "" {
				currentBlock = []string{line}
				flushBlock() // list item is its own block
			}
			continue
		}
		// Regular line: add to current block.
		currentBlock = append(currentBlock, trimmed)
	}
	flushBlock()

	// Strip emphasis markers from each block.
	for i, block := range blocks {
		blocks[i] = strings.ReplaceAll(block, "**", "")
	}

	// Split each block into sentences.
	allSentences := []string{} // Return empty slice (not nil) if there are no sentences
	for _, block := range blocks {
		sentences := splitBlockIntoSentences(block)
		allSentences = append(allSentences, sentences...)
	}
	return allSentences
}

// isListItem checks if a line starts with a list marker.
func isListItem(line string) bool {
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
		return true
	}
	// Check for numbered list: digit(s) followed by ". ".
	if len(line) > 0 && unicode.IsDigit(rune(line[0])) {
		for i := 1; i < len(line); i++ {
			if line[i] == '.' && i+1 < len(line) && line[i+1] == ' ' {
				return true
			}
			if !unicode.IsDigit(rune(line[i])) {
				break
			}
		}
	}
	return false
}

// splitBlockIntoSentences splits a block of text into sentences using punctuation rules.
func splitBlockIntoSentences(text string) []string {
	// Walk the text looking for sentence-ending punctuation.
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

		// English punctuation: check for optional closing quote, paren, or curly quotes.
		j := i + 1
		for j < len(runes) && isClosingPunct(runes[j]) {
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
		// Check if next char starts a new sentence: uppercase, digit, quote, '(', Han, or opening curly quote.
		if !isValidSentenceStart(nextChar) {
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

// isClosingPunct checks if r is a closing punctuation mark (quote, paren, curly quotes).
func isClosingPunct(r rune) bool {
	return r == '"' || r == ')' ||
		r == 0x201D || // right double curly quote (U+201D)
		r == 0x201C || // left double curly quote (U+201C)
		r == 0x2019 || // right single curly quote (U+2019)
		r == 0x2018 // left single curly quote (U+2018)
}

// isValidSentenceStart checks if r can start a sentence.
// Valid starts: uppercase, digit, quote, '(', Han character, or opening curly quote.
func isValidSentenceStart(r rune) bool {
	return unicode.IsUpper(r) ||
		unicode.IsDigit(r) ||
		r == '"' || r == '(' || r == '\'' ||
		unicode.Is(unicode.Han, r) ||
		r == 0x201C || // left double curly quote (U+201C)
		r == 0x201D || // right double curly quote (U+201D) - also valid start
		r == 0x2018 || // left single curly quote (U+2018)
		r == 0x2019 // right single curly quote (U+2019) - also valid start
}
