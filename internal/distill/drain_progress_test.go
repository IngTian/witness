package distill

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// drainEvents records Drain's progress and fails if two snapshots are ever delivered at once:
// the contract is that Progress is called from the Drain goroutine only, never from a miner.
type drainEvents struct {
	t        *testing.T
	inFlight atomic.Int32
	mu       sync.Mutex
	evs      []Progress
}

func (d *drainEvents) record(p Progress) {
	if d.inFlight.Add(1) != 1 {
		d.t.Error("Progress called concurrently; it must only be called from the Drain goroutine")
	}
	defer d.inFlight.Add(-1)
	d.mu.Lock()
	d.evs = append(d.evs, p)
	d.mu.Unlock()
}

func (d *drainEvents) mining(t *testing.T) []Stage {
	t.Helper()
	var out []Stage
	for _, p := range d.evs {
		out = append(out, stage(t, p, "mining sessions"))
	}
	if len(out) == 0 {
		t.Fatal("Drain reported no progress")
	}
	for i := 1; i < len(out); i++ {
		if out[i].Done < out[i-1].Done || out[i].Total < out[i-1].Total {
			t.Fatalf("progress went backwards: %+v -> %+v", out[i-1], out[i])
		}
	}
	return out
}

// Every session counts once it is finished, whatever happened to it — a failed mine is finished
// work, or the bar stalls short of its total — and the count matches the calls the model got.
func TestDrainProgressCountsEverySession(t *testing.T) {
	for _, conc := range []int{1, 4} {
		s := newStore(t)
		var calls atomic.Int32
		w := drainWorker(s, func(ctx context.Context, model, prompt, input string) (string, error) {
			calls.Add(1)
			if strings.Contains(input, "turn-bad") {
				return "", errors.New("provider down") // backs off inside MineSession
			}
			if strings.Contains(input, "turn-boom") {
				panic("embedder blew up") // surfaces as a mine error from mineSessionSafe
			}
			return (&safeMiner{}).run(ctx, model, prompt, input)
		})
		for _, id := range []string{"a", "b", "bad", "c", "boom", "e"} {
			capture(t, s, id, "user", "turn-"+id)
		}
		pending := func() []string { p, _ := s.PendingSessions([]string{"default"}); return p }
		ev := &drainEvents{t: t}
		n := w.Drain(context.Background(), DrainOpts{Conc: conc, Pending: pending, Progress: ev.record})

		m := ev.mining(t)
		if m[0].Done != 0 || m[0].Total != 6 || !m[0].Open {
			t.Errorf("conc=%d: the first snapshot must carry the whole batch before any call: %+v", conc, m[0])
		}
		last := m[len(m)-1]
		if last.Done != last.Total || last.Done != int(calls.Load()) || last.Done != 6 || n != 6 {
			t.Errorf("conc=%d: ended at %d/%d after %d model calls (Drain reaped %d), want 6/6",
				conc, last.Done, last.Total, calls.Load(), n)
		}
		if ev.evs[0].Where != "" {
			t.Errorf("the engine leaves Where for the caller to name, got %q", ev.evs[0].Where)
		}
	}
}

// A session captured mid-drain is mined by the next scan, so the total grows to include it —
// which is why the stage is Open — and the bar still ends at its (grown) total.
func TestDrainProgressTotalGrowsWithAMidDrainArrival(t *testing.T) {
	s := newStore(t)
	var calls atomic.Int32
	w := drainWorker(s, func(ctx context.Context, model, prompt, input string) (string, error) {
		calls.Add(1)
		return (&safeMiner{}).run(ctx, model, prompt, input)
	})
	capture(t, s, "first", "user", "turn-first")
	pending := func() []string {
		if s.DistilledCount("first", "default") > 0 && s.RawCount("second") == 0 {
			capture(t, s, "second", "user", "turn-second") // lands while the drain is running
		}
		p, _ := s.PendingSessions([]string{"default"})
		return p
	}
	ev := &drainEvents{t: t}
	w.Drain(context.Background(), DrainOpts{Conc: 1, Pending: pending, Progress: ev.record})

	m := ev.mining(t)
	if m[0].Total != 1 {
		t.Fatalf("only one session was pending at the start: %+v", m[0])
	}
	last := m[len(m)-1]
	if last.Total != 2 || last.Done != 2 || calls.Load() != 2 {
		t.Fatalf("the arrival must join the total and be counted: ended %d/%d after %d calls", last.Done, last.Total, calls.Load())
	}
	for _, st := range m {
		if !st.Open {
			t.Fatalf("a drain's total can grow, so its stage is Open: %+v", st)
		}
	}
}

// A capped drain (Max) counts only what it will attempt, so it ends at its total, not short.
func TestDrainProgressHonoursMax(t *testing.T) {
	s := newStore(t)
	var calls atomic.Int32
	w := drainWorker(s, func(ctx context.Context, model, prompt, input string) (string, error) {
		calls.Add(1)
		return (&safeMiner{}).run(ctx, model, prompt, input)
	})
	for _, id := range []string{"a", "b", "c"} {
		capture(t, s, id, "user", "turn-"+id)
	}
	pending := func() []string { p, _ := s.PendingSessions([]string{"default"}); return p }
	for _, conc := range []int{1, 2} {
		ev := &drainEvents{t: t}
		w.Drain(context.Background(), DrainOpts{Conc: conc, Max: 1, Pending: pending, Progress: ev.record})
		m := ev.mining(t)
		if last := m[len(m)-1]; last.Total != 1 || last.Done != 1 {
			t.Errorf("conc=%d: Max 1 must read 1/1, ended %d/%d", conc, last.Done, last.Total)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("two capped drains, one session each: %d calls", calls.Load())
	}
}

// Nothing pending says nothing: no 0/0 snapshot for a caller to draw.
func TestDrainProgressSilentWhenNothingPending(t *testing.T) {
	s := newStore(t)
	w := drainWorker(s, (&safeMiner{}).run)
	ev := &drainEvents{t: t}
	w.Drain(context.Background(), DrainOpts{Conc: 2, Pending: func() []string { return nil }, Progress: ev.record})
	if len(ev.evs) != 0 {
		t.Fatalf("an empty drain reported progress: %+v", ev.evs)
	}
}
