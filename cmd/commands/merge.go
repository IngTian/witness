package commands

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/store"
	"github.com/mattn/go-isatty"
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

// askYesNo is the production prompt. Anything but y/yes is a no, including EOF from a pipe.
func askYesNo(prompt string) bool {
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	answer := strings.TrimSpace(line)
	if !isatty.IsTerminal(os.Stdin.Fd()) {
		fmt.Println(answer) // a terminal echoes what was typed; piped input would leave the line open
	}
	switch strings.ToLower(answer) {
	case "y", "yes":
		return true
	}
	// Only when NOBODY answered — /dev/null or a closed pipe. Keyed on EOF, not on "is a terminal":
	// /dev/null is a character device, and someone who typed "n" doesn't need told about --yes.
	if err != nil && answer == "" {
		fmt.Println("(no answer — pass --yes to merge without asking)")
	}
	return false
}

// resolveMergeSource accepts an `export --all` directory or a single database file. It never
// suggests a command that reuses the source path: an earlier draft's "create one with: witness
// export --all <dir>" exported THIS archive over the user's backup, and with --force deleted it.
func resolveMergeSource(target string) (dbPath, dir string, err error) {
	target = strings.TrimSpace(target)
	info, err := os.Stat(target)
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
		return fmt.Errorf("a distillation worker is running and holds this archive; nothing was merged — " +
			"wait for it to finish, or stop it with `witness worker stop`, then retry")
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
			fmt.Printf("no data to merge from %s, but it holds definitions that differ from yours:\n", target)
			printMergeFiles(st.Root, files)
			return nil
		}
		fmt.Printf("nothing to merge — everything in %s is already here\n", target)
		return nil
	}

	printMergePlan(target, st.Root, rep, files)
	// The plan's transaction is closed before asking: held across a human pause it would block every
	// capture hook until the user answered. The worker lock stays held, so no review rewrites the
	// facets meanwhile; the apply recomputes the same deterministic merge, and can differ from what
	// was shown only by turns captured while the prompt was open.
	if !yes && !ask("Merge it? This archive is backed up first. [y/N]: ") {
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
			f.results = append(f.results, fmt.Sprintf("%s %s: NOT registered (%v) — its prompt exists only in the export", badGlyph(), l.name, err))
			continue
		}
		f.results = append(f.results, l.name+": registered, NOT enabled — enable it with `witness lens enable "+l.name+"`")
	}
}

func printMergePlan(src, root string, r *store.MergeReport, f *mergeFiles) {
	fmt.Println(header(fmt.Sprintf("merge %s → %s", src, root)))
	span := "no transcripts"
	if r.FirstTS != "" {
		span = r.FirstTS[:min(10, len(r.FirstTS))] + " → " + r.LastTS[:min(10, len(r.LastTS))]
	}
	fmt.Printf("  incoming      %s · schema v%d", span, r.IncomingSchema)
	if len(r.Cwds) > 0 {
		fmt.Printf(" · mostly in %s", strings.Join(r.Cwds, ", "))
	}
	fmt.Println()

	s := r.Sessions
	fmt.Printf("  sessions      %d new · %d already here · %d longer there (%s appended) · %d longer here\n",
		s.New, s.Equal, s.LongerThere, pluralize(s.Appended, "turn", "turns"), s.LongerHere)
	for _, d := range s.Diverged {
		fmt.Printf("                %s %s differs from yours — yours keeps the id, theirs arrives as %s\n", warnGlyph(), d.From, d.To)
	}
	if s.Duplicates > 0 {
		fmt.Printf("                %d skipped: a second copy of a transcript already here\n", s.Duplicates)
	}
	o := r.Observations
	fmt.Printf("  observations  %d reviewed there · %d to review · %d already here", o.Reviewed, o.Unreviewed, o.AlreadyHere)
	if o.SharedMined > 0 {
		fmt.Printf(" · %d skipped (sessions you already mined)", o.SharedMined)
	}
	if o.Orphaned > 0 {
		fmt.Printf(" · %d skipped (yours will be mined instead)", o.Orphaned)
	}
	fmt.Println()
	if r.Staged > 0 {
		fmt.Printf("  staged        %d recorded in-session, not yet distilled\n", r.Staged)
	}
	fc := r.Facets
	fmt.Printf("  facets        %d incoming: %d new · %d already here · %d updated · %d with later history adopted\n",
		fc.Incoming, fc.New, fc.Shared, fc.Updated, fc.FastForwarded)
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
	fmt.Println("  config.toml   yours is kept")
	fmt.Println()
}

// printMergeDone reports the applied merge and how to undo it.
func printMergeDone(root string, r *store.MergeReport, f *mergeFiles, backup string) {
	for _, line := range f.results {
		fmt.Printf("  lens          %s\n", line)
	}
	glyph := okGlyph()
	if len(f.failed) > 0 {
		glyph = warnGlyph()
	}
	fmt.Printf("%s merged: %d sessions, %d observations, %d facets new here.\n", glyph,
		r.Sessions.New, r.Observations.Reviewed+r.Observations.Unreviewed, r.Facets.New)
	fmt.Printf("  this archive as it was before: %s\n", backup)
	fmt.Printf("  to undo: stop witness, then replace %s with that directory\n", root)
	fmt.Println(dim("  nothing here called a model. Optionally, `witness compact` merges duplicates the two"))
	fmt.Println(dim("  archives wrote under different keys (that one does call the model)."))
}

func printMergeFiles(root string, f *mergeFiles) {
	for _, l := range f.register {
		fmt.Printf("  lens          %s: will be registered, NOT enabled — enable it with `witness lens enable %s`\n", l.name, l.name)
	}
	for _, l := range f.differ {
		fmt.Printf("  lens          %s %s: their definition differs — yours is kept (theirs: witness lens register %s %q)\n",
			warnGlyph(), l.name, l.name, l.path)
	}
	for _, n := range f.refused {
		fmt.Printf("  lens          %s %s: would be refused here, skipped\n", warnGlyph(), n)
	}
	for _, n := range f.unknown {
		fmt.Printf("  lens          %s %s: its data arrives, but there is no definition for it here — nothing will review\n"+
			"                  it until you `witness lens register %s <dir>` and enable it\n", warnGlyph(), n, n)
	}
	for _, n := range f.disabled {
		fmt.Printf("  lens          %s: data arrives for a lens you have registered but not enabled — `witness lens enable %s` to review it\n", n, n)
	}
	for _, src := range f.summarize {
		dst := filepath.Join(root, "summarize", filepath.Base(src))
		note := ""
		if _, err := os.Stat(dst); err == nil {
			note = " (this REPLACES yours — copy it aside first)"
		}
		fmt.Printf("  summarize     %s not copied — it changes how your portrait is written. To use it%s:\n"+
			"                  mkdir -p %q && cp %q %q\n", filepath.Base(src), note, filepath.Dir(dst), src, dst)
	}
	if f.profile {
		fmt.Printf("  profile       not merged — rebuilt from the merged facets; theirs stays in %s\n", filepath.Join(f.srcDir, "profile"))
	}
}
