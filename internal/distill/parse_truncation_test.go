package distill

import (
	"errors"
	"strings"
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

// A markdown checkbox is not an empty result.
//
// "[ ]" is valid JSON for an empty array, so a model that drifted into a prose checklist
// ("- [ ] nothing to report") parsed as an EXPLICIT empty array — recorded as "the lens
// looked and found nothing", a genuinely quiet session. Both outcomes advance the watermark,
// so no data is lost; what was lost is the #57 signal that the user's model is below the
// lens's floor, which is the entire reason that signal exists.
func TestParseJSONArrayDoesNotReadAMarkdownCheckboxAsAQuietSession(t *testing.T) {
	reply := "I could not find anything to extract.\n\n- [ ] nothing to report\n- [ ] no patterns\n"
	got, err := ParseJSONArray[tobs](reply)
	if !errors.Is(err, ErrNoJSONArray) {
		t.Fatalf("a checklist reply must read as DRIFT, not a quiet session: err=%v obs=%v", err, got)
	}
}

// An explicit empty array IS a quiet session, and must stay one — that is the legitimate
// "the lens looked and found nothing" reply, distinct from drift.
func TestParseJSONArrayStillHonorsAnExplicitEmptyArray(t *testing.T) {
	for _, reply := range []string{`[]`, "```json\n[]\n```", "Nothing this time:\n[]\n"} {
		got, err := ParseJSONArray[tobs](reply)
		if err != nil {
			t.Errorf("explicit empty array must be a quiet session, got err %v (reply %q)", err, reply)
		}
		if len(got) != 0 {
			t.Errorf("want 0 obs, got %d", len(got))
		}
	}
}

// A checkbox appearing ALONGSIDE a real array must not suppress the real array, and a
// complete array is never mistaken for truncation.
func TestParseJSONArrayPrefersARealArrayOverSurroundingProse(t *testing.T) {
	reply := "- [ ] scratch note\nHere is the result:\n[{\"observation\":\"real finding\"}]\n- [ ] done"
	got, err := ParseJSONArray[tobs](reply)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Observation != "real finding" {
		t.Fatalf("got %+v, want the real array", got)
	}
}

// The last-wins and object-wrapped behaviors must survive the truncation check: a complete
// array anywhere means no truncation error, whatever else the reply contains.
func TestTruncationCheckDoesNotDisturbCompleteReplies(t *testing.T) {
	cases := []struct {
		name, reply string
		want        string
	}{
		{"echoed example then the real answer",
			`Format: [{"observation":"EXAMPLE"}]. Here are yours: [{"observation":"real"}]`, "real"},
		{"object wrapped", `{"observations":[{"observation":"wrapped"}]}`, "wrapped"},
		{"array then trailing prose", `[{"observation":"first"}] and that is all I found.`, "first"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseJSONArray[tobs](tc.reply)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != 1 || got[0].Observation != tc.want {
				t.Fatalf("got %+v, want %q", got, tc.want)
			}
		})
	}
}

// A truncated reply must not be silently emptied even when it is very long — the failure
// mode that motivated this is a big session hitting an output cap.
func TestLongTruncatedReplyIsRetryable(t *testing.T) {
	var b strings.Builder
	b.WriteString("[\n")
	for i := 0; i < 200; i++ {
		b.WriteString(`  {"observation":"finding number ` + strings.Repeat("x", 40) + `"},` + "\n")
	}
	b.WriteString(`  {"observation":"cut off here`)
	_, err := ParseJSONArray[tobs](b.String())
	if !errors.Is(err, ErrTruncatedJSONArray) {
		t.Fatalf("a 200-element truncated reply must be retryable, got %v", err)
	}
}
