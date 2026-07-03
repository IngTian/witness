function eventType(event) {
  return String(event?.type || "")
}

function eventInfo(event) {
  return event?.info || event?.properties?.info || event?.message || event?.properties?.message || {}
}

function sessionID(event) {
  return event?.sessionID || event?.properties?.sessionID || eventInfo(event)?.sessionID || event?.part?.sessionID || event?.properties?.part?.sessionID || ""
}

function completedAssistantMessage(event) {
  const info = eventInfo(event)
  return info?.role === "assistant" && Boolean(info?.time?.completed || info?.completed || info?.finish)
}

function sync(args) {
  if (process.env.WITNESS_WORKER === "1") return
  try {
    const proc = Bun.spawn([SHIM, "opencode-sync", ...args], {
      stdin: "ignore",
      stdout: "ignore",
      stderr: "ignore",
      env: { ...process.env, WITNESS_OPENCODE_PLUGIN: "1" },
    })
    proc.unref?.()
  } catch {
    // Plugins must never break an OpenCode session.
  }
}

export const ClaudeWitness = async () => ({
  event: async ({ event }) => {
    if (process.env.WITNESS_WORKER === "1") return
    const type = eventType(event)
    if (type.startsWith("server.connected")) {
      sync([])
      return
    }
    if (type.startsWith("message.updated") && completedAssistantMessage(event)) {
      const id = sessionID(event)
      if (id) sync([id])
      return
    }
    if (type.startsWith("session.idle")) {
      const id = sessionID(event)
      if (id) sync([id])
    }
  },
})
