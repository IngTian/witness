import { existsSync } from "node:fs"
import { fileURLToPath } from "node:url"
import { modelDir, modelReady, promptsDir, startModelDownload } from "./bin/model.js"
import { platformPackage, platformWitnessBin, supportedPlatforms } from "./bin/platform.js"

const PACKAGE_ROOT = fileURLToPath(new URL(".", import.meta.url))
const DOWNLOAD_RETRY_MAX = 6
const DOWNLOAD_RETRY_BASE_MS = 1000
const DOWNLOAD_RETRY_CAP_MS = 30000
const IMPORT_GRACE_MS = 5000

const platformBin = platformWitnessBin()
const WITNESS_BIN = globalThis.WITNESS_SHIM || process.env.WITNESS_BIN || (existsSync(platformBin) ? platformBin : "")

// Warn ONCE, non-throwing, when the plugin is genuinely inert: no witness binary
// resolved for this platform/arch. Every hook below early-returns on an empty
// WITNESS_BIN, so without this a user on darwin-x64 / linux-arm64 / win32 (no
// matching optional package) sees the plugin load with zero diagnostic and
// believes witness is capturing when nothing runs (issue #54 I4). Mirrors the
// clear message the CLI (bin/witness.js) already prints. Suppressed inside the
// worker subprocess (WITNESS_WORKER=1) so the recursion guard stays quiet, and a
// warn must never throw — capture/plugin code must never break an OpenCode session.
function warnIfInert() {
  if (WITNESS_BIN || process.env.WITNESS_WORKER === "1") return
  try {
    const pkg = platformPackage()
    const reason = pkg ? `optional package ${pkg} is not installed` : `unsupported platform ${process.platform}/${process.arch}`
    console.warn(`witness: plugin inactive — ${reason}; nothing is being captured. Supported platforms: ${supportedPlatforms()}`)
  } catch {
    // Never let a diagnostic break the session.
  }
}

function spawnWitness(args, payload) {
  if (!WITNESS_BIN || process.env.WITNESS_WORKER === "1") return
  try {
    const env = { ...process.env, WITNESS_OPENCODE_PLUGIN: "1" }
    env.WITNESS_ASSETS ||= modelDir(PACKAGE_ROOT)
    // Point the binary at THIS package's bundled prompts. The binary lives in a
    // separate per-platform package, so its exe-relative probe can't find them —
    // without this, LoadDefault fails and no distillation happens. Mirrors WITNESS_ASSETS.
    env.WITNESS_PROMPTS ||= promptsDir(PACKAGE_ROOT)
    // Bind distillation to OpenCode for the npm user, who never runs `witness
    // install` (so their config carries the default runner="claude" but they have
    // no `claude` CLI). Non-persistent fallback: an explicit `install` choice
    // still wins (see ResolveRunner). Don't clobber a value the user already set.
    env.WITNESS_RUNNER ||= "opencode"
    const proc = Bun.spawn([WITNESS_BIN, ...args], {
      stdin: payload ? new Blob([JSON.stringify(payload)]) : "ignore",
      stdout: "ignore",
      stderr: "ignore",
      env,
    })
    proc.unref?.()
    return proc
  } catch {
    // Plugins must never break an OpenCode session.
  }
}

function eventType(event) {
  return String(event?.type || "")
}

function waitForExit(child) {
  if (!child) return Promise.resolve()
  if (typeof child.exited?.then === "function") return child.exited.catch(() => {})
  if (child.exitCode !== null && child.exitCode !== undefined) return Promise.resolve()
  return new Promise((resolve) => {
    child.once?.("exit", () => resolve())
    child.once?.("error", () => resolve())
  })
}

const plugin = async () => {
  warnIfInert()
  let disposed = false
  let disposing = false
  let disposePromise = null
  let retryTimer = null
  let download = null
  let activeImport = null
  let activeWaiters = null
  let globalImportPending = false
  const pendingSessions = new Set()
  const sessionWaiters = new Map()
  const modernIdleWaiters = new Map()
  const idleWaiters = []

  function clearRetry() {
    if (retryTimer) clearTimeout(retryTimer)
    retryTimer = null
  }

  function scheduleRetry(attempt) {
    if (disposed || disposing || process.env.WITNESS_SKIP_MODEL_DOWNLOAD === "1" || modelReady(PACKAGE_ROOT) || attempt > DOWNLOAD_RETRY_MAX) return
    clearRetry()
    const delay = Math.min(DOWNLOAD_RETRY_BASE_MS * (2 ** (attempt - 1)), DOWNLOAD_RETRY_CAP_MS)
    retryTimer = setTimeout(() => {
      retryTimer = null
      ensureDownload(attempt)
    }, delay)
    retryTimer.unref?.()
  }

  function ensureDownload(attempt = 0) {
    if (disposed || disposing || download || process.env.WITNESS_SKIP_MODEL_DOWNLOAD === "1" || modelReady(PACKAGE_ROOT)) return
    // Retry lock contention and transient download failures, but stop after a
    // small bounded backoff window so a broken network does not spin forever.
    const nextAttempt = attempt + 1
    download = startModelDownload(PACKAGE_ROOT, {
      onExit(code) {
        download = null
        if (disposed || disposing) return
        if (code === 0 && modelReady(PACKAGE_ROOT)) {
          sync()
          return
        }
        scheduleRetry(nextAttempt)
      },
    })
    if (!download && !modelReady(PACKAGE_ROOT)) scheduleRetry(nextAttempt)
  }

  function syncOpenCode(sessions = []) {
    const args = ["import", "--agent", "opencode", "--quiet", "--auto"]
    for (const sessionID of sessions) args.push("--session", sessionID)
    const proc = spawnWitness(args)
    if (!proc) return
    return proc
  }

  function claimWaiters(sessions) {
    const claimed = new Map()
    for (const sessionID of sessions) {
      claimed.set(sessionID, sessionWaiters.get(sessionID) || [])
      sessionWaiters.delete(sessionID)
    }
    return claimed
  }

  function resolveWaiters(waiters) {
    for (const resolves of waiters.values()) {
      for (const resolve of resolves) resolve()
    }
  }

  function queueIdle() {
    return !activeImport && !globalImportPending && pendingSessions.size === 0
  }

  function notifyIdle() {
    if (!queueIdle()) return
    while (idleWaiters.length) idleWaiters.pop()()
  }

  function waitForIdle() {
    return queueIdle() ? Promise.resolve() : new Promise((resolve) => idleWaiters.push(resolve))
  }

  function drain() {
    if (disposed || activeImport) return
    const coveredSessions = [...pendingSessions]
    if (!globalImportPending && coveredSessions.length === 0) return
    const sessions = globalImportPending ? [] : coveredSessions
    globalImportPending = false
    pendingSessions.clear()
    const batchWaiters = claimWaiters(coveredSessions)
    const proc = syncOpenCode(sessions)
    if (!proc) {
      resolveWaiters(batchWaiters)
      drain()
      notifyIdle()
      return
    }
    activeImport = proc
    activeWaiters = batchWaiters
    waitForExit(proc).then(() => resolveWaiters(batchWaiters)).finally(() => {
      if (activeImport === proc) {
        activeImport = null
        activeWaiters = null
      }
      drain()
      notifyIdle()
    })
  }

  function sync() {
    globalImportPending = true
    drain()
  }

  function syncSessions(sessionID) {
    pendingSessions.add(sessionID)
    const done = new Promise((resolve) => {
      const waiters = sessionWaiters.get(sessionID) || []
      waiters.push(resolve)
      sessionWaiters.set(sessionID, waiters)
    })
    drain()
    return done
  }

  if (WITNESS_BIN) {
    ensureDownload()
    sync()
  }
  return {
    config: async (input) => {
      if (!WITNESS_BIN) return
      input.mcp ||= {}
      if (input.mcp.witness) return
      input.mcp.witness = {
        type: "local",
        command: [WITNESS_BIN, "mcp"],
        environment: {
          WITNESS_ASSETS: modelDir(PACKAGE_ROOT),
          WITNESS_PROMPTS: promptsDir(PACKAGE_ROOT),
          WITNESS_RUNNER: "opencode",
        },
        enabled: true,
      }
    },
    dispose: async () => {
      if (disposePromise) return disposePromise
      disposing = true
      clearRetry()
      modernIdleWaiters.clear()
      disposePromise = (async () => {
        let timer
        const drained = await Promise.race([
          waitForIdle().then(() => true),
          new Promise((resolve) => {
            timer = setTimeout(() => resolve(false), IMPORT_GRACE_MS)
          }),
        ])
        clearTimeout(timer)
        if (!drained) {
          disposed = true
          resolveWaiters(claimWaiters(pendingSessions))
          pendingSessions.clear()
          globalImportPending = false
          const importProc = activeImport
          resolveWaiters(activeWaiters || new Map())
          activeWaiters = null
          if (importProc && !importProc.killed && importProc.exitCode === null) importProc.kill?.()
          await waitForExit(importProc)
        }
        disposed = true
        download?.stop()
        await waitForExit(download?.child)
        download = null
        // Only stop automatically-started workers. A user may have explicitly run
        // `witness distill start`; closing OpenCode must not kill that manual job.
        const proc = spawnWitness(["distill", "stop", "--auto-only"])
        await proc?.exited?.catch?.(() => {})
      })()
      return disposePromise
    },
    event: async ({ event }) => {
      if (!WITNESS_BIN || disposed || disposing || process.env.WITNESS_WORKER === "1") return
      const type = eventType(event)
      const sessionID = event?.properties?.sessionID
      const modernIdle = type === "session.status" && event?.properties?.status?.type === "idle"
      if (type === "session.idle" && sessionID && modernIdleWaiters.has(sessionID)) {
        const done = modernIdleWaiters.get(sessionID)
        modernIdleWaiters.delete(sessionID)
        return done
      }
      if ((type === "session.idle" || modernIdle) && sessionID) {
        clearRetry()
        ensureDownload()
        const done = syncSessions(sessionID)
        if (modernIdle) modernIdleWaiters.set(sessionID, done)
        return done
      }
      if (type === "session.status" && sessionID) modernIdleWaiters.delete(sessionID)
    },
  }
}

export default plugin
export const Witness = plugin
export const ClaudeWitness = plugin
