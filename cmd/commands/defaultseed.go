package commands

import (
	"log/slog"
	"os"
	"slices"

	"github.com/IngTian/witness/internal/lens"
	"github.com/IngTian/witness/internal/store"
)

// This file owns the "default" lens's first-open auto-seed (#44 slice 1a + #102). Since
// 1a the default lens is an ORDINARY registered lens, not an always-on built-in; since
// #102 `install` is a pure mechanism that no longer seeds content, so the default lens
// arrives through exactly ONE automatic path plus one explicit one:
//
//   - AUTO-SEED (automatic, here): the FIRST time any process opens an archive that is
//     either brand-new (a fresh personal install — install/import/doctor open the store
//     before any capture) OR predates 1a (distilled under the old always-on default),
//     register+enable default. This unifies the Claude-install and npm-OpenCode paths
//     for free (both just open the store) with zero install-specific seed code.
//   - LOAD/RESTORE (explicit): `witness lens load-default` re-seeds from any state,
//     bypassing the one-shot gate (see cmdLens "load-default").
//
// The logic lives in cmd (not store.Open) because seeding copies the BUNDLED default
// prompts (lens.DefaultSeedDir) into the registry, needing internal/lens + internal/
// bundle — and store, the bottom of the stack, imports nothing internal. store exposes
// the injection point store.SeedDefaultLens, wired below.

func init() { store.SeedDefaultLens = seedDefaultLensOnOpen }

// defaultMigratedKey is the meta flag marking that the one-shot first-open default-lens
// decision has already been made for this archive (seed, opt-out, or nothing-to-do). It
// is the SOLE gate: once set, the auto-seed NEVER runs again, so a user who later
// deregisters/disables default keeps it gone — the "default is deletable" promise (#102):
// a deliberate delete sticks across every future Open. Keying the gate on "did we
// already decide?" — not on a re-derivable condition — is what makes the one-shot truly
// one-shot and prevents resurrection. (Kept its pre-#102 name so archives already
// migrated under #99 skip re-seeding: the flag's meaning only broadened from "migrated"
// to "first-open decision made", a strict superset.)
const defaultMigratedKey = "default_lens_migrated_v1a"

// noDefaultLensEnv opts an archive OUT of the first-open auto-seed (a "declined marker",
// #102): when set to a non-empty value the hook records the opt-out in the one-shot gate
// and seeds nothing, so a library/service install (the future `witness init --data`,
// slice 1b) — or anyone who simply doesn't want the person-growth starter — begins with
// no lenses. `witness lens load-default` remains the escape hatch to add it later.
const noDefaultLensEnv = "WITNESS_NO_DEFAULT_LENS"

// seedDefaultLensOnOpen is the store.SeedDefaultLens hook: a TRUE one-shot, run once per
// archive at the end of store.Open. It brings the built-in default lens into an archive
// that WANTS it — a fresh personal install or a pre-1a archive — while never forcing it
// onto a library archive or resurrecting a deliberately-removed one. It is gated ONLY by
// the defaultMigratedKey meta flag, set the first time the hook makes ANY decision. It
// does NOT re-derive "should I seed?" on every Open from conditions that persist forever
// (a 'default' progress row, an empty registry): gating on those would RESURRECT default
// each time a user removed it. Best-effort: a seed failure is logged and the flag left
// UNSET so the next Open retries.
func seedDefaultLensOnOpen(st *store.Store) {
	if st.MetaString(defaultMigratedKey) != "" {
		return // one-shot already decided — never touch default again (delete stays deleted)
	}
	// Explicit opt-out (library/service install, or a user who declines the starter):
	// record it durably so a later Open with the env unset does not silently seed.
	if os.Getenv(noDefaultLensEnv) != "" {
		_ = st.SetMetaString(defaultMigratedKey, "opted-out")
		return
	}
	// Complete an INCOMPLETE PRIOR SEED first. A partial seed (RegisterLens succeeded but
	// EnableLens failed, leaving the marker unset for retry) shows up as: marker unset AND
	// default already registered. Because ANY command Opens the store and this hook stamps
	// the marker on its first decision, a user cannot deregister/disable default before the
	// marker is set — so "unset marker + registered default" is unambiguously an unfinished
	// seed, never a user's choice. Finish it (idempotently ensure registered+enabled) and
	// only then stamp the marker. Doing this BEFORE the wantSeed gate matters: once default
	// is registered, `IsEmptyArchive() && 0-registered` is false, so a fresh archive's
	// partial seed would otherwise fall into the !wantSeed "n/a" branch and be left
	// registered-but-DISABLED with the one-shot burned.
	if slices.Contains(st.RegisteredLenses(), store.LensDefault) {
		if err := seedDefaultLens(st); err != nil {
			slog.Warn("could not finish seeding the built-in default lens; will retry on the next start (or run `witness lens load-default`)",
				"err", err)
			return // leave the flag UNSET → retry next Open
		}
		_ = st.SetMetaString(defaultMigratedKey, "done")
		return
	}
	// Decide whether this archive should get default:
	//   - PRE-1a archive (a 'default' progress row) → migrate, so an existing install
	//     keeps working exactly as before; OR
	//   - BRAND-NEW archive (no raw, no watermarks) with an EMPTY registry → a fresh
	//     personal install auto-seeds the starter lens.
	// A library archive that already ingested records + ran its own domain lens is
	// neither (not empty, no legacy default), so it does not get default forced onto it.
	wantSeed := st.HasLegacyDefaultData() ||
		(st.IsEmptyArchive() && len(st.RegisteredLenses()) == 0)
	if !wantSeed {
		_ = st.SetMetaString(defaultMigratedKey, "n/a") // decided: don't seed; don't re-check
		return
	}
	if err := seedDefaultLens(st); err != nil {
		slog.Warn("could not seed the built-in default lens into the registry; will retry on the next start (or run `witness lens load-default` to add it)",
			"err", err)
		return // leave the flag UNSET → retry next Open
	}
	_ = st.SetMetaString(defaultMigratedKey, "done")
	slog.Info("seeded the built-in default lens into the registry as an ordinary enabled lens (#44 slice 1a); disable or deregister it any time — it will not come back")
}

// seedDefaultLens registers the bundled default prompts into the archive's registry as
// the "default" lens and enables it. Shared by the auto-seed above and the explicit
// `witness lens load-default` restore. Idempotent-safe to call when default already
// exists (RegisterLens overwrites the same definition; EnableLens is idempotent).
func seedDefaultLens(st *store.Store) error {
	if err := st.RegisterLens(store.LensDefault, lens.DefaultSeedDir()); err != nil {
		return err
	}
	return st.EnableLens(store.LensDefault)
}
