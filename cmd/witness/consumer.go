package main

// drainQueue is the single-flight consumer's core loop. It processes every job
// the `pending` source reports, exactly once per run, re-scanning after each pass
// so jobs that ARRIVE mid-run are still picked up. It terminates when a scan turns
// up nothing not-yet-attempted — so a job that stays pending (e.g. one being
// dead-lettered) is attempted once here, not spun on forever.
//
// The caller holds the single-flight lock for the duration, so only one of these
// runs at a time across the whole machine; extra triggers no-op instead of piling
// up as blocked processes.
func drainQueue(pending func() []string, process func(string)) {
	_ = drainQueueLimit(pending, process, 0)
}

// drainQueueLimit is drainQueue with an optional process budget. max <= 0 means
// unbounded. It is used for runners that cannot safely create many nested agent
// sessions in one background pass.
func drainQueueLimit(pending func() []string, process func(string), max int) int {
	attempted := map[string]bool{}
	processed := 0
	for {
		var next []string
		for _, job := range pending() {
			if !attempted[job] {
				next = append(next, job)
			}
		}
		if len(next) == 0 {
			return processed
		}
		for _, job := range next {
			if max > 0 && processed >= max {
				return processed
			}
			attempted[job] = true
			process(job)
			processed++
		}
	}
}
