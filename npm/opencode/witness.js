import { existsSync } from "node:fs"
import path from "node:path"
import { fileURLToPath } from "node:url"
import { modelDir, modelReady, startModelDownload } from "./bin/model.js"

const PACKAGE_ROOT = path.dirname(fileURLToPath(import.meta.url))

function bundledWitnessBin() {
  const os = { darwin: "darwin", linux: "linux", win32: "windows" }[process.platform]
  const arch = { x64: "amd64", arm64: "arm64" }[process.arch]
  if (!os || !arch) return ""
  const name = `witness-${os}-${arch}${os === "windows" ? ".exe" : ""}`
  const bin = path.join(PACKAGE_ROOT, "dist", name)
  return existsSync(bin) ? bin : ""
}

const WITNESS_BIN = globalThis.WITNESS_SHIM || process.env.WITNESS_BIN || bundledWitnessBin() || "witness"

function eventType(event) {
  return String(event?.type || "")
}

function spawnWitness(args, payload) {
  if (process.env.WITNESS_WORKER === "1") return
  try {
    const env = { ...process.env, WITNESS_OPENCODE_PLUGIN: "1" }
    env.WITNESS_ASSETS ||= modelDir(PACKAGE_ROOT)
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

function capture(event) {
  spawnWitness(["capture", "--agent", "opencode"], event)
}

function syncOpenCode() {
  spawnWitness(["import", "--agent", "opencode", "--quiet", "--auto"])
}

const plugin = async () => {
  let disposed = false
  // Start at most one plugin-owned model download. It is intentionally tied to
  // the plugin lifecycle so closing OpenCode stops the network/disk work; npm
  // install itself never starts a hidden 470MB transfer.
  const download = startModelDownload(PACKAGE_ROOT, {
    onExit(code) {
      // A completed model download may unblock queued raw turns. Reconcile once,
      // then let the Go-side auto gate decide whether it is allowed to run model
      // work now (cooldown, session budget, and worker liveness are enforced there).
      if (!disposed && code === 0 && modelReady(PACKAGE_ROOT)) syncOpenCode()
    },
  })
  return {
    dispose: async () => {
      disposed = true
      download?.stop()
      // Only stop automatically-started workers. A user may have explicitly run
      // `witness distill start`; closing OpenCode must not kill that manual job.
      const proc = spawnWitness(["distill", "stop", "--auto-only"])
      await proc?.exited?.catch?.(() => {})
    },
    event: async ({ event }) => {
      if (disposed || process.env.WITNESS_WORKER === "1") return
      const type = eventType(event)
      if (type.startsWith("message.updated")) {
        capture(event)
        return
      }
      if (type.startsWith("session.idle")) {
        capture(event)
        syncOpenCode()
      }
    },
  }
}

export default plugin
export const Witness = plugin
export const ClaudeWitness = plugin
