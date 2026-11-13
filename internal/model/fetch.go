// Package model downloads the embedding model (multilingual-e5-small: model.onnx +
// tokenizer.json) into the directory embed reads it from.
//
// witness is one static binary, but the 470MB model is too large to ride along on every
// channel: a source checkout fetches it once (`witness model fetch`, which install.sh and the
// Makefile call), and the npm package ships without it: its OpenCode plugin runs `witness model
// fetch` at startup, and a `witness mcp` started by npm fetches in the background. This is the
// only downloader (#199 F127).
//
// The on-disk protocol is still that of the JS downloader the npm plugin had up to v0.13.0
// (npm/opencode/bin/model.js), so an older plugin sharing the model directory cannot collide with
// this one: the same `.download.lock` body ("<token> <pid> <hostname>", reaped when its same-host
// owner is dead or it is 12h old), and the same per-pid `<file>.<pid>.part` temp names, so each
// side's crash leftovers are reaped by the other.
package model

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/proc"
)

// The pinned HuggingFace commit and its git-LFS oids. Pinning the revision (not `main`) keeps
// the hashes valid the day upstream republishes. These are one face of embed.Dim's unstamped
// embedding contract: changing them means re-embedding every stored observation.
const (
	Revision        = "614241f622f53c4eeff9890bdc4f31cfecc418b3"
	ModelSHA256     = "ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665"
	TokenizerSHA256 = "0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39"
	defaultBaseURL  = "https://huggingface.co/intfloat/multilingual-e5-small/resolve/" + Revision
)

// SkipEnv=1 turns off every AUTOMATIC download (the airgapped / metered / bring-your-own-model
// switch, shared with model.js). Fetch does not read it: `witness model fetch` is an explicit
// request, as model.js's own CLI is.
const SkipEnv = "WITNESS_SKIP_MODEL_DOWNLOAD"

// ErrBusy means another process holds the download lock and is alive. Its files land
// atomically, so a caller waiting for the model just checks again later.
var ErrBusy = errors.New("another process is downloading the embedding model")

var errStalled = errors.New("download stalled")

const (
	lockName       = ".download.lock"
	lockStaleAfter = 12 * time.Hour
)

// stallTimeout cancels a download that receives no bytes for this long (model.js's default),
// so a dead connection fails instead of hanging. A var so tests can shorten it.
var stallTimeout = 30 * time.Second

type file struct{ remote, name, sha string }

// source is where the files come from: HuggingFace at the pinned revision, or a mirror
// (WITNESS_MODEL_BASE_URL), which must name both hashes — a mirror is trusted only as far as
// the hashes the user gave for it.
func source() (base string, files []file, err error) {
	base, model, tok := defaultBaseURL, ModelSHA256, TokenizerSHA256
	if b := os.Getenv("WITNESS_MODEL_BASE_URL"); b != "" {
		base = b
		model = strings.ToLower(strings.TrimSpace(os.Getenv("WITNESS_MODEL_SHA256")))
		tok = strings.ToLower(strings.TrimSpace(os.Getenv("WITNESS_TOKENIZER_SHA256")))
		if model == "" || tok == "" {
			return "", nil, errors.New("custom WITNESS_MODEL_BASE_URL requires WITNESS_MODEL_SHA256 and WITNESS_TOKENIZER_SHA256")
		}
	}
	return strings.TrimRight(base, "/"), []file{
		{"onnx/model.onnx", "model.onnx", model},
		{"tokenizer.json", "tokenizer.json", tok},
	}, nil
}

// Fetch makes model.onnx and tokenizer.json in dir match their pinned SHA-256, downloading any
// that do not. A file already right is hashed, not downloaded. progress, when non-nil, is called
// as bytes arrive (total is -1 when the server does not say).
func Fetch(ctx context.Context, dir string, progress func(name string, done, total int64)) error {
	base, files, err := source()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	release, err := lock(dir)
	if err != nil {
		return err
	}
	defer release()
	reapParts(dir)
	for _, f := range files {
		dst := filepath.Join(dir, f.name)
		if got, err := hashFile(dst); err == nil {
			if got == f.sha {
				continue
			}
			// A wrong file must not survive a failed download: embed.ModelReady checks size only.
			_ = os.Remove(dst)
		}
		if err := download(ctx, base+"/"+f.remote, dst, f.sha, progress); err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
	}
	return nil
}

func download(ctx context.Context, url, dst, want string, progress func(string, int64, int64)) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// net/http returns this cause as the request error, so a stall reads as one, not as "canceled".
	stall := time.AfterFunc(stallTimeout, func() { cancel(fmt.Errorf("%w: no bytes for %s", errStalled, stallTimeout)) })
	defer stall.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	tmp := fmt.Sprintf("%s.%d.part", dst, os.Getpid())
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	c := &counter{name: filepath.Base(dst), total: resp.ContentLength, progress: progress, stall: stall}
	_, err = io.Copy(io.MultiWriter(out, h, c), resp.Body)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if got := hex.EncodeToString(h.Sum(nil)); got != want {
			err = fmt.Errorf("sha256 mismatch: got %s, want %s (corrupted or tampered download)", got, want)
		}
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// counter reports progress and keeps the stall timer from firing while bytes arrive.
type counter struct {
	name     string
	done     int64
	total    int64
	progress func(string, int64, int64)
	stall    *time.Timer
}

func (c *counter) Write(p []byte) (int, error) {
	c.stall.Reset(stallTimeout)
	c.done += int64(len(p))
	if c.progress != nil {
		c.progress(c.name, c.done, c.total)
	}
	return len(p), nil
}

// lock takes dir's download lock in model.js's format, or returns ErrBusy. A lock whose owner
// is dead on this host, or that is older than 12h (another host, or an unreadable body), is
// stale and taken over: the stdio MCP server is routinely killed without running cleanup, so
// a lock outliving its owner is the normal case, not an edge.
func lock(dir string) (release func(), err error) {
	path := filepath.Join(dir, lockName)
	host, _ := os.Hostname()
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			if !lockStale(path, host) {
				return nil, ErrBusy
			}
			_ = os.Remove(path)
			continue
		}
		if err != nil {
			return nil, err
		}
		_, err = fmt.Fprintf(f, "%s %d %s\n", token, os.Getpid(), host)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(path)
			return nil, err
		}
		return func() {
			// Only ours: a stale-lock reap may have handed the slot to another downloader.
			if body, err := os.ReadFile(path); err == nil && lockField(body, 0) == token {
				_ = os.Remove(path)
			}
		}, nil
	}
	return nil, ErrBusy
}

func lockStale(path, host string) bool {
	if body, err := os.ReadFile(path); err == nil && lockField(body, 2) == host {
		// Our own pid can only be a dead predecessor's (a restarted container reuses pids), since
		// one process never runs two downloads. pid 1 is left to the 12h rule, as model.js leaves
		// it: proc.Alive calls it dead, but in a container it is the live plugin host.
		if pid, err := strconv.Atoi(lockField(body, 1)); err == nil && (pid == os.Getpid() || pid > 1 && !proc.Alive(pid)) {
			return true
		}
	}
	fi, err := os.Stat(path)
	return err == nil && time.Since(fi.ModTime()) > lockStaleAfter
}

func lockField(body []byte, i int) string {
	if f := strings.Fields(string(body)); i < len(f) {
		return f[i]
	}
	return ""
}

// reapParts removes temp files left by other pids. Safe only under the lock: we are then the
// sole legitimate writer, so any part not named for our pid is a crashed owner's (either
// downloader's: model.js uses the same naming).
func reapParts(dir string) {
	mine := fmt.Sprintf(".%d.part", os.Getpid())
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".part") && !strings.HasSuffix(n, mine) {
			_ = os.Remove(filepath.Join(dir, n))
		}
	}
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
