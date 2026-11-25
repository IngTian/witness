package codex

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// A pinned version passed the contract matrix in CI. Any other version gets the same
// wire check on this machine before it may receive a transcript: a real `codex exec`
// with the exact argv Run sends, pointed at a loopback Responses endpoint. The check
// runs in a scratch home on a random key, so the user's key and witness's login are
// never involved, and only the request itself is judged.

const probeSystem, probeCorpus, probeAnswer = "WITNESS-SYSTEM-ONLY", "WITNESS-CORPUS-ONLY", "WITNESS-PROBE-OK"

var selfCheckTimeout = 2 * time.Minute

// ensureToolFree admits a pinned version, or a (binary, model) pair this machine has
// already checked, and checks any other pair now. A record vouches for the checked
// binary (version, size and mtime), not for everything printing that version. Only a
// pass is recorded, so a transient failure never sticks. Callers hold the login lock,
// which serializes writes.
func ensureToolFree(ctx context.Context, bin, root, version string, catalog []byte, models ...string) error {
	if slices.Contains(testedVersions, version) {
		return nil
	}
	st, err := os.Stat(bin)
	if err != nil {
		return err
	}
	id := fmt.Sprintf("%s %d %d", version, st.Size(), st.ModTime().UnixNano())
	path := filepath.Join(root, "codex", "verified.json")
	verified := map[string][]string{}
	if b, err := os.ReadFile(path); err == nil {
		var saved map[string][]string
		if json.Unmarshal(b, &saved) == nil && saved != nil { // a corrupt record only costs a re-check
			verified = saved
		}
	}
	for _, model := range models {
		if slices.Contains(verified[id], model) {
			continue
		}
		if err := selfCheck(ctx, bin, catalog, model); err != nil {
			return fmt.Errorf("Codex CLI %s failed witness's tool-free check for %s: %w; install one of %s and point WITNESS_CODEX_BINARY at it",
				version, model, err, strings.Join(testedVersions, ", "))
		}
		verified[id] = append(verified[id], model)
		b, err := json.Marshal(verified)
		if err != nil {
			return err
		}
		if err := os.WriteFile(path+".tmp", b, 0600); err != nil {
			return err
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			return err
		}
	}
	return nil
}

// selfCheck passes when witness's request for model offers no tools and carries only
// the instructions and the corpus, and when dropping the catalog override visibly
// offers tools. The second run proves the first could have seen them.
func selfCheck(ctx context.Context, bin string, catalog []byte, model string) error {
	ctx, cancel := context.WithTimeout(ctx, selfCheckTimeout)
	defer cancel()
	stripped, err := stripCatalog(catalog, model)
	if err != nil {
		return err
	}
	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	srv, err := newProbeServer(catalog, hex.EncodeToString(key))
	if err != nil {
		return err
	}
	defer srv.Close()
	requests, err := srv.exec(ctx, bin, model, stripped)
	if err != nil {
		return err
	}
	for _, req := range requests {
		var sent string
		if json.Unmarshal(req["model"], &sent) != nil || sent != model {
			return fmt.Errorf("request named model %s, not %q", req["model"], model)
		}
		if err := checkWireRequest(req, probeSystem, probeCorpus); err != nil {
			return err
		}
	}
	if requests, err = srv.exec(ctx, bin, model, nil); err != nil {
		return err
	}
	if slices.ContainsFunc(requests, offersTools) {
		return nil
	}
	return errors.New("without witness's catalog override this CLI's requests still show no tools, so the check cannot see them")
}

// offersTools is the sensitivity run's test, deliberately narrower than any contract
// failure: top-level tools, or an input item other than a message (Responses Lite
// sends its tools as an additional_tools item).
func offersTools(req map[string]json.RawMessage) bool {
	var tools []json.RawMessage
	if json.Unmarshal(req["tools"], &tools) == nil && len(tools) > 0 {
		return true
	}
	var input []struct{ Type string }
	_ = json.Unmarshal(req["input"], &input)
	return slices.ContainsFunc(input, func(item struct{ Type string }) bool { return item.Type != "message" })
}

// checkWireRequest is the tool-free contract for one Responses request: no top-level
// tools, and no input other than developer instructions and exactly one user corpus.
func checkWireRequest(req map[string]json.RawMessage, system, corpus string) error {
	var offered []json.RawMessage
	if raw := req["tools"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &offered); err != nil {
			return err
		}
	}
	if len(offered) != 0 {
		return fmt.Errorf("tools offered: %s", req["tools"])
	}
	var input []struct {
		Type, Role string
		Content    []struct{ Type, Text string }
	}
	if err := json.Unmarshal(req["input"], &input); err != nil {
		return err
	}
	dev, user := 0, 0
	for _, msg := range input {
		if msg.Type != "message" {
			return fmt.Errorf("unexpected wire input item: %s", msg.Type)
		}
		if len(msg.Content) != 1 || msg.Content[0].Type != "input_text" {
			return fmt.Errorf("unexpected %s message content", msg.Role)
		}
		switch msg.Role {
		case "developer":
			if msg.Content[0].Text != system {
				return fmt.Errorf("unexpected developer instructions")
			}
			dev++
		case "user":
			if msg.Content[0].Text != corpus {
				return fmt.Errorf("unexpected corpus")
			}
			user++
		default:
			return fmt.Errorf("unexpected message role %q", msg.Role)
		}
	}
	if dev == 0 || user != 1 {
		return fmt.Errorf("instruction/corpus channels: developer=%d user=%d", dev, user)
	}
	return nil
}

// probeServer is the fake provider: GET serves the catalog unstripped (as the bundled
// one would be), and only POST /v1/responses with the check's own key is answered.
type probeServer struct {
	*http.Server
	url, token string
	catalog    []byte
	mu         sync.Mutex
	requests   []map[string]json.RawMessage
}

func newProbeServer(catalog []byte, token string) (*probeServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &probeServer{url: "http://" + ln.Addr().String(), token: token, catalog: catalog}
	p.Server = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.Serve(ln) }()
	return p, nil
}

func (p *probeServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(p.catalog)
		return
	}
	if req.Method != http.MethodPost || req.URL.Path != "/v1/responses" || req.Header.Get("Authorization") != "Bearer "+p.token {
		http.Error(w, "unexpected provider request", http.StatusBadRequest)
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(req.Body, 16<<20)).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.requests = append(p.requests, body)
	p.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	item := map[string]any{"id": "msg_check", "type": "message", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": probeAnswer, "annotations": []any{}}}}
	for _, e := range []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_check", "status": "in_progress", "output": []any{}}},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": map[string]any{"id": "resp_check", "status": "completed", "output": []any{item},
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}},
	} {
		b, _ := json.Marshal(e)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e["type"], b)
	}
}

// exec sends one check call and returns the requests it produced. A nil catalog drops
// the model_catalog_json override, as the sensitivity run needs.
func (p *probeServer) exec(ctx context.Context, bin, model string, catalog []byte) ([]map[string]json.RawMessage, error) {
	dir, err := scratchDir()
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	home, cwd := filepath.Join(dir, "home"), filepath.Join(dir, "cwd")
	for _, d := range []string{home, cwd} {
		if err := os.Mkdir(d, 0700); err != nil {
			return nil, err
		}
	}
	args, err := execArgs(dir, cwd, model, probeSystem, catalog)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.requests = nil
	p.mu.Unlock()
	cmd := commandContext(ctx, bin, append([]string{"-c", configPath("openai_base_url", p.url+"/v1")}, args...)...)
	cmd.Env, cmd.Dir, cmd.Stdin = probeEnv(home, p.token), cwd, strings.NewReader(probeCorpus)
	cmd.WaitDelay = time.Second
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) { // ErrWaitDelay: exited 0 (see Run)
		return nil, childError(ctx, "self-check", err, stderr.String())
	}
	if answer, err := parseEvents(out.Bytes(), time.Now()); err != nil || answer != probeAnswer {
		return nil, fmt.Errorf("codex self-check answered %q: %v", answer, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		return nil, errors.New("codex self-check: no generation request reached the check")
	}
	return p.requests, nil
}

// probeEnv drops the user's CODEX_API_KEY and any proxy, so the only credential in the
// child is the check's own key and its only provider is the loopback endpoint.
func probeEnv(home, token string) []string {
	env := []string{}
	for _, entry := range childEnv(home) {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "CODEX_API_KEY", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY":
			continue
		}
		env = append(env, entry)
	}
	return append(env, "CODEX_API_KEY="+token)
}
