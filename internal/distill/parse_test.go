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
