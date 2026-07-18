package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMineConcurrencyParse(t *testing.T) {
	if DefaultConfig().MineConcurrency != DefaultMineConcurrency {
		t.Fatalf("default MineConcurrency=%d want %d", DefaultConfig().MineConcurrency, DefaultMineConcurrency)
	}
	dir := t.TempDir()
	t.Setenv("WITNESS_HOME", dir)
	s, _ := Open()
	defer s.Close()
	// EnsureConfigFile template should carry the knob at the default.
	if got := s.LoadConfig().MineConcurrency; got != DefaultMineConcurrency {
		t.Fatalf("template config MineConcurrency=%d want %d", got, DefaultMineConcurrency)
	}
	// Override + <=0 restores default.
	for _, tc := range []struct {
		line string
		want int
	}{
		{"mine_concurrency = 8", 8},
		{"mine_concurrency = 0", DefaultMineConcurrency},
		{"mine_concurrency = -3", DefaultMineConcurrency},
	} {
		os.WriteFile(filepath.Join(dir, "config.toml"), []byte(tc.line+"\n"), 0o600)
		if got := s.LoadConfig().MineConcurrency; got != tc.want {
			t.Errorf("%q -> MineConcurrency=%d want %d", tc.line, got, tc.want)
		}
	}
}

// TestChunkMaxCharsParse pins the #57 knob: default 0 (never chunk / mine whole),
// the template ships at 0, a positive value is honored verbatim, and — UNLIKE
// mine_concurrency — a 0 or negative value is NOT clamped to a default (any <=0 means
// "send whole", the intended default), so it round-trips as-is.
func TestChunkMaxCharsParse(t *testing.T) {
	if DefaultConfig().ChunkMaxChars != 0 {
		t.Fatalf("default ChunkMaxChars=%d, want 0 (chunking off)", DefaultConfig().ChunkMaxChars)
	}
	dir := t.TempDir()
	t.Setenv("WITNESS_HOME", dir)
	s, _ := Open()
	defer s.Close()
	// EnsureConfigFile template must ship the knob at the default (off).
	if got := s.LoadConfig().ChunkMaxChars; got != 0 {
		t.Fatalf("template config ChunkMaxChars=%d, want 0", got)
	}
	for _, tc := range []struct {
		line string
		want int
	}{
		{"chunk_max_chars = 200000", 200000},
		{"chunk_max_chars = 0", 0},   // explicit off, accepted verbatim (not clamped)
		{"chunk_max_chars = -5", -5}, // still means "send whole"; stored as-is, RenderChunks treats <=0 as off
	} {
		os.WriteFile(filepath.Join(dir, "config.toml"), []byte(tc.line+"\n"), 0o600)
		if got := s.LoadConfig().ChunkMaxChars; got != tc.want {
			t.Errorf("%q -> ChunkMaxChars=%d want %d", tc.line, got, tc.want)
		}
	}
}
