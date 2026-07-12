import { existsSync } from "node:fs"
import { fileURLToPath } from "node:url"
import { modelDir, modelReady, promptsDir, startModelDownload } from "./bin/model.js"
import { platformWitnessBin } from "./bin/platform.js"

const PACKAGE_ROOT = fileURLToPath(new URL(".", import.meta.url))
const DOWNLOAD_RETRY_MAX = 6
const DOWNLOAD_RETRY_BASE_MS = 1000
const DOWNLOAD_RETRY_CAP_MS = 30000

const platformBin = platformWitnessBin()
const WITNESS_BIN = globalThis.WITNESS_SHIM || process.env.WITNESS_BIN || (existsSync(platformBin) ? platformBin : "")

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
  let disposed = false
  let retryTimer = null
  let download = null
  const imports = new Set()

  function clearRetry() {
    if (retryTimer) clearTimeout(retryTimer)
    retryTimer = null
  }

  function scheduleRetry(attempt) {
    if (disposed || process.env.WITNESS_SKIP_MODEL_DOWNLOAD === "1" || modelReady(PACKAGE_ROOT) || attempt > DOWNLOAD_RETRY_MAX) return
    clearRetry()
    const delay = Math.min(DOWNLOAD_RETRY_BASE_MS * (2 ** (attempt - 1)), DOWNLOAD_RETRY_CAP_MS)
    retryTimer = setTimeout(() => {
      retryTimer = null
      ensureDownload(attempt)
    }, delay)
    retryTimer.unref?.()
  }

  function ensureDownload(attempt = 0) {
    if (disposed || download || process.env.WITNESS_SKIP_MODEL_DOWNLOAD === "1" || modelReady(PACKAGE_ROOT)) return
    // Retry lock contention and transient download failures, but stop after a
    // small bounded backoff window so a broken network does not spin forever.
    const nextAttempt = attempt + 1
    download = startModelDownload(PACKAGE_ROOT, {
      onExit(code) {
        download = null
        if (disposed) return
        if (code === 0 && modelReady(PACKAGE_ROOT)) {
          syncOpenCode()
          return
        }
        scheduleRetry(nextAttempt)
      },
    })
    if (!download && !modelReady(PACKAGE_ROOT)) scheduleRetry(nextAttempt)
  }

  function syncOpenCode() {
    const proc = spawnWitness(["import", "--agent", "opencode", "--quiet", "--auto"])
    if (!proc) return
    imports.add(proc)
    waitForExit(proc).finally(() => imports.delete(proc))
    return proc
  }

  if (WITNESS_BIN) {
    ensureDownload()
    syncOpenCode()
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
      disposed = true
      clearRetry()
      const activeImports = [...imports]
      for (const proc of activeImports) {
        if (!proc.killed && proc.exitCode === null) proc.kill?.()
      }
      await Promise.all(activeImports.map((proc) => waitForExit(proc)))
      download?.stop()
      await waitForExit(download?.child)
      download = null
      // Only stop automatically-started workers. A user may have explicitly run
      // `witness distill start`; closing OpenCode must not kill that manual job.
      const proc = spawnWitness(["distill", "stop", "--auto-only"])
      await proc?.exited?.catch?.(() => {})
    },
    event: async ({ event }) => {
      if (!WITNESS_BIN || disposed || process.env.WITNESS_WORKER === "1") return
      const type = eventType(event)
      if (type === "session.idle") {
        clearRetry()
        ensureDownload()
        syncOpenCode()
      }
    },
  }
}

export default plugin
export const Witness = plugin
export const ClaudeWitness = plugin
