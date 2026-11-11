package distill

import (
	"strings"
	"testing"
)

// #199 G001: the claim and the two compaction prompts are shared by every lens, and the claim pass
// now runs before every fold, so a market or research lens gets them with nobody asking. Their text
// must name the lens's subject rather than assume a person. This pins the PHRASES that assumed one,
// not the bare words: "usually a person" and "the third person" are deliberate and stay.
func TestCompactionAndClaimPromptsNameTheSubject(t *testing.T) {
	// Compared with whitespace collapsed, so a phrase cannot hide behind a line wrap.
	flat := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	prompts := map[string]string{"claim": flat(claimPrompt), "group": flat(compactGroupPrompt), "write": flat(compactWritePrompt)}
	for name, p := range prompts {
		for _, phrase := range []string{
			"behavioural profile", "about one person", "about this person", "what the person does",
			"with the person", "person's own words", "thing about the person", "work sessions",
			"every session wrote", "how the session was recorded", "true of anyone", "separate session",
			"personality description", "his own state",
		} {
			if strings.Contains(p, phrase) {
				t.Errorf("%s prompt still assumes a person: %q", name, phrase)
			}
		}
	}
	for _, name := range []string{"claim", "group"} {
		if !strings.Contains(prompts[name], "a lens tracks one subject") {
			t.Errorf("%s prompt must say what a lens's subject is", name)
		}
	}
}
