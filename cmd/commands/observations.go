package commands

import (
	"crypto/sha1"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/store"
	"github.com/IngTian/witness/internal/vector"
	"github.com/spf13/cobra"
)

const (
	maxCLIStagedPerSession = 200
	// maxCLIStagedTotal mirrors the MCP server's global bound so both writers into `staged`
	// share one ceiling on the buffer, rather than each policing only its own sessions.
	maxCLIStagedTotal = 5000
)

func newObservationsCmd() *cobra.Command {
	obsCmd := &cobra.Command{
		Use:     "observations",
		GroupID: groupRead,
		Short:   "Search what witness has noticed about you.",
		Long:    "Search the specific things witness has noticed about your work patterns, preferences, and growth. Each observation is timestamped and organized by lens and dimension.",
		RunE:    groupRunE, // a typo'd subcommand is an error, not its help page and exit 0
	}

	var searchLens string
	var searchK int
	var searchJSON bool
	search := &cobra.Command{
		Use:   "search <query>",
		Short: "Search what witness has noticed, by meaning.",
		Long:  "Search the things witness has observed about you, finding semantically similar observations via local embedding search.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdObservationSearch(args[0], searchLens, searchK, searchJSON)
		},
	}
	search.Flags().StringVar(&searchLens, "lens", store.LensDefault, "lens to search")
	search.Flags().IntVarP(&searchK, "limit", "k", 8, "max results")
	search.Flags().BoolVarP(&searchJSON, "json", "j", false, "output as JSON")

	var recordSession, recordLens, recordDimension, recordObservation, recordEvidence string
	var recordPoignancy int
	record := &cobra.Command{
		Use:    "record --session <id> --dimension <name> --observation <text>",
		Short:  "Record an active observation for a session.",
		Long:   "Stage an active observation for the worker to append to L1, matching the MCP record_observation behavior.",
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return cmdObservationRecord(recordSession, recordLens, recordDimension, recordObservation, recordEvidence, recordPoignancy)
		},
	}
	record.Flags().StringVar(&recordSession, "session", "", "session id to attach the observation to")
	record.Flags().StringVar(&recordLens, "lens", store.LensDefault, "lens tag")
	record.Flags().StringVar(&recordDimension, "dimension", "", "dimension within the lens")
	record.Flags().StringVar(&recordObservation, "observation", "", "one-sentence observation")
	record.Flags().StringVar(&recordEvidence, "evidence", "", "short evidence anchor")
	record.Flags().IntVar(&recordPoignancy, "poignancy", 5, "salience from 1 to 10")

	deleteCmd := &cobra.Command{
		Use:    "delete <obs_id>",
		Short:  "Delete one observation.",
		Long:   "Delete one L1 observation by obs_id. This is the CLI equivalent of the MCP delete_observation tool.",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(_ *cobra.Command, args []string) error {
			return cmdObservationDelete(args[0])
		},
	}

	obsCmd.AddCommand(search, record, deleteCmd)
	return obsCmd
}

func cmdObservationSearch(query, lensName string, k int, asJSON bool) error {
	query = strings.TrimSpace(query)
	if query == "" {
		return fmt.Errorf("query is required")
	}
	lensName = strings.TrimSpace(lensName)
	if lensName == "" {
		lensName = store.LensDefault
	}
	if k <= 0 {
		k = 8
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	obs, err := st.ReadObservations("")
	if err != nil {
		return err
	}
	emb, err := embed.New()
	if err != nil {
		return err
	}
	qv, err := emb.Embed(query)
	if err != nil {
		return err
	}
	// Observations carry the lens's mined name, which a registered lens may override.
	dataName := lensDataName(st, lensName)
	hits := vector.Search(obs, qv, dataName, k)
	if asJSON {
		out := observationSearchJSON{Lens: lensName, Query: query, Limit: k, Hits: []observationHitJSON{}}
		for _, h := range hits {
			o := h.Obs
			out.Hits = append(out.Hits, observationHitJSON{
				ID:          o.ID,
				TS:          o.TS,
				Session:     o.Session,
				Lens:        o.Lens,
				Dimension:   o.Dimension,
				Observation: o.Observation,
				Evidence:    o.Evidence,
				Poignancy:   o.Poignancy,
				Score:       h.Score,
			})
		}
		return emitJSON(out)
	}
	renderObservationHits(lensName, query, hits,
		slices.Contains(st.RegisteredLenses(), lensName), slices.Contains(st.LoadConfig().EnabledLenses, lensName),
		func() store.EmptyReason { return st.WhyEmpty(dataName) })
	return nil
}

type observationSearchJSON struct {
	Lens  string               `json:"lens"`
	Query string               `json:"query"`
	Limit int                  `json:"limit"`
	Hits  []observationHitJSON `json:"hits"`
}

type observationHitJSON struct {
	ID          string  `json:"id"`
	TS          string  `json:"ts"`
	Session     string  `json:"session,omitempty"`
	Lens        string  `json:"lens"`
	Dimension   string  `json:"dimension"`
	Observation string  `json:"observation"`
	Evidence    string  `json:"evidence,omitempty"`
	Poignancy   int     `json:"poignancy"`
	Score       float64 `json:"score"`
}

// renderObservationHits prints the hits most relevant first. lensName is what the user typed;
// registered/enabled decide which next step an empty result can honestly suggest.
func renderObservationHits(lensName, query string, hits []vector.Hit, registered, enabled bool, whyEmpty func() store.EmptyReason) {
	if len(hits) == 0 {
		fmt.Println(title("observations", lensName, fmt.Sprintf("no hits for %q", query)))
		fmt.Println()
		// vector.Search is top-k with no score threshold, so zero hits never means "your
		// query missed" — it means there is nothing embedded to search. Say which layer is
		// actually missing instead of leaving the user to guess (#166). An unregistered lens
		// comes first: every other step (backfill, doctor) would fail or mislead for it.
		reason := store.NoCapture
		if registered {
			reason = whyEmpty()
		}
		switch {
		case !registered:
			fmt.Printf("  %s no lens %q is registered\n", skipGlyph(), lensName)
			fmt.Println(hint("see lenses", "witness lens list"))
		case reason == store.NoCapture:
			fmt.Printf("  %s nothing to search — nothing is capturing (the archive holds no raw turns)\n", skipGlyph())
			fmt.Println(hint("wire editor", wireCommand()))
		case reason == store.NoObservations:
			fmt.Printf("  %s nothing has been mined into %q yet\n", skipGlyph(), lensName)
			if !enabled {
				fmt.Println(hint("enable it", "witness lens enable "+lensName))
			}
			fmt.Println(hint("mine history", "witness lens backfill "+lensName))
		default:
			fmt.Printf("  %s %q has observations, but none carry embeddings, so nothing is searchable\n", warnGlyph(), lensName)
			fmt.Println(hint("diagnose", "witness doctor"))
		}
		return
	}
	fmt.Println(title("observations", lensName, fmt.Sprintf("%s for %q", countNoun(len(hits), "hit", "hits"), query)))
	for _, h := range hits {
		o := h.Obs
		// 2 indent + 5 score + 2 gap = the 9-column hanging indent every line of a hit shares.
		fmt.Println()
		fmt.Printf("  %s  %s\n", bold(cyan(fmt.Sprintf("%.3f", h.Score))), dim(o.Dimension+" · "+shortTime(valueOrNever(o.TS))))
		fmt.Printf("         %s\n", fitLine(o.Observation, 9))
		if o.Evidence != "" {
			fmt.Printf("         %s\n", dim(glyph("↳", "->")+" "+fitLine(o.Evidence, 11)))
		}
		ids := o.ID
		if o.Session != "" {
			ids += " · session " + o.Session
		}
		fmt.Printf("         %s\n", dim(ids))
	}
}

func cmdObservationRecord(session, lensName, dimension, observation, evidence string, poignancy int) error {
	session = strings.TrimSpace(session)
	lensName = strings.TrimSpace(lensName)
	dimension = strings.TrimSpace(dimension)
	observation = strings.TrimSpace(observation)
	evidence = strings.TrimSpace(evidence)
	if lensName == "" {
		lensName = store.LensDefault
	}
	if session == "" || observation == "" {
		return fmt.Errorf("--session and --observation are required")
	}
	if dimension == "" {
		return fmt.Errorf("--dimension is required")
	}
	if poignancy < 1 {
		poignancy = 5
	}
	if poignancy > 10 {
		poignancy = 10
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	o := store.Observation{
		ID:          cliObsID(session, lensName, observation),
		TS:          time.Now().UTC().Format(time.RFC3339),
		Session:     session,
		Lens:        lensName,
		Dimension:   dimension,
		Observation: observation,
		Evidence:    evidence,
		Poignancy:   poignancy,
		Source:      "active",
	}
	// The CLI path is human-driven (one `witness observations record` per invocation), so the
	// agent-rotates-the-session-id evasion the MCP path guards against does not apply here.
	// It still passes the same TOTAL cap so both writers share one bound on the staged buffer.
	inserted, err := st.StageObservationCapped(o, maxCLIStagedPerSession, maxCLIStagedTotal)
	if err != nil {
		return err
	}
	if !inserted {
		// Duplicate vs. cap: check the dedup case first so a re-recorded obs at the
		// cap isn't mislabeled a quota error (mirrors the MCP path).
		if st.StagedExists(session, o.ID) {
			fmt.Println(result(skipGlyph(), fmt.Sprintf("already recorded %s/%s %s", lensName, dimension, o.ID), "nothing staged"))
			return nil
		}
		return fmt.Errorf("too many staged observations for session %s (limit %d)", session, maxCLIStagedPerSession)
	}
	if !spawnDetachedOK("worker-run") {
		// Staged is durable — the next capture or `worker run` folds it in — so this is a warning.
		fmt.Println(result(warnGlyph(), fmt.Sprintf("recorded %s/%s %s", lensName, dimension, o.ID), "the worker did not start"))
		fmt.Println(hint("fold it in", "witness worker run"))
		return nil
	}
	fmt.Println(result(okGlyph(), fmt.Sprintf("recorded %s/%s %s", lensName, dimension, o.ID), "worker started in the background"))
	return nil
}

func cmdObservationDelete(obsID string) error {
	obsID = strings.TrimSpace(obsID)
	if obsID == "" {
		return fmt.Errorf("obs_id is required")
	}
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	deleted, err := st.DeleteObservation(obsID)
	if err != nil {
		return err
	}
	if !deleted {
		// Exit 0 on purpose: deleting what is already gone is idempotent, the MCP tool's contract too.
		fmt.Println(result(skipGlyph(), "no observation "+obsID, "nothing deleted"))
		return nil
	}
	fmt.Println(result(okGlyph(), "deleted "+obsID, ""))
	return nil
}

func cliObsID(session, lensName, text string) string {
	h := sha1.Sum([]byte(session + "|" + lensName + "|" + text))
	return "obs_" + fmt.Sprintf("%x", h[:6])
}
