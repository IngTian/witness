package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// OpenCode model generations may overlap. This uses only a fake HTTP server, so
// concurrency is verified without loading models or launching subprocesses.
func TestOpenCodeServerRunsModelsConcurrently(t *testing.T) {
	const n = 4
	var maxInFlight int32
	var inFlight int32
	var messageIDs sync.Map

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Each Run creates its own session id from the request; echo a per-session id
		// so the isolated-session assumption holds. We key off a session counter.
		switch r.Method {
		case http.MethodPost:
			if r.URL.Path == "/session" {
				// unique id per created session
				id := fmt.Sprintf("ses_%d", atomic.AddInt32(&sessionSeq, 1))
				_, _ = fmt.Fprintf(w, `{"id":%q}`, id)
				return
			}
			// prompt_async
			var body struct {
				MessageID string `json:"messageID"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			messageIDs.Store(r.URL.Path, body.MessageID)
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			// The reply poll is where model generation concurrency is observable.
			cur := atomic.AddInt32(&inFlight, 1)
			defer atomic.AddInt32(&inFlight, -1)
			for {
				old := atomic.LoadInt32(&maxInFlight)
				if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			promptPath := strings.TrimSuffix(r.URL.Path, "/message") + "/prompt_async"
			requestID, _ := messageIDs.Load(promptPath)
			// reply keyed to whatever message id was requested
			_, _ = fmt.Fprintf(w, `[
				{"info":{"id":%q,"role":"user"},"parts":[{"id":"u","type":"text","text":"DATA"}]},
				{"info":{"id":"msg_reply","role":"assistant"},"parts":[{"id":"a","type":"text","text":"RESULT"}]}
			]`, requestID)
		case http.MethodDelete:
			_, _ = w.Write([]byte(`{}`))
		}
	})
	ts := httptest.NewServer(h)
	defer ts.Close()
	srv := &OpenCodeServer{baseURL: ts.URL, authHeader: "Basic test", client: ts.Client()}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			_, errs[i] = srv.Run(ctx, "", "EXTRACT", "DATA")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Run %d failed: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&maxInFlight); got < 2 {
		t.Fatalf("max concurrent OpenCode model runs = %d, want at least 2", got)
	}
}

// sessionSeq gives each created fake session a unique id across the concurrency
// test's goroutines.
var sessionSeq int32

// TestOpenCodeServerRunRejectsAfterClose keeps the closed-check honest after the
// narrowing: a Run started once the server is marked closed still errors fast.
func TestOpenCodeServerRunRejectsAfterClose(t *testing.T) {
	srv := &OpenCodeServer{baseURL: "http://127.0.0.1:0", authHeader: "Basic test", client: http.DefaultClient}
	srv.mu.Lock()
	srv.closed = true
	srv.mu.Unlock()
	if _, err := srv.Run(context.Background(), "", "EXTRACT", "DATA"); err == nil {
		t.Fatal("Run on a closed server should error")
	}
}
