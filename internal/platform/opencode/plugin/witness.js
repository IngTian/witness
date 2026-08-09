const WITNESS_BIN = globalThis.WITNESS_SHIM || process.env.WITNESS_BIN || "witness"
const IMPORT_GRACE_MS = 5000
const QUIET_PERIOD_MS = 5 * 60 * 1000

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
      env: { ...process.env },
    })
    proc.unref?.()
    return proc
  } catch {
    // Plugins must never break an OpenCode session.
  }
}

function syncOpenCode(sessions = []) {
  const args = ["import", "--agent", "opencode", "--quiet", "--no-kick"]
  for (const sessionID of sessions) args.push("--session", sessionID)
  return spawnWitness(args)
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
  let disposing = false
  let disposePromise = null
  let quietTimer = null
  let activeImport = null
  let activeWaiters = null
  let globalImportPending = false
  const pendingSessions = new Set()
  const sessionWaiters = new Map()
  const idleCycles = new Map()
  const idleWaiters = []
  const clearQuietTimer = () => {
    if (quietTimer) clearTimeout(quietTimer)
    quietTimer = null
  }
  const scheduleQuietWorker = () => {
    clearQuietTimer()
    quietTimer = setTimeout(() => {
      quietTimer = null
      if (disposed || disposing) return
      // worker-kick, NOT `worker-run --auto`: dispose below runs `worker stop
      // --auto-only`, which latches a durable worker_stop_requested flag that an auto
      // worker refuses to run under. Only the shared kick gate clears it, so spawning
      // worker-run directly would no-op forever after the first OpenCode close.
      spawnWitness(["worker-kick"])
    }, QUIET_PERIOD_MS)
    quietTimer.unref?.()
  }
  const claimWaiters = (sessions) => {
    const claimed = new Map()
    for (const sessionID of sessions) {
      claimed.set(sessionID, sessionWaiters.get(sessionID) || [])
      sessionWaiters.delete(sessionID)
    }
    return claimed
  }
  const resolveWaiters = (waiters) => {
    for (const resolves of waiters.values()) {
      for (const resolve of resolves) resolve()
    }
  }
  const queueIdle = () => !activeImport && !globalImportPending && pendingSessions.size === 0
  const notifyIdle = () => {
    if (!queueIdle()) return
    while (idleWaiters.length) idleWaiters.pop()()
  }
  const waitForIdle = () => queueIdle() ? Promise.resolve() : new Promise((resolve) => idleWaiters.push(resolve))
  const drain = () => {
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
  const sync = () => {
    globalImportPending = true
    drain()
  }
  const syncSessions = (sessionID) => {
    pendingSessions.add(sessionID)
    const done = new Promise((resolve) => {
      const waiters = sessionWaiters.get(sessionID) || []
      waiters.push(resolve)
      sessionWaiters.set(sessionID, waiters)
    })
    drain()
    return done
  }
  sync()
  return {
    dispose: async () => {
      if (disposePromise) return disposePromise
      disposing = true
      clearQuietTimer()
      idleCycles.clear()
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
        // From-source installs do not own the model downloader, but they still own
        // automatic worker starts from plugin events. Stop only auto workers so a
        // manual `witness worker run --detach` keeps running after OpenCode closes.
        const proc = spawnWitness(["worker", "stop", "--auto-only"])
        await proc?.exited?.catch?.(() => {})
      })()
      return disposePromise
    },
    event: async ({ event }) => {
      if (disposed || disposing || process.env.WITNESS_WORKER === "1") return
      const type = eventType(event)
      const sessionID = event?.properties?.sessionID
      const modernIdle = type === "session.status" && event?.properties?.status?.type === "idle"
      if ((type === "session.idle" || modernIdle) && sessionID) {
        const existing = idleCycles.get(sessionID)
        if (existing) return existing
        const done = syncSessions(sessionID)
        idleCycles.set(sessionID, done)
        scheduleQuietWorker()
        return done
      }
      const sessionActivity = sessionID && (type.startsWith("message.") || type.startsWith("session."))
      if (sessionActivity) {
        if (sessionID) idleCycles.delete(sessionID)
        clearQuietTimer()
      }
    },
  }
}

export default plugin
export const Witness = plugin
export const ClaudeWitness = plugin
