// Package sentence is the one definition of "a sentence" in a portrait. The resolver numbers a
// portrait's sentences with it and persists the numbers in portrait_sources; the read-only view
// numbers the same text with it to show which traits each sentence rests on. Any drift between
// two splitters moves every stored link onto the wrong sentence, so both walk the blocks below.
// Standard library only: the view must never pull the distill engine in (#195: never the read
// path).
package sentence

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
	all := []string{} // empty, not nil, when there are no sentences
	for _, b := range PortraitBlocks(md) {
		all = append(all, BlockSentences(b)...)
	}
	return all
}

// Block is one markdown block as the splitter reads it. Kind is "h" (a heading: Text is the
// trimmed line, # and all), "p" (a paragraph: its trimmed lines joined by one space) or "li"
// (a list item: the text after its marker). Text keeps the ** markers, so a reader can still
// show the bold the sentences themselves drop.
type Block struct {
	Kind, Text string
}

// PortraitBlocks is the block pass of SplitSentences: blank lines, headings and list items are
// block boundaries, and each list item is a block of its own. It is exported so the view walks
// exactly these blocks and numbers sentences exactly as the resolver did.
func PortraitBlocks(md string) []Block {
	var blocks []Block
	var para []string
	flush := func() {
		if len(para) > 0 {
			blocks = append(blocks, Block{Kind: "p", Text: strings.Join(para, " ")})
			para = nil
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			flush()
			continue
		}
		if strings.HasPrefix(t, "#") {
			flush()
			blocks = append(blocks, Block{Kind: "h", Text: t})
			continue
		}
		if item, ok := listItem(t); ok {
			flush()
			if item != "" {
				blocks = append(blocks, Block{Kind: "li", Text: item})
			}
			continue
		}
		para = append(para, t)
	}
	flush()
	return blocks
}

// BlockSentences is one block's sentences, numbered in order after the blocks before it.
// Headings carry none; elsewhere the ** markers are dropped before splitting.
func BlockSentences(b Block) []string {
	if b.Kind == "h" {
		return nil
	}
	return splitBlockIntoSentences(strings.ReplaceAll(b.Text, "**", ""))
}

// listItem reports whether a trimmed line starts with a list marker ("- ", "* ", or digits
// then ". ") and returns the text after it, trimmed.
func listItem(line string) (string, bool) {
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
		return strings.TrimSpace(line[2:]), true
	}
	if len(line) > 0 && unicode.IsDigit(rune(line[0])) {
		for i := 1; i < len(line); i++ {
			if line[i] == '.' && i+1 < len(line) && line[i+1] == ' ' {
				return strings.TrimSpace(line[i+2:]), true
			}
			if !unicode.IsDigit(rune(line[i])) {
				break
			}
		}
	}
	return line, false
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
