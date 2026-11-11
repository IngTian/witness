package commands

import (
	"fmt"
	"slices"
	"strings"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
	"github.com/spf13/cobra"
)

func newFacetsCmd() *cobra.Command {
	var asJSON, all bool
	var expand string
	c := &cobra.Command{
		Use:     "facets [lens]",
		GroupID: groupRead,
		Short:   "Show the specific traits and patterns behind your profile.",
		Long: "Print the structured traits and patterns — organized by dimension — that witness currently tracks about you. " +
			"These are the building blocks that feed the narrative profile. Defaults to the default lens.\n\n" +
			"Shows the ACTIVE profile. Facets consolidated by `witness compact` are archived rather than deleted: " +
			"pass --all to list them too, or --expand <dimension/key> to unfold one active facet into the full " +
			"trajectory of facets that were merged into it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if expand != "" {
				return cmdFacetTrajectory(args, expand, asJSON)
			}
			return cmdFacets(args, asJSON, all)
		},
	}
	c.Flags().BoolVarP(&asJSON, "json", "j", false, "output as JSON")
	c.Flags().BoolVar(&all, "all", false, "include archived facets (marked with where they went)")
	c.Flags().StringVar(&expand, "expand", "", "unfold one facet (dimension/key) into everything merged into it")
	return c
}

func cmdFacets(args []string, asJSON, all bool) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	lensName := store.LensDefault
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		lensName = strings.TrimSpace(args[0])
	}
	// Facets are tagged with the lens's MINED name (its lens.json `name`), which can differ from
	// the name it was registered under: a lens registered as corpus-example from a checkout whose
	// lens.json still said "regime" must find the facets the worker wrote under that name, not
	// report an empty lens.
	dataName := lens.DataName(st, lensName)
	// The live profile by default: an archived facet is history, and listing it beside the
	// active ones would misrepresent what witness currently claims about the person.
	read := st.ReadActiveFacets
	if all {
		read = st.ReadFacets
	}
	facets, err := read()
	if err != nil {
		return err
	}
	if asJSON {
		filtered := []store.Facet{}
		for _, f := range facets {
			if f.Lens == dataName && f.Current() != nil {
				filtered = append(filtered, f)
			}
		}
		return emitJSON(facetsJSON{Lens: lensName, Facets: filtered})
	}
	// Whether this lens is registered AND enabled decides the right empty-state message:
	// since #44 slice 1a "default" is an ordinary, DELETABLE lens, so an absent lens (the
	// user removed it, or typo'd the name) must not be blamed on "the reviewer hasn't run
	// yet" — and a registered-but-DISABLED lens won't be helped by `witness review` either
	// (review only iterates the enabled set), so that hint must point at `lens enable`.
	registered := slices.Contains(st.RegisteredLenses(), lensName)
	enabled := slices.Contains(st.LoadConfig().EnabledLenses, lensName)
	// The listing shows CURRENT confidence (decayed to now); --json above stays the stored
	// record. docs/how-it-works.md promises this view moves on its own as evidence fails to
	// arrive, and before this it never did.
	hw, err := st.ObservationHighWater()
	if err != nil {
		return err
	}
	// whyEmpty is a closure, not a value: it costs two indexed queries and is only needed
	// on the empty path, which is the uncommon one.
	renderCurrentFacets(lensName, dataName, facets, hw, registered, enabled, func() store.EmptyReason { return st.WhyEmpty(dataName) })
	return nil
}

type facetsJSON struct {
	Lens   string        `json:"lens"`
	Facets []store.Facet `json:"facets"`
}

type trajectoryJSON struct {
	Lens      string        `json:"lens"`
	Facet     store.Facet   `json:"facet"`
	MergedIn  []store.Facet `json:"merged_in"`
	TotalKept int           `json:"total_versions_kept"`
}

// cmdFacetTrajectory answers "how did this facet get here" — the expansion half of a compact
// profile. Compaction keeps every absorbed facet with its full history and a link to the
// survivor, so a consolidated node is never a dead end: this unfolds it into the facets that
// were merged into it, and the ones merged into those.
func cmdFacetTrajectory(args []string, ref string, asJSON bool) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	lensName := store.LensDefault
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		lensName = strings.TrimSpace(args[0])
	}
	dataName := lens.DataName(st, lensName)
	dimension, key, ok := store.ParseFacetRef(strings.TrimSpace(ref))
	if !ok {
		return fmt.Errorf("--expand wants a facet as dimension/key (e.g. thinking/zooms_out), got %q", ref)
	}

	all, err := st.ReadFacets()
	if err != nil {
		return err
	}
	var target *store.Facet
	for i := range all {
		if all[i].Lens == dataName && all[i].Dimension == dimension && all[i].Key == key {
			target = &all[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("no facet %s/%s in lens %q (see `witness facets %s --all`)", dimension, key, lensName, lensName)
	}
	tributaries, err := st.FacetTrajectory(dataName, dimension, key)
	if err != nil {
		return err
	}

	if asJSON {
		kept := len(target.Versions)
		for _, f := range tributaries {
			kept += len(f.Versions)
		}
		return emitJSON(trajectoryJSON{Lens: lensName, Facet: *target, MergedIn: tributaries, TotalKept: kept})
	}

	state := "active"
	if !target.Active() {
		state = "archived " + shortTime(target.ArchivedAt)
	}
	summary := state + " · " + countOf(len(target.Versions), "version", "versions")
	if len(tributaries) > 0 {
		summary += fmt.Sprintf(" · %d merged in", len(tributaries))
	}
	scope := lensName + " · " + store.FacetRef(dimension, key)
	if !fits(cols("witness facets · " + scope + "  " + summary)) {
		// A long key already fills the title line: the summary goes under it, not off the edge.
		fmt.Println(title("facets", scope, ""))
		fmt.Println("  " + dim(summary))
	} else {
		fmt.Println(title("facets", scope, summary))
	}
	fmt.Println()
	// --expand is how a facet is read IN FULL, so its value is never cut to the terminal — only
	// wrapped at its indent, where a soft wrap would have run it back to column 0.
	if cur := target.Current(); cur != nil {
		fmt.Printf("  %s\n", wrapHanging(cur.Value, 2))
	}
	if !target.Active() {
		// An archived facet is not the end of its own story: say where it went (or that it was
		// retired) and why — the reason is persisted exactly for this audit trail.
		fmt.Println()
		if target.ArchiveReason != "" {
			fmt.Printf("  %s %s\n", dim("why:"), target.ArchiveReason)
		}
		if target.MergedInto != "" {
			fmt.Println(hint("merged into", "witness facets "+lensName+" --expand "+target.MergedInto))
		} else {
			// Compaction and the review's deleted-evidence prune both retire outright; the why
			// line above says which.
			fmt.Println(dim("  retired — not merged into another facet"))
		}
	}

	if len(tributaries) == 0 {
		// Only for a live facet: an archived one's story went on in the facet it was merged into,
		// which the hint above already points at.
		if target.Active() {
			fmt.Println()
			fmt.Println(dim("  nothing was merged into this facet — its own versions are the whole story"))
		}
		return nil
	}
	for _, f := range tributaries {
		fmt.Println()
		ref := store.FacetRef(f.Dimension, f.Key)
		meta := fmt.Sprintf("archived %s · %s", shortTime(f.ArchivedAt), countOf(len(f.Versions), "version", "versions"))
		if fits(4 + cols(ref) + 2 + cols(meta)) {
			fmt.Printf("  %s %s  %s\n", dim(glyph("←", "<-")), ref, dim(meta))
		} else { // a long key keeps its whole name (it is what --expand takes); the meta drops a line
			fmt.Printf("  %s %s\n    %s\n", dim(glyph("←", "<-")), ref, dim(meta))
		}
		if f.ArchiveReason != "" {
			fmt.Printf("    %s %s\n", dim("why:"), f.ArchiveReason)
		}
		// The tributary's ORIGINAL value, whole: seeing what was folded in is the point.
		if cur := f.Current(); cur != nil {
			fmt.Printf("    %s\n", wrapHanging(cur.Value, 4))
		}
	}
	return nil
}

// renderCurrentFacets prints one lens's listing. name is what the user typed — the title and every
// suggested command use it; dataName is the name the facets are tagged with.
func renderCurrentFacets(name, dataName string, facets []store.Facet, decaySeq int64, registered, enabled bool, whyEmpty func() store.EmptyReason) {
	count, archived, perDim := 0, 0, map[string]int{}
	for _, f := range facets {
		if f.Lens == dataName && f.Current() != nil {
			count++
			perDim[f.Dimension]++
			if !f.Active() {
				archived++
			}
		}
	}
	if count == 0 {
		fmt.Println(title("facets", name, ""))
		fmt.Println()
		// Three distinct empty states, each with the ACCURATE next step — the deletable
		// default (#102) makes "absent" and "disabled" normal, not a stuck reviewer:
		switch {
		case !registered:
			fmt.Printf("  %s no lens %q is registered, so it has no facets\n", skipGlyph(), name)
			fmt.Println(hint("see lenses", "witness lens list"))
			if name == store.LensDefault {
				fmt.Println(hint("restore it", "witness lens load-default"))
			}
		case !enabled:
			// `witness worker review` only iterates the ENABLED set, so it would no-op here —
			// point at enable, not review.
			fmt.Printf("  %s lens %q is disabled, so it has no facets\n", skipGlyph(), name)
			fmt.Println(hint("enable it", "witness lens enable "+name))
			fmt.Println(hint("then mine it", "witness lens backfill "+name))
		default:
			// Registered AND enabled, so the lens itself is fine — the question is which
			// layer is missing. "The reviewer runs after enough observations accumulate" is
			// only true when observations EXIST; telling someone with no capture wired to
			// wait for the reviewer describes a fix that will never arrive (#166).
			switch whyEmpty() {
			case store.NoCapture:
				fmt.Printf("  %s no facets for %q — nothing is capturing (the archive holds no raw turns)\n", skipGlyph(), name)
				fmt.Println(hint("wire editor", wireCommand()))
			case store.NoObservations:
				fmt.Printf("  %s no facets for %q — capture works, but nothing has been mined into it yet\n", skipGlyph(), name)
				fmt.Println(hint("mine history", "witness lens backfill "+name))
			default:
				fmt.Printf("  %s no facets for %q yet — its observations have not been reviewed\n", skipGlyph(), name)
				fmt.Println(hint("review now", "witness worker review"))
			}
		}
		return
	}
	// The title says what is listed: under --all active and archived are counted apart, so the
	// number is never a silent mix of the two.
	summary := humanCount(count) + " active · " + countOf(len(perDim), "dimension", "dimensions")
	if archived > 0 {
		summary = fmt.Sprintf("%s facets · %s active · %s archived", humanCount(count), humanCount(count-archived), humanCount(archived))
	}
	fmt.Println(title("facets", name, summary))
	dimension := ""
	for _, f := range facets {
		if f.Lens != dataName {
			continue
		}
		cur := f.Current()
		if cur == nil {
			continue
		}
		if f.Dimension != dimension { // the read is ORDER BY dimension, so each group is contiguous
			dimension = f.Dimension
			fmt.Printf("\n  %s  %s\n", bold(dimension), dim(humanCount(perDim[dimension])))
		}
		meta := fmt.Sprintf("%d obs", len(cur.BecauseOf))
		if f.LastSeen != "" {
			meta = "seen " + shortTime(f.LastSeen) + " · " + meta
		}
		// 4 indent + 4 confidence + 2 gap put the key at column 10; it is cut, never the meta.
		fmt.Printf("    %s  %s  %s\n", confidence(cur.DecayedConfidence(decaySeq)),
			bold(fitLine(f.Key, 10+2+len([]rune(meta)))), dim(meta))
		// Only reachable under --all. An archived facet listed without saying so would read as
		// a live claim about the person, which is exactly what compaction decided it is not.
		if !f.Active() {
			where := "retired"
			if f.MergedInto != "" {
				where = "merged into " + f.MergedInto
			}
			fmt.Printf("          %s\n", yellow("archived — "+where))
		}
		// The gist (the short claim, once compaction wrote one), cut to the terminal; a pipe gets
		// it whole. --expand reads one facet in full.
		fmt.Printf("          %s\n", fitLine(cur.Gist(), 10))
	}
}

// wireCommand is the copyable "connect an editor" step, naming the editors wire really accepts.
func wireCommand() string {
	return "witness wire <" + strings.Join(editorNames(), "|") + ">"
}
