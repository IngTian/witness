function eventType(event) {
  return String(event?.type || "")
}

function spawnWitness(args, payload) {
  if (process.env.WITNESS_WORKER === "1") return
  try {
    const proc = Bun.spawn([SHIM, ...args], {
      stdin: payload ? new Blob([JSON.stringify(payload)]) : "ignore",
      stdout: "ignore",
      stderr: "ignore",
      env: { ...process.env, WITNESS_OPENCODE_PLUGIN: "1" },
    })
    proc.unref?.()
  } catch {
    // Plugins must never break an OpenCode session.
  }
}

function capture(event) {
  spawnWitness(["capture", "--agent", "opencode"], event)
}

function kickWorker() {
  spawnWitness(["distill", "start", "--quiet"])
}

export const ClaudeWitness = async () => ({
  event: async ({ event }) => {
    if (process.env.WITNESS_WORKER === "1") return
    const type = eventType(event)
    if (type.startsWith("message.updated")) {
      capture(event)
      return
    }
    if (type.startsWith("session.idle")) {
      capture(event)
      kickWorker()
    }
  },
})
