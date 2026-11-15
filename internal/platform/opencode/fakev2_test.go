package opencode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeV2 is an httptest fake of the OpenCode v2 routes the runner drives (the plan's PR 2 route
// table), so no runner test spawns a real opencode. It is strict where the real serve is silent:
//   - every request must carry `Authorization: Basic test`;
//   - every message poll must ask for the turn oldest-first (`order=asc&limit=200`, the ONE
//     spelling, messagesPath): the default is newest-first, and parseTurn reads records AFTER our
//     request, so a poll in the default order would find nothing ours forever;
//   - /api/agent lists only the agents written into configPath, and only after agentDelay polls
//     (a real serve picks a new config agent up ~0.25 s after the atomic rename);
//   - a prompt to a session whose agent is not listed ends in an idle `failed` "Agent not found",
//     the real serve's loud failure (critic C1).
type fakeV2 struct {
	t          *testing.T
	configPath string
	agentDelay int
	models     []map[string]any    // GET /api/model's data
	envNames   map[string][]string // GET /api/integration/{provider}: the provider's env var names
	// turn renders a PROMPTED session's whole message list (an unprompted one is empty). It runs
	// under mu. A nil list makes that poll fail with a 500.
	turn                  func(*fakeSession) []map[string]any
	promptStatus          int               // non-zero: every prompt POST fails with this status
	onCall                func(call string) // runs under mu as each "METHOD path" arrives, if set
	calls                 []string          // "METHOD path", in arrival order
	inFlight, maxInFlight int               // prompted sessions whose turn has not yet shown an idle

	mu         sync.Mutex
	sessions   map[string]*fakeSession
	order      []string // session ids in creation order
	agentPolls int
}

type fakeSession struct {
	ID, Agent, MsgID, Text string
	Create, Prompt         map[string]any // the request bodies, as decoded
	Prompted               time.Time
	idle, deleted          bool
	prior                  []map[string]any // leftover: the messages an earlier serve recorded
}

func (s *fakeSession) user() map[string]any {
	return map[string]any{"id": s.MsgID, "type": "user", "content": []map[string]any{{"type": "text", "text": s.Text}}}
}

// assistantRec is an assistant record with one text part (none when text is ""); retry != nil
// is OpenCode's internal backoff, i.e. the provider has not started.
func assistantRec(text string, retry map[string]any) map[string]any {
	content := []map[string]any{}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	return map[string]any{"type": "assistant", "content": content, "retry": retry}
}

func idleRec(outcome string, errRec map[string]any) map[string]any {
	r := map[string]any{"type": "idle", "outcome": outcome}
	if errRec != nil {
		r["error"] = errRec
	}
	return r
}

func retryRec(at time.Time, typ, msg string) map[string]any {
	return map[string]any{"attempt": 2, "at": at.UnixMilli(), "error": map[string]any{"type": typ, "message": msg, "status": 429}}
}

// okTurn answers every poll with a finished, successful turn.
func okTurn(text string) func(*fakeSession) []map[string]any {
	return func(s *fakeSession) []map[string]any {
		return []map[string]any{s.user(), assistantRec(text, nil), idleRec("succeeded", nil)}
	}
}

// newFakeV2 starts the fake and builds the server under test through the one constructor, so the
// generation cap and the agent map are what production gets. It also polls every millisecond.
func newFakeV2(t *testing.T) (*fakeV2, *OpenCodeServer) {
	t.Helper()
	f := &fakeV2{t: t, configPath: filepath.Join(t.TempDir(), "opencode.json"), turn: okTurn("RESULT"), sessions: map[string]*fakeSession{}}
	ts := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(ts.Close)
	prev := openCodeAsyncPollInterval
	openCodeAsyncPollInterval = time.Millisecond
	t.Cleanup(func() { openCodeAsyncPollInterval = prev })
	return f, newOpenCodeServer(ts.URL, "Basic test", f.configPath, ts.Client())
}

func (f *fakeV2) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeV2) count(match func(call string) bool) int {
	n := 0
	for _, c := range f.seen() {
		if match(c) {
			n++
		}
	}
	return n
}

func (f *fakeV2) peak() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

// leftover seeds a session an earlier serve left in the root's private DB (every serve of one
// root shares it): its messages are served as recorded, whatever this serve's agents are.
func (f *fakeV2) leftover(id string, msgs ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[id] = &fakeSession{ID: id, prior: msgs}
}

// session returns a copy of the session with this id (nil when none was created).
func (f *fakeV2) session(id string) *fakeSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s := f.sessions[id]; s != nil {
		c := *s
		return &c
	}
	return nil
}

// config reads the private config the server under test wrote.
func (f *fakeV2) config() map[string]any {
	f.t.Helper()
	b, err := os.ReadFile(f.configPath)
	if err != nil {
		f.t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		f.t.Fatal(err)
	}
	return cfg
}

// listed reports the agent ids /api/agent shows right now. Caller holds mu.
func (f *fakeV2) listed() []string {
	if f.agentPolls <= f.agentDelay {
		return nil
	}
	b, err := os.ReadFile(f.configPath)
	if err != nil {
		return nil
	}
	var cfg struct {
		Agents map[string]any `json:"agents"`
	}
	_ = json.Unmarshal(b, &cfg)
	var ids []string
	for id := range cfg.Agents {
		ids = append(ids, id)
	}
	return ids
}

func (f *fakeV2) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if f.onCall != nil {
		f.onCall(r.Method + " " + r.URL.Path)
	}
	if got := r.Header.Get("Authorization"); got != "Basic test" {
		f.t.Errorf("%s %s: Authorization = %q, want %q", r.Method, r.URL.Path, got, "Basic test")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	decode := func() map[string]any {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("%s %s: body: %v", r.Method, r.URL.Path, err)
		}
		return body
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/session/")
	id, action, _ := strings.Cut(path, "/")
	s := f.sessions[id]
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/agent":
		f.agentPolls++
		data := []map[string]any{}
		for _, a := range f.listed() {
			data = append(data, map[string]any{"id": a})
		}
		reply(map[string]any{"data": data})
	case r.Method == http.MethodGet && r.URL.Path == "/api/model":
		reply(map[string]any{"location": "fake", "data": f.models})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/integration/"):
		names, ok := f.envNames[strings.TrimPrefix(r.URL.Path, "/api/integration/")]
		if !ok { // a provider with no integration
			w.WriteHeader(http.StatusNotFound)
			return
		}
		reply(map[string]any{"data": map[string]any{"methods": []map[string]any{{"type": "env", "names": names}}}})
	case r.Method == http.MethodPost && r.URL.Path == "/api/session":
		body := decode()
		agent, _ := body["agent"].(string)
		s := &fakeSession{ID: fmt.Sprintf("ses_%d", len(f.order)+1), Agent: agent, Create: body}
		f.sessions[s.ID], f.order = s, append(f.order, s.ID)
		reply(map[string]any{"data": map[string]any{"id": s.ID}})
	case (s == nil || s.deleted) && strings.HasPrefix(r.URL.Path, "/api/session/"):
		w.WriteHeader(http.StatusNotFound)
		reply(map[string]any{"_tag": "SessionNotFoundError", "sessionID": id})
	case r.Method == http.MethodPost && action == "prompt" && f.promptStatus != 0:
		w.WriteHeader(f.promptStatus)
	case r.Method == http.MethodPost && action == "prompt":
		body := decode()
		s.Prompt, s.Prompted = body, time.Now()
		s.MsgID, _ = body["id"].(string)
		s.Text, _ = body["text"].(string)
		f.inFlight++
		f.maxInFlight = max(f.maxInFlight, f.inFlight)
		reply(map[string]any{"data": map[string]any{"id": s.MsgID, "sessionID": s.ID, "type": "user", "delivery": "steer"}})
	case r.Method == http.MethodGet && action == "message":
		if q := r.URL.Query(); q.Get("order") != "asc" || q.Get("limit") != "200" {
			f.t.Errorf("message poll %s asks for order=%q limit=%q, want asc and 200", r.URL, q.Get("order"), q.Get("limit"))
		}
		data := []map[string]any{}
		switch {
		case s.prior != nil:
			data = s.prior
		case s.MsgID == "":
		case !slices.Contains(f.listed(), s.Agent):
			data = []map[string]any{s.user(), idleRec("failed", map[string]any{"type": "unknown", "message": "Agent not found: \"" + s.Agent + "\""})}
		default:
			if data = f.turn(s); data == nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		for _, m := range data {
			if m["type"] == "idle" {
				f.finish(s)
			}
		}
		reply(map[string]any{"data": data, "cursor": map[string]any{}})
	case r.Method == http.MethodPost && action == "interrupt":
		f.finish(s)
		reply(map[string]any{"interrupted": true})
	case r.Method == http.MethodDelete && action == "":
		f.finish(s)
		s.deleted = true
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
	}
}

// finish ends a prompted session's generation once. Caller holds mu.
func (f *fakeV2) finish(s *fakeSession) {
	if s.MsgID != "" && !s.idle {
		s.idle = true
		f.inFlight--
	}
}
