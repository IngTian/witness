package store

import "testing"

// ResolvedConfig is LoadConfig + ResolveRunner in one step, and RunnerOrigin walks the same
// ladder, so the runner a surface reports and where it says it came from always agree.
func TestResolvedConfigAndRunnerOrigin(t *testing.T) {
	s := tempStore(t)
	t.Setenv("WITNESS_RUNNER", "")
	if got := s.ResolvedConfig().Runner; got != RunnerClaude || s.RunnerOrigin() != RunnerFromDefault || s.RunnerBound() {
		t.Fatalf("fresh archive: runner %q from %q (bound %v), want claude from the built-in default",
			got, s.RunnerOrigin(), s.RunnerBound())
	}

	// The unbound npm OpenCode user: config says the template's claude, the plugin's env wins.
	t.Setenv("WITNESS_RUNNER", RunnerOpenCode)
	if got := s.ResolvedConfig().Runner; got != RunnerOpenCode || s.RunnerOrigin() != RunnerFromEnv {
		t.Fatalf("unbound with WITNESS_RUNNER: runner %q from %q, want opencode from the env", got, s.RunnerOrigin())
	}

	// An explicit binding beats the env.
	if err := s.SetRunner(RunnerClaude); err != nil {
		t.Fatal(err)
	}
	if got := s.ResolvedConfig().Runner; got != RunnerClaude || s.RunnerOrigin() != RunnerFromConfig || !s.RunnerBound() {
		t.Fatalf("bound: runner %q from %q (bound %v), want claude from config.toml", got, s.RunnerOrigin(), s.RunnerBound())
	}
}

func TestProfileModTime(t *testing.T) {
	s := tempStore(t)
	if _, ok, err := s.ProfileModTime("default"); ok || err != nil {
		t.Fatalf("missing profile: ok=%v err=%v, want ok=false and no error", ok, err)
	}
	if err := s.WriteProfile("default", "# me\n"); err != nil {
		t.Fatal(err)
	}
	if mt, ok, err := s.ProfileModTime("default"); !ok || err != nil || mt.IsZero() {
		t.Fatalf("written profile: mt=%v ok=%v err=%v", mt, ok, err)
	}
	if _, _, err := s.ProfileModTime("../escape"); err == nil {
		t.Fatal("an invalid lens name must be refused, as ReadProfile refuses it")
	}
}
