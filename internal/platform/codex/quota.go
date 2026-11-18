package codex

import (
	"regexp"
	"runtime"
	"strings"
	"time"
)

var ordinalDay = regexp.MustCompile(`(\d+)(st|nd|rd|th),`)

// Codex 0.169 emits a local wall-clock string, not a structured reset field
// (protocol/src/error.rs format_retry_timestamp). childEnv sets TZ=UTC. Only a
// genuine usage-limit message with a future reset can arm a pause: missing plan
// access, exhausted credit and an unknown/stale reset are ordinary failures.
func usageReset(message string, now time.Time) (time.Time, bool) {
	location := time.UTC
	// Chrono's Windows Local implementation uses Win32 timezone APIs and does
	// not honor TZ. Match that clock rather than treating local text as UTC.
	if runtime.GOOS == "windows" {
		location = time.Local
	}
	return usageResetIn(message, now, location)
}

func usageResetIn(message string, now time.Time, location *time.Location) (time.Time, bool) {
	lower := strings.ToLower(message)
	if !strings.Contains(lower, "hit your usage limit") {
		return time.Time{}, false
	}
	i := strings.LastIndex(lower, "try again at ")
	if i < 0 {
		return time.Time{}, false
	}
	value := strings.TrimSpace(message[i+len("try again at "):])
	value = strings.TrimSuffix(value, ".")
	value = ordinalDay.ReplaceAllString(value, "$1,")
	if clock, err := time.Parse("3:04 PM", value); err == nil {
		now = now.In(location)
		reset := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, location)
		// A reset rounded to minutes may be stale by seconds. Do not turn a past
		// time into a new 24-hour pause; the next drain may retry normally.
		return reset, reset.After(now)
	}
	for _, layout := range []string{"Jan 2, 2006 3:04 PM", "Jan 2, 2006, 3:04 PM"} {
		if reset, err := time.ParseInLocation(layout, value, location); err == nil {
			return reset, reset.After(now)
		}
	}
	return time.Time{}, false
}
