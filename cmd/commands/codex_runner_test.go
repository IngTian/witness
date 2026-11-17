package commands

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IngTian/witness/internal/platform"
	"github.com/IngTian/witness/internal/store"
)

func TestWireCodexDoesNotBindWithoutRunnerLogin(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	t.Setenv(platform.DisableExternalRunnersEnv, "1")
	previous := editors["codex"]
	t.Cleanup(func() { editors["codex"] = previous })
	var called bool
	registerFakeCaptureOnlyEditor(t, "codex", &called)
	var err error
	out := captureStdout(t, func() { err = cmdInstall([]string{"codex"}) })
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if !called || st.RunnerBound() || !strings.Contains(out, "witness auth codex") {
		t.Fatalf("runner bound before login: %s", out)
	}
}

func TestCodexModelPromptAcceptsBareNames(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SetRunner("codex"); err != nil {
		t.Fatal(err)
	}
	if err := askForModels(st, "codex", bytes.NewBufferString("gpt-test\ngpt-review\n")); err != nil {
		t.Fatal(err)
	}
	cfg := st.LoadConfig()
	if cfg.TriageModel != "gpt-test" || cfg.DistillModel != "gpt-review" {
		t.Fatalf("config: %+v", cfg)
	}
}

type planRunner struct{ limitedPreviewRunner }

func (planRunner) OnPlanAllowance(string) bool { return true }

func TestPlanPauseIsIndependentAndSurvivesNewGate(t *testing.T) {
	meta := &mapMeta{}
	gate := testGate(meta)
	reset := gateNow.Add(time.Hour)
	var calls atomic.Int32
	refuse := gate.wrap("codex", planRunner{}, counted(&calls, func() (string, error) { return "", &platform.QuotaExhaustedError{Runtime: "codex", ResetAt: reset} }))
	_, _ = refuse(context.Background(), "test", "p", "i")
	var quota *platform.QuotaExhaustedError
	_, err := testGate(meta).wrap("codex", planRunner{}, counted(&calls, ok))(context.Background(), "test", "p", "i")
	if !errors.As(err, &quota) || calls.Load() != 1 {
		t.Fatalf("pause lost: %d calls, %v", calls.Load(), err)
	}
	if meta.MetaString(freeModelsPausedKey) != "" {
		t.Fatal("Codex paused OpenCode")
	}
	_, err = gate.wrap("other-plan", planRunner{}, counted(&calls, ok))(context.Background(), "test", "p", "i")
	if err != nil || calls.Load() != 2 {
		t.Fatal("Codex paused a different plan")
	}
	if err := meta.SetMetaString(freeModelsPausedKey, reset.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := meta.SetMetaString(runnerPauseKey("codex"), gateNow.Add(-time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	_, err = testGate(meta).wrap("codex", planRunner{}, counted(&calls, ok))(context.Background(), "test", "p", "i")
	if err != nil || meta.MetaString(runnerPauseKey("codex")) != "" || meta.MetaString(freeModelsPausedKey) == "" {
		t.Fatal("probe cleared a different allowance", err)
	}
}

func TestPlanPauseSchedulesWakeupAndAppearsInStatus(t *testing.T) {
	t.Setenv("WITNESS_HOME", t.TempDir())
	t.Setenv("WITNESS_NO_DEFAULT_LENS", "1")
	st, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if err := st.SetMetaString(runnerPauseKey("codex"), reset.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if got, ok := retryWakeupAt(st, time.Now()); !ok || !got.Equal(reset) {
		t.Fatal("no plan wakeup", got)
	}
	st.Close()
	out := captureStdout(t, func() { _ = cmdDistillStatus(true) })
	if !strings.Contains(out, `"runner_usage_paused_until"`) || !strings.Contains(out, reset.Format(time.RFC3339)) {
		t.Fatal(out)
	}
}
