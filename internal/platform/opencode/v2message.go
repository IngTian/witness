package opencode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/IngTian/witness/internal/platform"
)

// v2Error represents an OpenCode v2 error structure found in message.error or retry.error.
type v2Error struct {
	Type         string `json:"type"`
	Message      string `json:"message"`
	Status       int    `json:"status"`
	RetryAfterMs *int64 `json:"retryAfterMs,omitempty"`
	Response     *struct {
		Body string `json:"body"`
	} `json:"response,omitempty"`
}

// v2Retry represents the retry state visible in assistant messages during backoff.
type v2Retry struct {
	At    int64   `json:"at"`
	Error v2Error `json:"error"`
}

// v2Message represents one message from GET /api/session/{id}/message.
type v2Message struct {
	ID      string `json:"id"`
	Type    string `json:"type"`    // "user", "assistant", "idle"
	Outcome string `json:"outcome"` // "succeeded", "failed", "interrupted"
	Content []struct {
		Type string `json:"type"` // "text", "thinking"
		Text string `json:"text"`
	} `json:"content"`
	Retry *v2Retry `json:"retry,omitempty"`
	Error *v2Error `json:"error,omitempty"`
	Time  struct {
		Created   int64  `json:"created"`
		Completed *int64 `json:"completed,omitempty"`
	} `json:"time"`
}

// turnState holds the parsed state of one turn (user → assistant → idle).
type turnState struct {
	Present bool       // true when requestID is found in the data array
	Started bool       // true when any assistant after requestID has Retry == nil
	Retry   *v2Retry   // the last assistant's retry, set only while not started
	Idle    *v2Message // the first idle message after requestID
	Last    *v2Message // the last assistant message after requestID
	Reply   string     // the assistant reply text (reasoning parts dropped)
}

var errOpenCodeModelUnavailable = errors.New("model unavailable")

// parseTurn unmarshals the {"data":[]v2Message} response from GET /api/session/{id}/message
// and extracts the turn state for the request identified by requestID.
//
// Only records AFTER the requestID matter. The function finds Last (last assistant), Idle
// (first idle), Started (any assistant with Retry == nil), Retry (last assistant's retry,
// only while not started), and Reply (joined text parts from the last assistant, with
// reasoning parts dropped).
func parseTurn(data []byte, requestID string) (turnState, error) {
	var response struct {
		Data []v2Message `json:"data"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return turnState{}, fmt.Errorf("parse v2 messages: %w", err)
	}

	// Find the index of our request
	reqIndex := -1
	for i, msg := range response.Data {
		if msg.ID == requestID {
			reqIndex = i
			break
		}
	}

	var st turnState
	st.Present = reqIndex >= 0
	if !st.Present {
		return st, nil
	}

	// Process records after the request
	for i := reqIndex + 1; i < len(response.Data); i++ {
		msg := response.Data[i]

		switch msg.Type {
		case "assistant":
			st.Last = &response.Data[i] // keep a pointer to the message in the array

			// Started when any assistant has Retry == nil
			if msg.Retry == nil {
				st.Started = true
			}

			// Build reply text from the last assistant with non-blank text
			if len(msg.Content) > 0 {
				var parts []string
				for _, c := range msg.Content {
					if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
						parts = append(parts, strings.TrimSpace(c.Text))
					}
				}
				if len(parts) > 0 {
					st.Reply = strings.Join(parts, "\n\n")
				}
			}

		case "idle":
			if st.Idle == nil {
				st.Idle = &response.Data[i]
			}
		}
	}

	// Retry is the last assistant's retry, only when not started
	if !st.Started && st.Last != nil && st.Last.Retry != nil {
		st.Retry = st.Last.Retry
	}

	return st, nil
}

// turnError converts the turnState into an error, or nil on success.
func turnError(st turnState, now time.Time) error {
	// No idle or succeeded → success
	if st.Idle == nil || st.Idle.Outcome == "succeeded" {
		return nil
	}

	// Interrupted
	if st.Idle.Outcome == "interrupted" {
		return errors.New("opencode turn was interrupted before it finished")
	}

	// Failed: extract the error from Idle or Last
	var e v2Error
	if st.Idle.Error != nil {
		e = *st.Idle.Error
	} else if st.Last != nil && st.Last.Error != nil {
		e = *st.Last.Error
	} else {
		return errors.New("opencode turn failed with no error details")
	}

	// Free limit → QuotaExhaustedError
	if isFreeLimit(e) {
		return &platform.QuotaExhaustedError{
			ResetAt: freeLimitResetAt(st, e, now),
			Runtime: "opencode",
		}
	}

	// Model unavailable
	if e.Type == "provider.no-route" {
		return fmt.Errorf("%w: %s", errOpenCodeModelUnavailable, e.Message)
	}

	// Generic failure
	return fmt.Errorf("opencode generation failed: %s (status %d): %s", e.Type, e.Status, e.Message)
}

// isFreeLimit reports whether the error is a free-tier quota exhaustion.
func isFreeLimit(e v2Error) bool {
	if e.Type == "provider.free-limit" {
		return true
	}
	// Fallback: check the gateway body for FreeUsageLimitError
	if e.Response != nil && strings.Contains(e.Response.Body, "FreeUsageLimitError") {
		return true
	}
	return false
}

// freeLimitResetAt computes the reset time for a free-tier quota error.
//
// It starts from Last.Time.Completed, then Idle.Time.Created, then now. It adds
// retryAfterMs when 0 < x ≤ 45 days, else 1 h.
func freeLimitResetAt(st turnState, e v2Error, now time.Time) time.Time {
	const maxRetryAfter = 45 * 24 * time.Hour
	const fallback = time.Hour

	// Find the base time
	var base time.Time
	if st.Last != nil && st.Last.Time.Completed != nil {
		base = time.UnixMilli(*st.Last.Time.Completed)
	} else if st.Idle != nil {
		base = time.UnixMilli(st.Idle.Time.Created)
	} else {
		base = now
	}

	// Add the retry delay
	if e.RetryAfterMs != nil && *e.RetryAfterMs > 0 && time.Duration(*e.RetryAfterMs)*time.Millisecond <= maxRetryAfter {
		return base.Add(time.Duration(*e.RetryAfterMs) * time.Millisecond)
	}
	return base.Add(fallback)
}
