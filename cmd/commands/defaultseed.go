package commands

import (
	"log/slog"
	"slices"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// This file owns the "default" lens's seed + migration (#44 slice 1a). Since 1a the
// default lens is an ORDINARY registered lens, not an always-on built-in. Two paths
// bring it into an archive:
//
//   - MIGRATION (automatic, here): an archive that predates 1a distilled under the old
//     always-on default. On the first Open after upgrading, retro-register+enable
//     default so the install keeps working exactly as before — the user loses nothing.
//   - SCAFFOLD (explicit, install/init — slice-1a step 6): a fresh tool install seeds
//     default by consent; `init --data` (library mode) does NOT. Both call seedDefaultLens.
//
// The logic lives in cmd (not store.Open) because seeding copies the BUNDLED default
// prompts (lens.DefaultSeedDir) into the registry, needing internal/lens + internal/
// bundle — and store, the bottom of the stack, imports nothing internal. store exposes
// the injection point store.SeedDefaultLens, wired below.

func init() { store.SeedDefaultLens = migrateDefaultLensOnOpen }

// migrateDefaultLensOnOpen is the store.SeedDefaultLens hook: it runs once at the end
// of every store.Open. It ONLY acts on a pre-1a archive (HasLegacyDefaultData) that
// hasn't already been migrated (default not yet registered) — so it is idempotent, a
// no-op on fresh installs (install/init owns their scaffolding) and on library-mode
// archives that never ran default. Best-effort: a failure is logged, never fatal to
// Open (matching the other Open one-shots) — worst case the user re-seeds via
// `witness lens register default`.
func migrateDefaultLensOnOpen(st *store.Store) {
	if !st.HasLegacyDefaultData() {
		return // fresh, or a library archive that never ran default → nothing to migrate
	}
	if slices.Contains(st.RegisteredLenses(), store.LensDefault) {
		return // already migrated (idempotent)
	}
	if err := seedDefaultLens(st); err != nil {
		slog.Warn("could not migrate the built-in default lens into the registry; re-seed with `witness lens register default`",
			"err", err)
		return
	}
	slog.Info("migrated the built-in default lens into the registry as an ordinary enabled lens (#44 slice 1a)")
}

// seedDefaultLens registers the bundled default prompts into the archive's registry as
// the "default" lens and enables it. Shared by the auto-migration above and the
// install/init scaffold path. Idempotent-safe to call when default already exists
// (RegisterLens overwrites the same definition; EnableLens is idempotent).
func seedDefaultLens(st *store.Store) error {
	if err := st.RegisterLens(store.LensDefault, lens.DefaultSeedDir()); err != nil {
		return err
	}
	return st.EnableLens(store.LensDefault)
}
