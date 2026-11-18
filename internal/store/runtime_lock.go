package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RuntimeLock serializes a runner's login refresh across workers and lens try.
// Unlike config's best-effort waiting lock this must NEVER proceed unlocked.
// The OS releases it on process death; the lock file is deliberately not removed.
func RuntimeLock(ctx context.Context, root string) (func(), error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(filepath.Join(root, ".login.lock"), os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, fmt.Errorf("runtime lock: %w", err)
		}
		if unlock, ok := flockFile(f); ok {
			return unlock, nil
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
