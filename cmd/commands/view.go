package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/store"
	"github.com/IngTian/witness/internal/view"
	"github.com/spf13/cobra"
)

// `witness view` writes the archive as one self-contained HTML page and opens it. It is a
// reader in the strict sense: the only thing it writes is that page. It never takes the
// WorkerLock (a running drain must not delay or block it) and never calls a model — a portrait
// `witness profile` would rewrite is marked out of date on the page, not refreshed.

// viewWarnSize is the size past which a browser gets slow to open the page; the 75k-note
// archive renders to about 8 MiB. A var only so a test can trip it without writing 50 MiB.
var viewWarnSize = 50 << 20

// openInBrowser opens a file in the default browser. Inert under go test, because no test may
// launch a browser; a test that needs to see the call installs its own fake here.
var openInBrowser = func(path string) error {
	if underTest() {
		return nil
	}
	return openPath(path)
}

func newViewCmd() *cobra.Command {
	var noOpen bool
	var out string
	c := &cobra.Command{
		Use:     "view",
		GroupID: groupRead,
		Short:   "Open your archive as a page in the browser.",
		Long: "Write the archive as one self-contained HTML page and open it in your browser.\n\n" +
			"The page is a snapshot of this moment; run the command again to refresh it. It is " +
			"view.html in the data directory unless --out names another path. Nothing in the " +
			"archive changes and no model is called: a profile that is out of date is marked on " +
			"the page, and `witness profile` refreshes it.\n\n" +
			"The page includes the observations behind each facet, so keep it as private as the " +
			"archive itself.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return cmdView(noOpen, out)
		},
	}
	c.Flags().BoolVar(&noOpen, "no-open", false, "write the page without opening a browser")
	c.Flags().StringVarP(&out, "out", "o", "", "write the page to this path instead of <data dir>/view.html")
	return c
}

func cmdView(noOpen bool, out string) error {
	st, err := store.Open()
	if err != nil {
		return err
	}
	defer st.Close()
	in, err := st.ReadViewInput(context.Background())
	if err != nil {
		return err
	}
	portraits, err := viewPortraits(st)
	if err != nil {
		return err
	}
	page, err := view.Render(view.Build(in, portraits, time.Now(), time.Local))
	if err != nil {
		return err
	}
	if out == "" {
		out = filepath.Join(st.Root, "view.html")
	}
	path, err := filepath.Abs(out)
	if err != nil {
		return err
	}
	// Atomic and 0600 even over an existing file: the page holds the archive's own notes.
	if err := store.WriteFileAtomic(path, page); err != nil {
		return err
	}

	fmt.Println(title("view", "", ""))
	fmt.Println()
	fmt.Println("  " + kvRow("file", tildePath(path), ""))
	fmt.Println("  " + kvRow("size", humanBytes(int64(len(page))), ""))
	if len(page) > viewWarnSize {
		fmt.Println(warnGlyph() + " a large page — a browser may be slow to open it")
	}
	if noOpen {
		fmt.Println(hint("open it", fileURL(path)))
		return nil
	}
	reportOpen(path, openInBrowser(path))
	return nil
}

// viewPortraits reads every portrait in profile/ as the page draws it. Stale is Summarize's own
// skip test, from the Summarizer `witness profile` uses; when it cannot be computed every
// portrait reads as current, because a reader must still render.
func viewPortraits(st *store.Store) ([]view.PortraitFile, error) {
	ids, err := st.ListProfiles()
	if err != nil {
		return nil, err
	}
	var stale map[string]bool
	sm, err := profileSummarizer(st)
	if err == nil && sm != nil {
		stale, err = sm.Stale()
	}
	if err != nil {
		slog.Debug("view: could not tell which portraits are out of date; showing all as current", "err", err)
	}
	var out []view.PortraitFile
	for _, id := range ids {
		md, ok, err := st.ReadProfile(id)
		if err != nil {
			return nil, err
		}
		info, serr := os.Stat(filepath.Join(st.ProfileDir(), id+".md"))
		if !ok || os.IsNotExist(serr) {
			continue // removed since it was listed: a refresh or a reap got there first
		}
		if serr != nil {
			return nil, serr
		}
		src, srcOK, err := st.CurrentPortraitSources(id)
		if err != nil {
			return nil, err
		}
		out = append(out, view.PortraitFile{ID: id, Markdown: md, Written: info.ModTime(),
			Stale: stale[id], Sources: src, SourcesOK: srcOK})
	}
	return out, nil
}

// reportOpen ends the screen with how the open went. Any failure means "here is the path": the
// cause, then the page as a file URL, which a terminal can open and any browser's address bar
// takes.
func reportOpen(path string, err error) {
	if err != nil {
		fmt.Println(warnGlyph() + " could not open a browser: " + err.Error())
		fmt.Println(hint("open it", fileURL(path)))
		return
	}
	fmt.Println(okGlyph() + " opening it in your browser")
}

// fileURL is path as a file:// URL: forward slashes, a leading slash before a Windows drive
// letter (file:///C:/…), spaces and the like escaped.
func fileURL(path string) string {
	return (&url.URL{Scheme: "file", Path: "/" + strings.TrimPrefix(filepath.ToSlash(path), "/")}).String()
}
