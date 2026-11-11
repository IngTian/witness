package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/store"
)

// captureStdoutErr is captureStdout (lens_try_test.go) for a fn that returns an error, so a
// test can assert on the EXIT CODE and the emitted document together.
func captureStdoutErr(t *testing.T, fn func() error) (out string, err error) {
	t.Helper()
	out = captureStdout(t, func() { err = fn() })
	return out, err
}

// `witness doctor --json` must FAIL when the archive cannot distill.
//
// The --json branch returned emitJSON(out) directly, discarding deferredErr — so on a machine
// where the embedder model was never downloaded (or the runner is typo'd, or an OpenCode model
// the provider rejects), `witness doctor --json; echo $?` printed a report whose embedder status
// said UNAVAILABLE and exited **0**, while the human `witness doctor` printed a red [bad] line
// and exited 1. Any CI step or install-verify script doing `witness doctor --json || fail` — the
// natural machine-readable form of the check install.sh and `make doctor` point users at —
// reported the archive healthy while nothing could ever be mined.
func TestDoctorJSONFailsWhenTheEmbedderIsUnavailable(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	// Point the embedder at an empty dir so embed.New() fails, exactly as it does on a machine
	// whose model download never completed.
	t.Setenv("WITNESS_ASSETS", t.TempDir())

	out, err := captureStdoutErr(t, func() error { return cmdDoctor(true) })

	// The payload must still be emitted IN FULL — that is the whole point of --json, and a
	// consumer needs to see embedder.status to know what is wrong.
	var report map[string]any
	if jsonErr := json.Unmarshal([]byte(out), &report); jsonErr != nil {
		t.Fatalf("--json must still print a parseable document on failure; got %q (%v)", out, jsonErr)
	}
	if _, ok := report["embedder"]; !ok {
		t.Errorf("the report is missing the embedder section: %v", report)
	}

	// And the command must FAIL, so `doctor --json || fail` works.
	if err == nil {
		t.Fatal("doctor --json exited 0 on an archive whose embedder is unavailable — a CI health " +
			"check would report it healthy while no session can ever be mined")
	}
}

// A HEALTHY archive must still exit 0 with a parseable report — the fix must not make doctor
// fail spuriously, or every CI check that uses it starts failing.
func TestDoctorJSONSucceedsOnAHealthyArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("loads the real embedder model")
	}
	// Point at the repo's bundled model so this actually EXERCISES the success path rather than
	// skipping. Without it the test silently degrades into "no coverage that doctor still exits
	// 0", which is exactly the half a fix like this could break.
	assets := filepath.Join("..", "..", "assets", "e5-small")
	if _, statErr := os.Stat(filepath.Join(assets, "model.onnx")); statErr != nil {
		t.Skipf("bundled embedder model not present at %s: %v", assets, statErr)
	}
	abs, err := filepath.Abs(assets)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WITNESS_ASSETS", abs)
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))

	out, err := captureStdoutErr(t, func() error { return cmdDoctor(true) })
	if err != nil {
		t.Fatalf("doctor --json failed on a healthy archive: %v (out=%s)", err, out)
	}
	var report map[string]any
	if jsonErr := json.Unmarshal([]byte(out), &report); jsonErr != nil {
		t.Fatalf("healthy --json output is not parseable: %q (%v)", out, jsonErr)
	}
	// And it must actually have reported a working embedder, not merely exited 0.
	emb, _ := report["embedder"].(map[string]any)
	if status, _ := emb["status"].(string); !strings.HasPrefix(status, "OK") {
		t.Errorf("expected a healthy embedder on the success path, got status %q", status)
	}
}

// fixedEmbedder stands in for the model: the same vector for every text.
type fixedEmbedder struct{ vec []float32 }

func (f fixedEmbedder) Embed(string) ([]float32, error) { return f.vec, nil }

// The success path WITHOUT the real model, so it runs in CI. The real-model test above always
// skips there (the model is gitignored), which left "a healthy archive exits 0" unguarded: a
// doctor that always returned an error passed the suite.
func TestDoctorJSONSucceedsWithAWorkingEmbedder(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	prev := newDoctorEmbedder
	newDoctorEmbedder = func() (distill.Embedder, error) { return fixedEmbedder{vec: []float32{1, 0, 0}}, nil }
	t.Cleanup(func() { newDoctorEmbedder = prev })
	// Only mine_model set: review falls back to it, which the raw distill_model ("") hid (#199 G030).
	t.Setenv("WITNESS_RUNNER", "")
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := configApplySet(st, "mine_model", "claude-haiku-4-5", ""); err != nil {
		t.Fatal(err)
	}
	st.Close()

	out, err := captureStdoutErr(t, func() error { return cmdDoctor(true) })
	if err != nil {
		t.Fatalf("doctor --json failed with a working embedder: %v (out=%s)", err, out)
	}
	var report struct {
		Config   map[string]any `json:"config"`
		Embedder struct {
			Status    string `json:"status"`
			Dimension int    `json:"dimension"`
		} `json:"embedder"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &report); jsonErr != nil {
		t.Fatalf("--json output is not parseable: %q (%v)", out, jsonErr)
	}
	if report.Embedder.Status != "OK" || report.Embedder.Dimension != 3 {
		t.Errorf("embedder = %+v, want status OK and dimension 3", report.Embedder)
	}
	for key, want := range map[string]string{
		"triage_model": "claude-haiku-4-5", "distill_model": "", // raw, as typed in config.toml
		"mine_model": "claude-haiku-4-5", "mine_model_from": "config.toml", // effective
		"review_model": "claude-haiku-4-5", "review_model_from": "falls back to mine_model",
	} {
		if got, ok := report.Config[key]; !ok || got != want {
			t.Errorf("config.%s = %v (present=%v), want %q", key, got, ok, want)
		}
	}
}

// The human path must keep its own behavior: it already returned deferredErr, and the fix must
// not change that or double-report.
func TestDoctorHumanPathStillFailsWhenTheEmbedderIsUnavailable(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_PROMPTS", filepath.Join("..", "..", "prompts"))
	t.Setenv("WITNESS_ASSETS", t.TempDir())
	t.Setenv("NO_COLOR", "1")

	out, err := captureStdoutErr(t, func() error { return cmdDoctor(false) })
	if err == nil {
		t.Error("the human doctor must exit non-zero when the embedder is unavailable")
	}
	if !strings.Contains(out, "witness doctor") {
		t.Errorf("the human report was not printed: %q", out)
	}
}

// F095: doctor shows each enabled lens's portrait-writer input as plain info, measured the way
// the writer renders it, so a user can see their headroom before a call fails on it.
func TestDoctorShowsPortraitInputSize(t *testing.T) {
	st := readFixture(t) // the default lens is enabled
	t.Setenv("WITNESS_ASSETS", t.TempDir())
	t.Setenv("NO_COLOR", "1")
	facets := []store.Facet{{Lens: store.LensDefault, Dimension: "traits", Key: "k",
		Versions: []store.FacetVersion{{Value: strings.Repeat("x", 5000), Confidence: 0.9}}}}
	if err := st.WriteFacets(facets); err != nil {
		t.Fatal(err)
	}
	out, _ := captureStdoutErr(t, func() error { return cmdDoctor(false) })
	want := "portrait input  default: " + distill.InputSize(distill.PortraitInputChars(facets)[store.LensDefault])
	if !strings.Contains(out, want) || !strings.Contains(want, "5k chars (~1k tokens)") {
		t.Fatalf("doctor should show %q, got:\n%s", want, out)
	}
}
