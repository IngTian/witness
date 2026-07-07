const WITNESS_BIN = globalThis.WITNESS_SHIM || process.env.WITNESS_BIN || "witness"

function eventType(event) {
  return String(event?.type || "")
}

function spawnWitness(args, payload) {
  if (process.env.WITNESS_WORKER === "1") return
  try {
    const proc = Bun.spawn([WITNESS_BIN, ...args], {
      stdin: payload ? new Blob([JSON.stringify(payload)]) : "ignore",
      stdout: "ignore",
      stderr: "ignore",
      env: { ...process.env, WITNESS_OPENCODE_PLUGIN: "1" },
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
  return {
    dispose: async () => {
      disposed = true
      // From-source installs do not own the model downloader, but they still own
      // automatic worker starts from plugin events. Stop only auto workers so a
      // manual `witness distill start` keeps running after OpenCode closes.
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
