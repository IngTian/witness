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

function syncOpenCode() {
  return spawnWitness(["import", "--agent", "opencode", "--quiet", "--auto"])
}

function waitForExit(proc) {
  if (!proc) return Promise.resolve()
  if (typeof proc.exited?.then === "function") return proc.exited.catch(() => {})
  if (proc.exitCode !== null && proc.exitCode !== undefined) return Promise.resolve()
  return new Promise((resolve) => {
    proc.once?.("exit", resolve)
    proc.once?.("error", resolve)
  })
}

const plugin = async () => {
  let disposed = false
  const imports = new Set()
  const sync = () => {
    const proc = syncOpenCode()
    if (!proc) return
    imports.add(proc)
    waitForExit(proc).finally(() => imports.delete(proc))
  }
  sync()
  return {
    dispose: async () => {
      disposed = true
      const activeImports = [...imports]
      for (const proc of activeImports) {
        if (!proc.killed && proc.exitCode === null) proc.kill?.()
      }
      await Promise.all(activeImports.map(waitForExit))
      // From-source installs do not own the model downloader, but they still own
      // automatic worker starts from plugin events. Stop only auto workers so a
      // manual `witness distill start` keeps running after OpenCode closes.
      const proc = spawnWitness(["distill", "stop", "--auto-only"])
      await proc?.exited?.catch?.(() => {})
    },
    event: async ({ event }) => {
      if (disposed || process.env.WITNESS_WORKER === "1") return
      const type = eventType(event)
      if (type === "session.idle") sync()
    },
  }
}

export default plugin
export const Witness = plugin
export const ClaudeWitness = plugin
