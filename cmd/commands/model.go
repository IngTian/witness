package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/IngTian/witness/internal/distill"
	"github.com/IngTian/witness/internal/embed"
	"github.com/IngTian/witness/internal/model"
	"github.com/spf13/cobra"
)

// newModelCmd is `witness model fetch`, the Go downloader for the embedding model. It replaced
// scripts/fetch-model.sh, so install.sh, `make fetch-model` and package-windows call it, and it
// is the remedy every missing-model message names — including on npm, which never shipped the
// script.
func newModelCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "model",
		GroupID: groupSetup,
		Short:   "Download the local embedding model.",
		Args:    cobra.NoArgs,
	}
	c.AddCommand(&cobra.Command{
		Use:   "fetch",
		Short: "Download the embedding model (~470MB, once; checked by SHA-256).",
		Long: strings.TrimSpace(`
Download multilingual-e5-small (model.onnx + tokenizer.json) into the directory witness
reads it from, pinned to one revision and checked by SHA-256. Files already right are
checked, not downloaded again, so re-running is cheap and repairs a damaged copy.

  WITNESS_ASSETS            download into this directory instead
  WITNESS_MODEL_BASE_URL    a mirror; needs WITNESS_MODEL_SHA256 and
                            WITNESS_TOKENIZER_SHA256 for the files it serves`),
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return cmdModelFetch() },
	})
	return c
}

func cmdModelFetch() error {
	dir, err := filepath.Abs(embed.AssetsDir())
	if err != nil {
		return err
	}
	fmt.Println(title("model fetch", tildePath(dir), "multilingual-e5-small @ "+model.Revision[:7]))
	// Ctrl-C cancels the download, so its lock and temp file are cleaned up rather than reaped later.
	ctx, stop := procCtl.NotifyStop(context.Background())
	defer stop()
	p := startProgress()
	err = model.Fetch(ctx, dir, func(name string, done, total int64) {
		p.update(distill.Progress{Where: name, Stages: []distill.Stage{{Name: "MB", Done: int(done >> 20), Total: int(max(total, 0) >> 20)}}})
	})
	p.finish()
	if errors.Is(err, model.ErrBusy) {
		return fmt.Errorf("%w; run `witness model fetch` again once it finishes", err)
	}
	if err != nil {
		return fmt.Errorf("model fetch: %w", err)
	}
	fmt.Println(okGlyph() + " embedding model ready")
	return nil
}

// lazyModel reports whether `witness mcp` must start without the model. Under
// `npx @witness-ai/opencode mcp` (WITNESS_NPM_PACKAGE=1, set only by the npm launcher, never by
// the plugin's MCP entry) nothing else downloads it, so a missing model used to stop the server
// outright. Everywhere else, and whenever the model is present, the server is unchanged.
func lazyModel() bool {
	return os.Getenv("WITNESS_NPM_PACKAGE") == "1" && !embed.ModelReady()
}

// startLazyEmbedder fetches the model in the background and returns an embedder that answers
// "still downloading" until it lands. Only search embeds, so every other tool works from the
// first call.
func startLazyEmbedder() *lazyEmbedder {
	l := &lazyEmbedder{dir: embed.AssetsDir()}
	if os.Getenv(model.SkipEnv) == "1" {
		l.err = fmt.Errorf("%s=1 turns the download off", model.SkipEnv)
		return l
	}
	slog.Info("mcp: the embedding model is missing; downloading it in the background", "dir", l.dir)
	go l.fetch()
	return l
}

// lazyEmbedder loads the model on the first search after it is in place — whoever put it there:
// this server's fetch, the plugin's downloader, or `witness model fetch`.
type lazyEmbedder struct {
	dir string
	mu  sync.Mutex
	emb *embed.Embedder
	err error // why the background fetch gave up, once it has
}

func (l *lazyEmbedder) fetch() {
	err := model.Fetch(context.Background(), l.dir, nil)
	if errors.Is(err, model.ErrBusy) {
		return // another downloader is on it; Embed sees its files when they land
	}
	if err != nil {
		slog.Error("mcp: embedding model download failed", "dir", l.dir, "err", err)
		l.mu.Lock()
		l.err = err
		l.mu.Unlock()
		return
	}
	slog.Info("mcp: embedding model ready", "dir", l.dir)
}

func (l *lazyEmbedder) Embed(text string) ([]float32, error) {
	l.mu.Lock()
	if l.emb == nil {
		if !embed.ModelReady() {
			err := l.err
			l.mu.Unlock()
			if err != nil {
				return nil, fmt.Errorf("the embedding model is not in %s (%v); run `npx @witness-ai/opencode model fetch`", l.dir, err)
			}
			return nil, fmt.Errorf("the embedding model is still downloading (~470MB, once) into %s; search works when it finishes", l.dir)
		}
		emb, err := embed.New()
		if err != nil {
			l.mu.Unlock()
			return nil, err
		}
		l.emb = emb
	}
	emb := l.emb
	l.mu.Unlock()
	return emb.Embed(text)
}
