package distill

import (
	"reflect"
	"testing"
)

// SplitSentences breaks a portrait into numbered sentences for resolver input.
// It must be deterministic: the same markdown always gives the same slice, and
// index i is sentence number i+1. The resolver links portrait sentences to
// traits, so this function IS the single definition of "a sentence" every
// reader shares. Test cases verify: headings are skipped, emphasis markers are
// stripped, list items are split, abbreviations (e.g., i.e., vs., etc.) don't
// trigger breaks, decimals don't split, and Chinese punctuation works.
func TestSplitSentences(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want []string
	}{
		{
			"headings skipped",
			"# Title\n\nOne. Two.",
			[]string{"One.", "Two."},
		},
		{
			"bold lead sentence",
			"**Lead.** Rest here. More.",
			[]string{"Lead.", "Rest here.", "More."},
		},
		{
			"bullet list items",
			"- First item. Still first.\n- Second.",
			[]string{"First item.", "Still first.", "Second."},
		},
		{
			"decimal number no split",
			"Accuracy is 97.7% overall. Next.",
			[]string{"Accuracy is 97.7% overall.", "Next."},
		},
		{
			"abbreviations no split",
			"Use tools, e.g. hooks. Then stop.",
			[]string{"Use tools, e.g. hooks.", "Then stop."},
		},
		{
			"quoted sentence end",
			"He said \"stop.\" Then left.",
			[]string{"He said \"stop.\"", "Then left."},
		},
		{
			"chinese punctuation",
			"第一句。第二句！第三句？",
			[]string{"第一句。", "第二句！", "第三句？"},
		},
		{
			"empty input",
			"",
			[]string{},
		},
		{
			"whitespace only",
			"  \n\t  ",
			[]string{},
		},
		{
			"single char sentence filtered",
			"A. B.",
			[]string{}, // Both are ≤1 char after trim
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SplitSentences(tc.md)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("SplitSentences(%q)\n  got: %#v\n want: %#v", tc.md, got, tc.want)
			}
		})
	}
}
