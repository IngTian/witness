package commands

import (
	"crypto/sha1"
	"fmt"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/store"
	"github.com/IngTian/witness/internal/vector"
	"github.com/spf13/cobra"
)

const maxCLIStagedPerSession = 200

func newObservationsCmd() *cobra.Command {
	obsCmd := &cobra.Command{
		Use:   "observations",
		Short: "Search, record, or delete observations.",
		Long:  "Search, record, or delete L1 observations. These commands mirror the MCP search_observations, record_observation, and delete_observation tools.",
	}

	var searchLens string
	var searchK int
	var searchJSON bool
	search := &cobra.Command{
		Use:   "search <query>",
		Short: "Search observations by meaning.",
		Long:  "Search L1 observations by local embedding similarity. This is the CLI equivalent of the MCP search_observations tool.",
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
		Use:   "record --session <id> --dimension <name> --observation <text>",
		Short: "Record an active observation for a session.",
		Long:  "Stage an active observation for the worker to append to L1, matching the MCP record_observation behavior.",
		Args:  cobra.NoArgs,
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
		Use:   "delete <obs_id>",
		Short: "Delete one observation.",
		Long:  "Delete one L1 observation by obs_id. This is the CLI equivalent of the MCP delete_observation tool.",
		Args:  cobra.ExactArgs(1),
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
	hits := vector.Search(obs, qv, lensName, k)
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
	renderObservationHits(lensName, hits)
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

func renderObservationHits(lensName string, hits []vector.Hit) {
	if len(hits) == 0 {
		fmt.Printf("No observations found in the %q lens.\n", lensName)
		return
	}
	fmt.Printf("Growth observations (%s lens), most relevant first:\n\n", lensName)
	for _, h := range hits {
		o := h.Obs
		fmt.Printf("- [%s | %s | score %.3f] %s\n", valueOrNever(o.TS), o.Dimension, h.Score, o.Observation)
		if o.Evidence != "" {
			fmt.Printf("  evidence: %s\n", o.Evidence)
		}
		fmt.Printf("  id: %s\n", o.ID)
		if o.Session != "" {
			fmt.Printf("  session: %s\n", o.Session)
		}
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
	inserted, err := st.StageObservationCapped(o, maxCLIStagedPerSession)
	if err != nil {
		return err
	}
	if !inserted {
		if st.StagedCount(session) >= maxCLIStagedPerSession {
			return fmt.Errorf("too many staged observations for session %s (limit %d)", session, maxCLIStagedPerSession)
		}
		fmt.Printf("already recorded (%s/%s) id=%s\n", lensName, dimension, o.ID)
		return nil
	}
	spawnDetached("worker")
	fmt.Printf("recorded (%s/%s) id=%s\n", lensName, dimension, o.ID)
	fmt.Println("distill worker kicked in the background")
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
		fmt.Println("no observation with id " + obsID)
		return nil
	}
	fmt.Println("deleted " + obsID)
	return nil
}

func cliObsID(session, lensName, text string) string {
	h := sha1.Sum([]byte(session + "|" + lensName + "|" + text))
	return "obs_" + fmt.Sprintf("%x", h[:6])
}
