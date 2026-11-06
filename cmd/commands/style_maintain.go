package commands

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// Screen helpers for the commands that CHANGE something — wire/unwire, the worker, compact, merge,
// import, ingest, export, cleanup. They build on style.go's vocabulary; nothing here decides color
// on its own except the stderr variants, which follow useColorErr because errors go to stderr.

// resultRow is one part of a multi-part result: "  <glyph> " + kvRow(name, value, note). Without
// color the glyphs are ASCII of different widths ([ok], [!], *), so they are padded to one width
// there and the labels still line up.
func resultRow(glyph, name, value, note string) string {
	return "  " + padGlyph(glyph) + " " + kvRow(name, value, note)
}

// padGlyph pads an ASCII glyph ([ok], [!], +) to one width when color is off, so whatever follows
// it lines up from row to row; on a terminal every glyph is already one column.
func padGlyph(glyph string) string { return outPen().pick(glyph, fmt.Sprintf("%-4s", glyph)) }

// subHint is a next step that belongs to the row above it, indented under that row's value column
// rather than the screen's ("                ↳ enable  witness lens enable math").
func subHint(label, command string) string { return outPen().hint(16, label, command) }

// errHint is hint() for a line inside an error message: "  ↳ label  command" on stderr.
func errHint(label, command string) string { return errPen().hint(2, label, command) }

// errRow is an aligned "  label  text" line inside an error message — the cause of a failure,
// shown under its headline instead of only in witness.log.
func errRow(label, text string) string {
	if strings.TrimSpace(text) == "" {
		return "" // errLines drops it: a bare "cause" label explains nothing
	}
	return "  " + styledErr(ansiDim, fmt.Sprintf("%-15s", label)) + " " + text
}

// errLines is an error whose message is a headline plus the lines under it (a cause row, next
// steps), so the ✗ line stays short and the fix sits beneath it. Empty lines are dropped.
func errLines(headline string, lines ...string) error {
	out := []string{headline}
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return errors.New(strings.Join(out, "\n"))
}

// workerBusyHead is the one sentence for "a worker holds the lock", with its pid when it stamped one.
func workerBusyHead(st *store.Store, skipped string) string {
	head := "a distillation worker is running"
	if pid := strings.TrimSpace(st.MetaString(metaWorkerPID)); pid != "" {
		head += " (pid " + pid + ")"
	}
	return head + " — " + skipped
}

// workerBusy is the shared busy-worker screen for commands that skip and exit 0 (compact, worker
// review, worker run): four commands used to say it four different ways, one of them only after
// printing a banner for work it then did not do.
func workerBusy(st *store.Store, skipped string) string {
	return warnGlyph() + " " + workerBusyHead(st, skipped) + "\n" +
		hint("watch it", "witness status") + "\n" +
		hint("or stop it", "witness worker stop") + "\n"
}

// workerBusyErr is workerBusy for a command that must FAIL when it cannot run (merge: `merge x &&
// rm -rf x` must not delete the only copy after merging nothing).
func workerBusyErr(st *store.Store, skipped string) error {
	return errLines(workerBusyHead(st, skipped),
		errHint("watch it", "witness status"), errHint("or stop it", "witness worker stop"))
}

// causeTap keeps the first error a model call — or the runner/embedder startup — returned in this
// process, so a foreground command that failed can say WHY on the screen. Before it, compact, worker
// review and worker run printed "model error or timeout" or nothing at all, while witness.log said
// "claude runner disabled by WITNESS_DISABLE_EXTERNAL_RUNNERS". A nil tap is a no-op, which is what
// every background run passes.
type causeTap struct {
	mu    sync.Mutex
	first error
}

func (t *causeTap) note(err error) {
	if t == nil || err == nil || errors.Is(err, context.Canceled) {
		return // Ctrl-C is the user's doing, not the cause of a failure
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.first == nil {
		t.first = err
	}
}

// wrap returns f with every error it returns noted.
func (t *causeTap) wrap(f distill.MineFunc) distill.MineFunc {
	if t == nil {
		return f
	}
	return func(ctx context.Context, model, prompt, input string) (string, error) {
		out, err := f(ctx, model, prompt, input)
		t.note(err)
		return out, err
	}
}

// wrapFor is wrap for a per-lens runner resolver.
func (t *causeTap) wrapFor(f func(*lens.Lens) distill.MineFunc) func(*lens.Lens) distill.MineFunc {
	if t == nil {
		return f
	}
	return func(ln *lens.Lens) distill.MineFunc { return t.wrap(f(ln)) }
}

var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// cause is the first error as one screen line: escapes stripped, whitespace collapsed, cut to 100
// characters. A failing `claude -p` folds up to 4,000 bytes of its stderr into the error; that
// belongs in witness.log, and the head of it is what names the problem.
func (t *causeTap) cause() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.first == nil {
		return ""
	}
	s := strings.Join(strings.Fields(ansiSeq.ReplaceAllString(t.first.Error(), "")), " ")
	if r := []rune(s); len(r) > 100 {
		s = string(r[:99]) + "…"
	}
	return s
}

// shellArg renders a path for a command a person will copy: ~/… when that needs no quoting, else
// the absolute path single-quoted (a quoted ~ would not expand).
func shellArg(p string) string {
	if t := tildePath(p); !strings.ContainsAny(t, " \t'\"$`\\!*?[]{}()&;|<>#") {
		return t
	}
	return shellQuote(p)
}

// humanBytes renders a size the way a person reads it: 87.4 MB, 512 KB, 900 B.
func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, i := float64(n), 0
	for v >= unit && i < 4 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, []string{"B", "KB", "MB", "GB", "TB"}[i])
}
