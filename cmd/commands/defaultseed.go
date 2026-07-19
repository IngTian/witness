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

// defaultMigratedKey is the meta flag marking that the one-shot pre-1a default-lens
// migration has already run for this archive. It is the SOLE gate: once set, the
// migration NEVER runs again, so a user who later deregisters/disables the default lens
// keeps it gone (the "run any set of lenses, including none" promise). Keying the gate
// on "did we already migrate?" — not on a re-derivable condition — is what makes the
// one-shot truly one-shot.
const defaultMigratedKey = "default_lens_migrated_v1a"

// migrateDefaultLensOnOpen is the store.SeedDefaultLens hook: a TRUE one-shot run once
// per archive at the end of store.Open, to bring a pre-1a archive (which distilled under
// the old always-on default) into the world where default is an ordinary registered
// lens — so upgrading loses nothing. It is gated ONLY by the defaultMigratedKey meta
// flag, which is set the first time the hook makes its decision (whether it seeded or
// not). CRUCIALLY it does NOT re-derive "should I seed?" from HasLegacyDefaultData on
// every Open: that condition (a progress row for 'default') persists forever, so gating
// on it would RESURRECT default every time a user deregistered it. Best-effort: a seed
// failure is logged and the flag is left UNSET so the next Open retries.
func migrateDefaultLensOnOpen(st *store.Store) {
	if st.MetaString(defaultMigratedKey) != "" {
		return // one-shot already ran (seeded or decided not to) — never touch default again
	}
	// First contact on this archive. Only a PRE-1a archive (one that already distilled
	// under the old always-on default) needs seeding; a fresh archive is install/init's
	// job, and a library archive that never ran default must stay default-free.
	if !st.HasLegacyDefaultData() {
		_ = st.SetMetaString(defaultMigratedKey, "n/a") // decided: nothing to migrate; don't re-check
		return
	}
	// Already registered (e.g. a prior partial run seeded it before the flag was set) →
	// just record the one-shot as done; do not re-register/clobber a user's edits.
	if slices.Contains(st.RegisteredLenses(), store.LensDefault) {
		_ = st.SetMetaString(defaultMigratedKey, "done")
		return
	}
	if err := seedDefaultLens(st); err != nil {
		slog.Warn("could not migrate the built-in default lens into the registry; will retry on the next start (or re-run `witness install` to restore it)",
			"err", err)
		return // leave the flag UNSET → retry next Open
	}
	_ = st.SetMetaString(defaultMigratedKey, "done")
	slog.Info("migrated the built-in default lens into the registry as an ordinary enabled lens (#44 slice 1a); disable or deregister it any time")
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
