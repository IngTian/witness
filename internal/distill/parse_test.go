package distill

import "testing"

// ParseJSONArray must survive the ways real models wrap output: a stray '[' in
// prose before the real array, a ```json fence, or both — none should be read as
// "no observations" (which would needlessly back off a good extraction).
func TestParseJSONArrayTolerance(t *testing.T) {
	type obs struct {
		Dimension string `json:"dimension"`
	}
	cases := []struct {
		name  string
		reply string
		want  int
	}{
		{"clean array", `[{"dimension":"a"},{"dimension":"b"}]`, 2},
		{"prose then fenced", "I noticed [the user] iterates fast.\n```json\n[{\"dimension\":\"a\"}]\n```", 1},
		{"bracket in prose then bare array", `Step [1]: done. [{"dimension":"a"}]`, 1},
		{"fenced no lang tag", "```\n[{\"dimension\":\"a\"},{\"dimension\":\"b\"}]\n```", 2},
		{"empty array", `[]`, 0},
		// #2: an empty "[]" in prose before the real array must NOT be taken as the
		// result (that silently drops the session's observations and advances the
		// watermark — permanent loss). Keep scanning for the non-empty array.
		{"empty array before real array", `No items found: []. But: [{"dimension":"x"}]`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseJSONArray[obs](tc.reply)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("got %d items, want %d (reply=%q)", len(got), tc.want, tc.reply)
			}
		})
	}

	// Genuinely no array → error (the worker treats this as a quiet session).
	if _, err := ParseJSONArray[obs]("Nothing notable happened."); err == nil {
		t.Fatalf("prose with no array should return an error")
	}
}

type dimObs struct {
	Dimension string `json:"dimension"`
}

// #3: a top-level result array must win over an array nested inside an earlier
// object (e.g. a "schema example"). Counting items isn't enough — both are length
// 1 — so this asserts which array was chosen.
func TestParseJSONArrayPrefersTopLevelOverNested(t *testing.T) {
	reply := `Schema: {"examples":[{"dimension":"x"}]}` + "\n" + `[{"dimension":"a"}]`
	got, err := ParseJSONArray[dimObs](reply)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Dimension != "a" {
		t.Fatalf("want the top-level array [a], got %+v", got)
	}
}

// #5: when several ``` fences exist, the ```json fence wins over an incidental
// ```text/```sh fence that happens to contain a decodable array.
func TestParseJSONArrayPrefersJSONFence(t *testing.T) {
	reply := "```text\nexample: [{\"dimension\":\"x\"}]\n```\n```json\n[{\"dimension\":\"a\"}]\n```"
	got, err := ParseJSONArray[dimObs](reply)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Dimension != "a" {
		t.Fatalf("the ```json fence should win, got %+v", got)
	}
}

// An object-wrapped result (no top-level array at all) must still parse — the
// top-level preference (#3) must not regress the lenient fallback.
func TestParseJSONArrayObjectWrappedFallback(t *testing.T) {
	got, err := ParseJSONArray[dimObs](`{"observations":[{"dimension":"a"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Dimension != "a" {
		t.Fatalf("object-wrapped array should still parse, got %+v", got)
	}
}

// ParseJSONArray must take the LAST qualifying array, not the first. Models routinely
// restate the prompt's own ```json example before answering, and EVERY shipped
// extract/review prompt contains such an example — so first-wins wrote the EXAMPLE into L1
// as if the user had really done those things and silently discarded the real answer.
// That is fabricated evidence in a personal growth archive.
func TestParseJSONArrayPrefersTheLastArrayOverAnEchoedExample(t *testing.T) {
	type obs struct {
		Dimension   string `json:"dimension"`
		Observation string `json:"observation"`
	}
	const echoed = `Understood — the format you want is:

` + "```json" + `
[{"dimension":"thinking","observation":"EXAMPLE from the prompt"}]
` + "```" + `

Here are the observations for THIS session:

` + "```json" + `
[{"dimension":"debugging","observation":"REAL answer for this session"}]
` + "```"
	got, err := ParseJSONArray[obs](echoed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Observation != "REAL answer for this session" {
		t.Fatalf("must take the model's real answer, not the echoed example; got %+v", got)
	}

	// A well-behaved single-array reply is unaffected by the tiebreak.
	single, err := ParseJSONArray[obs]("```json\n[{\"dimension\":\"d\",\"observation\":\"only one\"}]\n```")
	if err != nil || len(single) != 1 || single[0].Observation != "only one" {
		t.Fatalf("single-array reply must still parse: %+v err=%v", single, err)
	}

	// An empty array LATER must not erase a real earlier one — an empty result is only
	// reported when nothing non-empty was found anywhere.
	mixed, err := ParseJSONArray[obs]("```json\n[{\"dimension\":\"d\",\"observation\":\"real\"}]\n```\n\nNothing else:\n```json\n[]\n```")
	if err != nil {
		t.Fatal(err)
	}
	if len(mixed) != 1 || mixed[0].Observation != "real" {
		t.Fatalf("a trailing empty array must not discard a real one; got %+v", mixed)
	}

	// Genuinely empty stays empty (the "model found nothing" contract).
	empty, err := ParseJSONArray[obs]("```json\n[]\n```")
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty array must report no observations, got %+v err=%v", empty, err)
	}
}
