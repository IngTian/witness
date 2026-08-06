package commands

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdoutErr is captureStdout (lens_try_test.go) for a fn that returns an error, so a
// test can assert on the EXIT CODE and the emitted document together.
func captureStdoutErr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	runErr := fn()
	_ = w.Close()
	os.Stdout = prev
	out := <-done
	_ = r.Close()
	return out, runErr
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
