package opencode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/IngTian/witness/internal/platform"
)

// nativeCommand is injectable so tests prove the two DB environments without a
// real OpenCode binary. Production uses CommandContext through this small port.
var nativeCommand = func(ctx context.Context, env []string, args ...string) ([]byte, error) {
	return commandOutput(ctx, env, args...)
}

type nativeRuntime struct {
	root   string
	server *OpenCodeServer
	mu     sync.Mutex
}
type nativeManifest struct {
	Session, Lens, Input, Digest, Fork, Reply, MessageID string
	RawHigh                                              int64
	Total                                                int
	Committed                                            bool
}

func newNativeRuntime(root string, server *OpenCodeServer) *nativeRuntime {
	return &nativeRuntime{root: root, server: server}
}
func (n *nativeRuntime) dir() string { return filepath.Join(n.root, "opencode-native") }
func (n *nativeRuntime) path(w *platform.NativeSession) string {
	h := sha256.Sum256([]byte(w.Session + "\x00" + fmt.Sprint(w.RawHigh) + "\x00" + w.Lens + "\x00" + w.Input))
	return filepath.Join(n.dir(), fmt.Sprintf("%x.json", h[:]))
}
func (n *nativeRuntime) snapshot(p string) string {
	return strings.TrimSuffix(p, ".json") + ".snapshot.json"
}

func isolatedEnv(root string) []string {
	return []string{"XDG_DATA_HOME=" + filepath.Join(root, "xdg"), "OPENCODE_DB=" + filepath.Join(root, "opencode.db")}
}
func replaceEnv(base, updates []string) []string {
	m := map[string]string{}
	order := []string{}
	for _, e := range base {
		k, _, _ := strings.Cut(e, "=")
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = e
	}
	for _, e := range updates {
		k, _, _ := strings.Cut(e, "=")
		if _, ok := m[k]; !ok {
			order = append(order, k)
		}
		m[k] = e
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, m[k])
	}
	return out
}

func (n *nativeRuntime) run(ctx context.Context, w *platform.NativeSession, model, prompt, input string) (string, error) {
	n.mu.Lock()
	locked := true
	defer func() {
		if locked {
			n.mu.Unlock()
		}
	}()
	if err := os.MkdirAll(n.dir(), 0o700); err != nil {
		return "", err
	}
	p := n.path(w)
	m, err := n.load(p)
	if err != nil {
		return "", err
	}
	if m.Session == "" {
		m = nativeManifest{Session: w.Session, RawHigh: w.RawHigh, Total: w.Total, Lens: w.Lens, Input: w.Input, Digest: w.Digest}
	} else if m.Digest != w.Digest {
		return "", fmt.Errorf("opencode native retained manifest digest does not match L0 generation")
	}
	if m.Reply != "" {
		w.SetFinalizer(func() error { return n.finalize(p) })
		return m.Reply, nil
	}
	if m.Fork == "" {
		snap := n.snapshot(p)
		if _, err := os.Stat(snap); os.IsNotExist(err) {
			if err = n.export(ctx, w, snap); err != nil {
				return "", err
			}
		} else if err != nil {
			return "", err
		} else if w.Digest != "" {
			data, err := os.ReadFile(snap)
			if err != nil {
				return "", err
			}
			if err := validateExportDigest(data, w.Digest); err != nil {
				return "", err
			}
		}
		// A previous lens may have left its disposable pristine import behind.
		// This server is bound to the private DB, so deleting here can never touch
		// the user's source session.
		if err = n.server.deleteSession(ctx, strings.TrimPrefix(w.Session, SessionPrefix)); err != nil {
			return "", fmt.Errorf("clear isolated opencode source: %w", err)
		}
		if err = n.importSnapshot(ctx, snap); err != nil {
			return "", err
		}
		id, err := n.server.fork(ctx, strings.TrimPrefix(w.Session, SessionPrefix))
		if err != nil {
			return "", err
		}
		m.Fork = id
		if err = n.save(p, m); err != nil {
			return "", err
		}
		n.server.deleteSessionBestEffort(strings.TrimPrefix(w.Session, SessionPrefix))
	}
	newRequest := m.MessageID == ""
	if newRequest {
		m.MessageID = "msg_" + mustRandomHex(12)
		if err = n.save(p, m); err != nil {
			return "", err
		}
	}
	// Import/fork setup is serialized; independent fork generation is not.
	n.mu.Unlock()
	locked = false
	if reply, err := n.server.replyForMessage(ctx, m.Fork, m.MessageID); err == nil && reply != "" {
		m.Reply = reply
	} else if m.Reply == "" {
		// A retained request with no completed assistant reply was interrupted.
		// Retry on the same native fork with a fresh message id rather than
		// colliding with OpenCode's unique message id constraint.
		if !newRequest {
			m.MessageID = "msg_" + mustRandomHex(12)
			n.mu.Lock()
			locked = true
			if err = n.save(p, m); err != nil {
				return "", err
			}
			n.mu.Unlock()
			locked = false
		}
		m.Reply, err = n.server.runSessionWithMessage(ctx, m.Fork, m.MessageID, model, prompt, input)
		if err != nil {
			return "", err
		}
	}
	n.mu.Lock()
	locked = true
	if err = n.save(p, m); err != nil {
		return "", err
	}
	n.mu.Unlock()
	locked = false
	w.SetFinalizer(func() error { return n.finalize(p) })
	return m.Reply, nil
}

func (n *nativeRuntime) load(p string) (nativeManifest, error) {
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nativeManifest{}, nil
	}
	if err != nil {
		return nativeManifest{}, err
	}
	var m nativeManifest
	if err = json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("opencode native retained manifest %s is malformed: %w", p, err)
	}
	if m.Session == "" || m.Lens == "" || m.Total < 0 {
		return m, fmt.Errorf("opencode native retained manifest %s is malformed", p)
	}
	return m, nil
}
func (n *nativeRuntime) save(p string, m nativeManifest) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err = os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
func (n *nativeRuntime) export(ctx context.Context, w *platform.NativeSession, snap string) error {
	src, err := DefaultDBPath()
	if err != nil {
		return err
	}
	b, err := nativeCommand(ctx, replaceEnv(os.Environ(), []string{"OPENCODE_DB=" + src}), "export", "--pure", strings.TrimPrefix(w.Session, SessionPrefix))
	if err != nil {
		return fmt.Errorf("opencode native export unavailable; upgrade to OpenCode 1.18.0+: %w", err)
	}
	if w.Digest != "" {
		if err := validateExportDigest(b, w.Digest); err != nil {
			return err
		}
	}
	return os.WriteFile(snap, b, 0o600)
}

func validateExportDigest(data []byte, want string) error {
	digest, err := exportedTranscriptDigest(data)
	if err != nil {
		return err
	}
	if digest != want {
		return fmt.Errorf("opencode native session changed after L0 capture; generation remains pending")
	}
	return nil
}

func exportedTranscriptDigest(data []byte) (string, error) {
	var exported struct {
		Messages []struct {
			Info struct {
				Role string `json:"role"`
				Time struct {
					Completed int64 `json:"completed"`
				} `json:"time"`
			} `json:"info"`
			Parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(data, &exported); err != nil {
		return "", fmt.Errorf("decode opencode native export: %w", err)
	}
	entries := make([]platform.TranscriptEntry, 0, len(exported.Messages))
	for _, message := range exported.Messages {
		if message.Info.Role != "user" && message.Info.Role != "assistant" {
			continue
		}
		if message.Info.Role == "assistant" && message.Info.Time.Completed == 0 {
			continue
		}
		var text strings.Builder
		for _, part := range message.Parts {
			if part.Type != "text" || strings.TrimSpace(part.Text) == "" {
				continue
			}
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(part.Text)
		}
		if value := strings.TrimSpace(text.String()); value != "" {
			entries = append(entries, platform.TranscriptEntry{Role: message.Info.Role, Text: value})
		}
	}
	return platform.TranscriptDigest(entries), nil
}
func (n *nativeRuntime) importSnapshot(ctx context.Context, snap string) error {
	if err := n.prepareAuth(); err != nil {
		return err
	}
	_, err := nativeCommand(ctx, replaceEnv(os.Environ(), isolatedEnv(n.root)), "import", "--pure", snap)
	if err != nil {
		return fmt.Errorf("opencode native import: %w", err)
	}
	return nil
}

// prepareAuth copies, never links or writes, user auth. Missing auth is normal for
// environment-backed providers.
func (n *nativeRuntime) prepareAuth() error {
	db, err := DefaultDBPath()
	if err != nil {
		return err
	}
	src := filepath.Join(filepath.Dir(db), "auth.json")
	si, err := os.Stat(src)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	dst := filepath.Join(n.root, "xdg", "opencode", "auth.json")
	if li, err := os.Lstat(dst); err == nil && li.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(dst); err != nil {
			return err
		}
	}
	if di, err := os.Stat(dst); err == nil && !si.ModTime().After(di.ModTime()) {
		return os.Chmod(dst, 0o600)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

func (n *nativeRuntime) finalize(p string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	m, err := n.load(p)
	if err != nil {
		return err
	}
	m.Committed = true
	if err = n.save(p, m); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if m.Fork != "" {
		if err = n.server.deleteSession(ctx, m.Fork); err != nil {
			return err
		}
	}
	if err = os.Remove(n.snapshot(p)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Remove(p)
}

// reconcile closes the post-L1/pre-finalizer crash window. The raw high id is the
// store's generation token: a missing token means a replace-import or cleanup
// superseded this manifest, so its private artifacts are no longer useful.
func (n *nativeRuntime) reconcile() error {
	entries, err := os.ReadDir(n.dir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, x := range entries {
		if !strings.HasSuffix(x.Name(), ".json") || strings.HasSuffix(x.Name(), ".snapshot.json") {
			continue
		}
		p := filepath.Join(n.dir(), x.Name())
		m, err := n.load(p)
		if err != nil {
			return err
		}
		if m.Committed {
			if err = n.finalize(p); err != nil {
				return err
			}
			continue
		}
		current, committed, err := n.generationStatus(m)
		if err != nil {
			continue // without store evidence, retain an uncommitted generation
		}
		if !current || committed {
			m.Committed = true
			if err = n.save(p, m); err != nil {
				return err
			}
			if err = n.finalize(p); err != nil {
				return err
			}
		}
	}
	return nil
}
func (n *nativeRuntime) generationStatus(m nativeManifest) (current, committed bool, err error) {
	db, err := sql.Open("sqlite", readOnlyURI(filepath.Join(filepath.Dir(n.root), "witness.db")))
	if err != nil {
		return false, false, err
	}
	defer db.Close()
	var currentValue, committedValue int
	err = db.QueryRow(`SELECT
		CASE WHEN (? = 0 AND NOT EXISTS (SELECT 1 FROM raw WHERE session = ?))
		       OR EXISTS (SELECT 1 FROM raw WHERE session = ? AND id = ?) THEN 1 ELSE 0 END,
		CASE WHEN COALESCE((SELECT distilled >= ? FROM progress WHERE session = ? AND lens = ?), 0)
		     THEN 1 ELSE 0 END`,
		m.RawHigh, m.Session, m.Session, m.RawHigh, m.Total, m.Session, m.Lens,
	).Scan(&currentValue, &committedValue)
	if err != nil {
		return false, false, err
	}
	return currentValue != 0, committedValue != 0, nil
}

func readOnlyURI(path string) string {
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	u.RawQuery = q.Encode()
	return u.String()
}
func commandOutput(ctx context.Context, env []string, args ...string) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("missing opencode command")
	}
	c := execCommandContext(ctx, "opencode", args...)
	c.Env = env
	procCtl.BindToParent(c)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// indirection keeps command execution testable without touching process globals.
var execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
