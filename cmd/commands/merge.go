package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newMergeCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:     "merge <export-dir | file.db>",
		GroupID: groupAdmin,
		Short:   "Merge another witness archive into this one (shows what would arrive, then asks).",
		Long: strings.TrimSpace(`
Merge another witness archive into this one — for a machine that already has its
own archive, where copying a backup in would REPLACE it. Point it at a directory
made by 'witness export --all', or at a single .db file (a plain 'witness export',
or a backup from before v0.8).

Nothing is replaced and the model is never called. Sessions, observations and
facets are unioned with their full histories, so both archives keep their real
dates; a session both machines already mined is not imported twice. Where both
archives minted the SAME facet key with different histories, the copy with newer
evidence keeps the key and the other is kept, archived, beside it. Duplicates the
two wrote under DIFFERENT keys are left for 'witness compact', which merges them
by meaning.

It is a one-time consolidation, not a sync: two machines merged back and forth
every week would grow an archived copy of every facet both changed, every round.

It first shows exactly what would arrive and changes nothing; then it asks. Only
on 'y' does it back this archive up with 'witness export --all' — a merged archive
cannot be split apart again — merge, and print how to undo. Anything else leaves
the archive untouched and exits non-zero. --yes merges without asking, for scripts;
it still backs up first.

Lens definitions found in the export are registered here but NOT enabled
(enabling one would mine every session you have). Your config.toml, your portrait
prompts (summarize/) and your profile are never replaced. Facet text from the
other archive is served to your agents through the MCP tools like your own.`),
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdMerge(args[0], yes, askYesNo)
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "merge without asking, for scripts (it still backs this archive up first)")
	return c
}

// errMergeDeclined is returned when the user does not confirm. It is an ERROR, unlike the
// abort in cleanup and backfill: those leave nothing for a following command to misread, but
// `witness merge x && rm -rf x` would delete the only copy of an archive that was never merged.
// A script with no one to answer reads EOF, which is a "no" — so it fails loudly, and says --yes.
var errMergeDeclined = errors.New("not merged — nothing was written")

// resolveMergeSource accepts an `export --all` directory or a single database file. It never
// suggests a command that reuses the source path: an earlier draft's "create one with: witness
// export --all <dir>" exported THIS archive over the user's backup, and with --force deleted it.
func resolveMergeSource(target string) (dbPath, dir string, err error) {
	target = strings.TrimSpace(target)
	info, err := os.Stat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", fmt.Errorf("%s does not exist (expected a directory from `witness export --all`, or a .db file)", tildePath(target))
	}
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return target, "", nil
	}
	db := filepath.Join(target, "witness.db")
	if _, err := os.Stat(db); err == nil {
		return db, target, nil
	}
	cands, _ := filepath.Glob(filepath.Join(target, "*.db"))
	if len(cands) > 0 {
		return "", "", fmt.Errorf("%s has no witness.db; did you mean one of these? %s",
			target, "witness merge "+strings.Join(cands, "  ·  witness merge "))
	}
	return "", "", fmt.Errorf("%s holds no witness database (expected witness.db, as `witness export --all` writes)", target)
}

// premergeBackupPath is the sibling directory a merge backs the archive up to. Root is cleaned at
// resolution; the inside-root check is the backstop for the failure it once had: a trailing slash
// put the backup INSIDE the archive, and following the undo instructions deleted it.
func premergeBackupPath(root string, now time.Time) (string, error) {
	root = filepath.Clean(root)
	p := root + ".premerge-" + now.UTC().Format("20060102-150405")
	if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("the backup path %s would be inside the archive %s", p, root)
	}
	return p, nil
}

// cmdMerge shows the plan, asks, and only then backs up and merges. ask is injected so tests can
// answer; yes skips it.
func cmdMerge(target string, yes bool, ask func(prompt string) bool) error {
	dbPath, dir, err := resolveMergeSource(target)
	if err != nil {
		return err
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	defer setupLogging(st)()

	// The worker rewrites the whole facet table on every review; a merge interleaved with it would
	// have its facets deleted by the worker's next write. Unlike compact this is an ERROR, not exit 0:
	// `witness merge x --yes && rm -rf x` must not delete the only copy after merging nothing.
	unlock, ok := st.WorkerLock()
	if !ok {
		return workerBusyErr(st, "nothing was merged")
	}
	defer unlock()

	ctx := context.Background()
	now := time.Now()
	// Plan first (a rolled-back transaction): the user approves what they were shown, and a re-run
	// with nothing new exits here without taking a backup.
	rep, err := st.Merge(ctx, dbPath, false, now)
	if err != nil {
		return err
	}
	files := planMergeFiles(st, dir, rep)
	if rep.Empty() && !files.changes() {
		if files.notes() {
			// Never "everything is already here" when the export holds a definition that differs
			// from ours: prompt text is in no database, and that claim invites discarding the export.
			fmt.Printf("%s no data to merge from %s, but it holds definitions that differ from yours:\n", skipGlyph(), tildePath(target))
			printMergeFiles(st.Root, files)
			return nil
		}
		fmt.Printf("%s nothing to merge — everything in %s is already here\n", skipGlyph(), tildePath(target))
		return nil
	}

	printMergePlan(target, st.Root, rep, files)
	// The plan's transaction is closed before asking: held across a human pause it would block every
	// capture hook until the user answered. The worker lock stays held, so no review rewrites the
	// facets meanwhile; the apply recomputes the same deterministic merge, and can differ from what
	// was shown only by turns captured while the prompt was open.
	if !yes && !ask(bold("Merge it?")+dim(" This archive is backed up first.")+" [y/N]: ") {
		return errMergeDeclined
	}

	backup, err := premergeBackupPath(st.Root, now)
	if err != nil {
		return err
	}
	if _, err := st.ExportAll(backup, false); err != nil {
		return fmt.Errorf("could not back this archive up before merging (%w); nothing was merged", err)
	}
	if rep, err = st.Merge(ctx, dbPath, true, now); err != nil {
		return fmt.Errorf("%w (nothing was merged; this archive is unchanged — the backup at %s is a copy of it and can be deleted)", err, backup)
	}
	files.apply(st)
	printMergeDone(st.Root, rep, files, backup)
	if n := len(files.failed); n > 0 {
		// The data is merged, but a lens prompt that failed to register exists ONLY in the export.
		// Exit non-zero so `merge x --yes && rm -rf x` cannot delete it.
		return fmt.Errorf("merged, but %d lens %s NOT registered (see above) — keep %s and retry", n,
			map[bool]string{true: "definition was", false: "definitions were"}[n == 1], target)
	}
	return nil
}

// mergeFiles is the file half of a merge: lens definitions, portrait prompts, profile.
type mergeFiles struct {
	register  []mergeLens // incoming-only lenses that pass registration: registered, not enabled
	differ    []mergeLens // same name, different definition: ours kept
	refused   []string    // incoming lens dirs registration would refuse, with the reason
	summarize []string    // portrait prompts that differ from ours: NEVER copied, reported
	unknown   []string    // lenses whose DATA arrived with no definition here or in the export
	disabled  []string    // lenses with data here whose definition is registered but not enabled
	srcDir    string
	profile   bool
	results   []string // what the merge actually did
	failed    []string // lenses the merge could not register
}

type mergeLens struct{ name, path string }

func (f *mergeFiles) changes() bool { return len(f.register) > 0 }

// notes reports whether there is something worth telling the user even with nothing to merge.
func (f *mergeFiles) notes() bool { return len(f.differ) > 0 || len(f.summarize) > 0 }

func planMergeFiles(st *store.Store, dir string, rep *store.MergeReport) *mergeFiles {
	f := &mergeFiles{srcDir: dir}
	registered := st.RegisteredLenses()
	inExport := map[string]bool{}
	if dir != "" {
		entries, _ := os.ReadDir(filepath.Join(dir, "lenses"))
		for _, e := range entries {
			// Staging residue (.tmp/.bak) and stray files are not lenses.
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasSuffix(e.Name(), ".tmp") || strings.HasSuffix(e.Name(), ".bak") {
				continue
			}
			l := mergeLens{name: e.Name(), path: filepath.Join(dir, "lenses", e.Name())}
			inExport[l.name] = true
			switch {
			case slices.Contains(registered, l.name):
				if !sameLensDef(l.path, filepath.Join(st.LensesDir(), l.name)) {
					f.differ = append(f.differ, l)
				}
			default:
				// Every check RegisterLens will make, now: the dry run must not promise a
				// registration the apply then refuses — that kept a re-run from converging,
				// taking another full-archive backup every time.
				if err := st.CheckLensRegistration(l.name, l.path); err != nil {
					f.refused = append(f.refused, fmt.Sprintf("%s (%v)", l.name, err))
					continue
				}
				f.register = append(f.register, l)
			}
		}
		// Portrait prompts are reported, never copied. One replaces the instruction the summarizer
		// writes the portrait with — an archive of market records would reframe a personal portrait
		// as a market report — and witness enables no foreign prompt without the user asking.
		sums, _ := filepath.Glob(filepath.Join(dir, "summarize", "*.md"))
		for _, src := range sums {
			ours, err := os.ReadFile(filepath.Join(st.Root, "summarize", filepath.Base(src)))
			theirs, err2 := os.ReadFile(src)
			if err2 == nil && (err != nil || !bytes.Equal(ours, theirs)) {
				f.summarize = append(f.summarize, src)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "profile")); err == nil {
			f.profile = true
		}
	}
	// Data for a lens arrives whatever the source is — a single .db carries no lens definitions at
	// all — and nothing reviews a lens that is not registered AND enabled. Say so.
	enabled := st.LoadConfig().EnabledLenses
	for _, l := range rep.Lenses {
		switch {
		case !slices.Contains(registered, l) && !inExport[l]:
			f.unknown = append(f.unknown, l)
		case slices.Contains(registered, l) && !slices.Contains(enabled, l):
			f.disabled = append(f.disabled, l)
		}
	}
	return f
}

// sameLensDef compares the files a lens definition consists of.
func sameLensDef(a, b string) bool {
	for _, name := range []string{"lens.json", "extract.md", "review.md", "emerge.md"} {
		x, errA := os.ReadFile(filepath.Join(a, name))
		y, errB := os.ReadFile(filepath.Join(b, name))
		if (errA == nil) != (errB == nil) || !bytes.Equal(x, y) {
			return false
		}
	}
	return true
}

func (f *mergeFiles) apply(st *store.Store) {
	for _, l := range f.register {
		if err := st.RegisterLens(l.name, l.path); err != nil {
			f.failed = append(f.failed, l.name)
			f.results = append(f.results, lensRow(badGlyph(), l.name, fmt.Sprintf("NOT registered (%v) — its prompt exists only in the export", err)))
			continue
		}
		f.results = append(f.results, lensRow(cyan("+"), l.name, "registered, NOT enabled")+"\n"+
			subHint("enable", "witness lens enable "+l.name))
	}
}

// lensRow is one lens line of a merge screen. Every lens row leads with a glyph, so the names line
// up under each other whatever the row says.
func lensRow(glyph, name, text string) string {
	return mergeRow("lens") + padGlyph(glyph) + " " + bold(name) + "  " + text
}

// joinNonZero renders the non-zero parts of a count line ("552 new · 3 longer there"), or none.
// A row of "0 already here · 0 longer there · 0 longer here" said nothing four times.
func joinNonZero(none string, parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" && !strings.HasPrefix(p, "0 ") {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return none
	}
	return strings.Join(kept, " · ")
}

func printMergePlan(src, root string, r *store.MergeReport, f *mergeFiles) {
	fmt.Println(header(fmt.Sprintf("merge %s → %s", tildePath(src), tildePath(root))))
	span := "no transcripts"
	if r.FirstTS != "" {
		span = r.FirstTS[:min(10, len(r.FirstTS))] + " → " + r.LastTS[:min(10, len(r.LastTS))]
	}
	fmt.Printf(mergeRow("incoming")+"%s · schema v%d", span, r.IncomingSchema)
	if len(r.Cwds) > 0 {
		cwds := make([]string, len(r.Cwds))
		for i, c := range r.Cwds {
			cwds[i] = tildePath(c)
		}
		fmt.Printf(" · mostly in %s", strings.Join(cwds, ", "))
	}
	fmt.Println()

	s := r.Sessions
	longer := ""
	if s.LongerThere > 0 {
		longer = fmt.Sprintf("%s longer there (%s appended)", humanCount(s.LongerThere), countOf(s.Appended, "turn", "turns"))
	}
	fmt.Println(mergeRow("sessions") + joinNonZero("none new", humanCount(s.New)+" new", humanCount(s.Equal)+" already here",
		longer, humanCount(s.LongerHere)+" longer here"))
	for _, d := range s.Diverged {
		fmt.Printf("                %s %s differs from yours — yours keeps the id, theirs arrives as %s\n", warnGlyph(), d.From, d.To)
	}
	if s.Duplicates > 0 {
		fmt.Printf("                %s skipped: a second copy of a transcript already here\n", humanCount(s.Duplicates))
	}
	o := r.Observations
	fmt.Println(mergeRow("observations") + joinNonZero("none new", humanCount(o.Reviewed)+" reviewed there",
		humanCount(o.Unreviewed)+" to review", humanCount(o.AlreadyHere)+" already here",
		humanCount(o.SharedMined)+" skipped (sessions you already mined)",
		humanCount(o.Orphaned)+" skipped (yours will be mined instead)"))
	if r.Staged > 0 {
		fmt.Printf(mergeRow("staged")+"%s recorded in-session, not yet distilled\n", humanCount(r.Staged))
	}
	fc := r.Facets
	fmt.Println(mergeRow("facets") + joinNonZero("none new", humanCount(fc.New)+" new", humanCount(fc.Shared)+" already here",
		humanCount(fc.Updated)+" updated", humanCount(fc.FastForwarded)+" with later history adopted"))
	if len(fc.SameKey) > 0 {
		fmt.Printf("                %s %s with yours — the copy with newer evidence keeps the key,\n"+
			"                  the other is kept beside it, archived into it:\n",
			warnGlyph(), pluralize(len(fc.SameKey), "shares a key", "share a key"))
		for i, k := range fc.SameKey {
			if i == 5 {
				fmt.Printf("                  … and %d more\n", len(fc.SameKey)-5)
				break
			}
			kept := "yours"
			if k.IncomingWon {
				kept = "theirs"
			}
			fmt.Printf("                  %s  %s/%s  (kept %s; other copy %s)\n", k.Lens, k.Dimension, k.Key, kept, k.LoserKey)
		}
	}
	printMergeFiles(root, f)
	fmt.Println(mergeRow("config.toml") + "yours is kept")
	fmt.Println()
}

// printMergeDone reports the applied merge and how to undo it.
func printMergeDone(root string, r *store.MergeReport, f *mergeFiles, backup string) {
	for _, line := range f.results {
		fmt.Println(line)
	}
	glyph := okGlyph()
	if len(f.failed) > 0 {
		glyph = warnGlyph()
	}
	fmt.Printf("%s merged: %s, %s, %s new here\n", glyph, countOf(r.Sessions.New, "session", "sessions"),
		countOf(r.Observations.Reviewed+r.Observations.Unreviewed, "observation", "observations"),
		countOf(r.Facets.New, "facet", "facets"))
	fmt.Println("  " + kvRow("backup", tildePath(backup), "this archive as it was before"))
	fmt.Println("  " + kvRow("to undo", "stop witness, then replace "+tildePath(root)+" with the backup", ""))
	// The note goes on its own line: after the command it made the hint uncopyable as a whole line.
	fmt.Println(dim("  the same trait may now sit under two keys; compact merges them (it calls the model)"))
	fmt.Println(hint("dedupe", "witness compact"))
}

func printMergeFiles(root string, f *mergeFiles) {
	for _, l := range f.register {
		fmt.Println(lensRow(cyan("+"), l.name, "will be registered, NOT enabled"))
	}
	for _, l := range f.differ {
		fmt.Println(lensRow(warnGlyph(), l.name, "their definition differs — yours is kept"))
		fmt.Println(subHint("theirs", "witness lens register "+l.name+" "+shellArg(l.path)))
	}
	for _, n := range f.refused {
		fmt.Println(lensRow(warnGlyph(), n, "would be refused here, skipped"))
	}
	for _, n := range f.unknown {
		fmt.Println(lensRow(warnGlyph(), n, "its data arrives, but there is no definition for it here"))
		fmt.Println(subHint("define it", "witness lens register "+n+" <dir>"))
	}
	for _, n := range f.disabled {
		// True whether or not anything arrives: this also prints when there is no data to merge.
		fmt.Println(lensRow(skipGlyph(), n, "registered here but not enabled — nothing reviews its data"))
		fmt.Println(subHint("review it", "witness lens enable "+n))
	}
	for _, src := range f.summarize {
		dst := filepath.Join(root, "summarize", filepath.Base(src))
		note := ""
		if _, err := os.Stat(dst); err == nil {
			note = " (using it REPLACES yours — copy yours aside first)"
		}
		fmt.Println(mergeRow("summarize") + padGlyph(skipGlyph()) + " " + bold(filepath.Base(src)) + "  not copied — it changes how your portrait is written" + note)
		fmt.Println(subHint("use it", fmt.Sprintf("mkdir -p %q && cp %q %q", filepath.Dir(dst), src, dst)))
	}
	if f.profile {
		fmt.Println(mergeRow("profile") + padGlyph(skipGlyph()) + " not merged — rebuilt from the merged facets " +
			dim("(theirs stays in "+tildePath(filepath.Join(f.srcDir, "profile"))+")"))
	}
}

// mergeRow is a plan row's label: kvRow's dim 14-column label, so the values line up with every
// other row style (and a pipe sees exactly the same bytes as before color).
func mergeRow(name string) string { return "  " + kvRow(name, "", "") }
