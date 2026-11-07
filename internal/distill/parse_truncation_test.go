package distill

import (
	"errors"
	"testing"
)

type tobs struct {
	Observation string `json:"observation"`
}

// A reply cut off mid-array is TRUNCATION, not prose drift.
//
// The distinction decides whether observations survive. Drift means "the model conversed
// instead of extracting", so the worker deliberately advances the watermark (a below-floor
// model would otherwise wedge the queue). Truncation means the model DID extract and the
// output was cut off — by a token cap, a killed child, a dropped stream. Classifying that as
// drift advanced the watermark over turns whose observations were sitting in the truncated
// text, and since the watermark counts raw records those turns are never offered again.
func TestParseJSONArrayReportsTruncationSeparatelyFromDrift(t *testing.T) {
	truncated := []struct{ name, reply string }{
		{"cut mid-string", `Here are the observations:
[
  {"observation": "first real finding"},
  {"observation": "second real finding"},
  {"observation": "third, cut off mid`},
		{"cut after a complete element", `[{"observation":"a"},{"observation":"b"},`},
		{"cut right after the bracket", `[{"observation":`},
		{"inside a fence", "```json\n[\n  {\"observation\": \"a\"},\n  {\"observ"},
		// An unclosed prose '[' that opens no element comes FIRST. It must be skipped, not
		// decisive: if it ended the scan this reply would read as drift and lose its observations.
		{"unclosed prose bracket before the cut-off array",
			"I looked at [the user's session first. Here:\n" + `[{"observation":"a"},{"observation":"cut`},
	}
	for _, tc := range truncated {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseJSONArray[tobs](tc.reply)
			if !errors.Is(err, ErrTruncatedJSONArray) {
				t.Errorf("want ErrTruncatedJSONArray (retry, keep the delta), got %v — "+
					"as drift this advances the watermark and the observations are lost", err)
			}
			if errors.Is(err, ErrNoJSONArray) {
				t.Error("truncation must not also satisfy ErrNoJSONArray: the worker keys drift on it")
			}
		})
	}
}

// Truncation must OUTRANK an echoed empty array — the field-realistic shape.
//
// The truncation check originally sat below the sawEmpty early return, so it was unreachable
// whenever the reply contained a balanced `[]` anywhere. That is not a corner case: every shipped
// extract prompt prints a literal `[]` while telling the model to return one for a quiet session
// (prompts/default/extract.md has two), and models routinely restate the instruction before
// answering — the same echoing behaviour the LAST-wins tiebreak was added to defend against. So
// the realistic truncated reply mentions `[]` in prose, then starts the real array and gets cut
// off. That returned ([], nil): a QUIET SESSION. The worker then advanced the watermark past turns
// whose observations were sitting in the truncated text, and since the watermark counts raw
// records they are never offered again — the exact permanent loss ErrTruncatedJSONArray exists to
// prevent, reached through the one path the fix did not cover.
func TestParseJSONArrayPrefersTruncationOverAnEchoedEmptyArray(t *testing.T) {
	for _, tc := range []struct{ name, reply string }{
		{"echoed instruction then a cut-off array",
			"If the session is quiet I should return []. Here are the observations:\n" +
				`[{"observation":"a real finding"},{"observation":"another one",`},
		{"echoed empty array inside a fence, real array truncated after",
			"```json\n[]\n```\nActually, here is what I found:\n" +
				`[{"observation":"a"},{"observation":"b`},
		{"prose mentions [] twice, as the prompt does",
			"Return [] when nothing happened. Otherwise a list like []. My findings:\n" +
				`[{"observation":"x"},{"obser`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseJSONArray[tobs](tc.reply)
			if !errors.Is(err, ErrTruncatedJSONArray) {
				t.Errorf("want ErrTruncatedJSONArray, got (%v, %v) — an echoed `[]` made a TRUNCATED "+
					"reply look like a quiet session, so the watermark advances and the "+
					"observations in the cut-off text are lost permanently", got, err)
			}
		})
	}
}

// A genuinely quiet session must still be quiet: an empty array and NO truncation.
// This is the half that prefering-truncation could break.
func TestParseJSONArrayStillReportsAQuietSessionAsEmpty(t *testing.T) {
	for _, tc := range []struct{ name, reply string }{
		{"bare empty array", `[]`},
		{"empty array in a fence", "```json\n[]\n```"},
		{"prose then an empty array", "Nothing noteworthy happened here.\n[]"},
		{"empty array after an echoed example", "The format is [] for quiet sessions.\n\n[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseJSONArray[tobs](tc.reply)
			if err != nil {
				t.Errorf("a quiet session must not error: got %v (%v) — a false truncation makes the "+
					"session back off forever instead of advancing", err, got)
			}
			if len(got) != 0 {
				t.Errorf("want an empty slice for a quiet session, got %v", got)
			}
		})
	}
}

// Genuine drift must stay drift. A false truncation is not harmless — it turns a
// deterministically-drifting model into a session that backs off forever instead of
// advancing, which is the wedge the drift-advances rule exists to prevent.
func TestParseJSONArrayStillReportsDriftForProse(t *testing.T) {
	drift := []struct{ name, reply string }{
		{"plain prose", `I don't see anything noteworthy in this session.`},
		{"prose with a stray bracket", `The user typed [ and then stopped talking about it.`},
		{"refusal", `I can't analyze this conversation.`},
		// "[ ]" is valid JSON for an empty array, so a prose checklist once parsed as an EXPLICIT
		// empty array — a quiet session. No data is lost either way (both advance the watermark);
		// what is lost is the #57 signal that the user's model is below the lens's floor.
		{"markdown checkbox", "I have nothing to report.\n- [ ] nothing found\n- [x] reviewed"},
		{"numbered checkbox", "1. [ ] first\n2. [ ] second"},
		{"bracket in a quoted string", `The model said "look at [this]" and stopped.`},
	}
	for _, tc := range drift {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseJSONArray[tobs](tc.reply)
			if !errors.Is(err, ErrNoJSONArray) {
				t.Errorf("want ErrNoJSONArray (drift: advance the watermark), got %v", err)
			}
		})
	}
}

// A complete array is not the end of the truncation check: an unclosed array AFTER the winner,
// whose next character opens an element, means the real answer was cut off. The winner is then an
// echoed prompt example, and returning it put the example into L1 as evidence and advanced the
// watermark past the truncated real observations — fabricated evidence and permanent loss at once.
// Trailing prose with a stray '[' that opens no element must still return the array, or a
// deterministic false positive stalls the lens in review.
func TestTruncationCheckAfterACompleteArray(t *testing.T) {
	cases := []struct {
		name, reply string
		want        string // "" = want ErrTruncatedJSONArray
	}{
		{"fenced prompt example, then the real array cut off",
			"The format is:\n```json\n[{\"observation\":\"EXAMPLE\"}]\n```\nHere are mine:\n" +
				`[{"observation":"real one"},{"observation":"real, cut`, ""},
		{"unfenced prompt example, then the real array cut off",
			`Format: [{"observation":"EXAMPLE"}]. Here are yours: [{"observation":"real, cut`, ""},
		// Same, with an unclosed prose '[' between: skipping it must not stop the scan, or the
		// echoed EXAMPLE is returned as the answer.
		{"example, an unclosed prose bracket, then the real array cut off",
			"Format: [{\"observation\":\"EXAMPLE\"}]. See [the notes. Mine:\n" + `[{"observation":"real, cut`, ""},
		{"array then a stray bracket in prose", `[{"observation":"first"}] — see [the "x" message for why.`, "first"},
		{"array then trailing prose", `[{"observation":"first"}] and that is all I found.`, "first"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseJSONArray[tobs](tc.reply)
			if tc.want == "" {
				if !errors.Is(err, ErrTruncatedJSONArray) {
					t.Fatalf("want ErrTruncatedJSONArray, got (%+v, %v): the echoed example would be "+
						"written to L1 and the cut-off real observations lost", got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != 1 || got[0].Observation != tc.want {
				t.Fatalf("got %+v, want %q", got, tc.want)
			}
		})
	}
}
