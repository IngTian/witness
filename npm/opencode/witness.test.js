import assert from "node:assert/strict"
import { EventEmitter } from "node:events"
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises"
import os from "node:os"
import path from "node:path"
import test from "node:test"
import { pathToFileURL } from "node:url"

const sourcePath = new URL("./witness.js", import.meta.url)
const source = await readFile(sourcePath, "utf8")

function deferred() {
  let resolve
  let reject
  const promise = new Promise((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

function makeProc(label, events, options = {}) {
  const done = deferred()
  const proc = new EventEmitter()
  proc.label = label
  proc.killed = false
  proc.exitCode = null
  proc.exited = done.promise
  proc.kill = () => {
    events.push(`kill:${label}`)
    proc.killed = true
    proc.exitCode = options.killCode ?? 0
    done.resolve(proc.exitCode)
    proc.emit("exit", proc.exitCode)
  }
  proc.finish = (code = 0) => {
    proc.exitCode = code
    done.resolve(code)
    proc.emit("exit", code)
  }
  if (options.autoExit !== undefined) queueMicrotask(() => proc.finish(options.autoExit))
  return proc
}

async function loadPlugin(harness) {
  const dir = await mkdtemp(path.join(os.tmpdir(), "witness-opencode-plugin-"))
  await mkdir(path.join(dir, "bin"), { recursive: true })
  await writeFile(path.join(dir, "witness.js"), source)
  await writeFile(
    path.join(dir, "bin", "model.js"),
    `
      const harness = globalThis.__witnessTestHarness
      export function modelDir() { return harness.modelDir() }
      export function modelReady() { return harness.modelReady() }
      export function promptsDir(packageRoot) { return (harness.promptsDir || ((r) => r + "/prompts"))(packageRoot) }
      export function startModelDownload(packageRoot, options = {}) { return harness.startModelDownload(packageRoot, options) }
    `,
  )
  await writeFile(
    path.join(dir, "bin", "platform.js"),
    `
      export function platformWitnessBin() { return globalThis.__witnessTestHarness.platformBin?.() || "" }
      export function platformPackage() { return globalThis.__witnessTestHarness.platformPackage?.() ?? "" }
      export function supportedPlatforms() { return "macOS Apple Silicon (darwin/arm64) and Linux x86-64 (linux/x64)" }
    `,
  )

  const previous = {
    Bun: globalThis.Bun,
    WITNESS_SHIM: globalThis.WITNESS_SHIM,
    WITNESS_BIN: process.env.WITNESS_BIN,
    harness: globalThis.__witnessTestHarness,
  }
  globalThis.__witnessTestHarness = harness
  globalThis.WITNESS_SHIM = Object.hasOwn(harness, "shim") ? harness.shim : "/shim/witness"
  if (Object.hasOwn(harness, "witnessBin")) process.env.WITNESS_BIN = harness.witnessBin
  globalThis.Bun = {
    spawn(args, options) {
      return harness.spawn(args, options)
    },
  }

  const mod = await import(`${pathToFileURL(path.join(dir, "witness.js")).href}?t=${Date.now()}-${Math.random()}`)
  return {
    dir,
    mod,
    async restore() {
      globalThis.Bun = previous.Bun
      globalThis.WITNESS_SHIM = previous.WITNESS_SHIM
      if (previous.WITNESS_BIN === undefined) delete process.env.WITNESS_BIN
      else process.env.WITNESS_BIN = previous.WITNESS_BIN
      globalThis.__witnessTestHarness = previous.harness
      await rm(dir, { recursive: true, force: true })
    },
  }
}

test("npm plugin stays inactive when no supported platform binary is available", async () => {
  const harness = {
    shim: "",
    witnessBin: "",
    platformBin: () => "",
    platformPackage: () => "", // unsupported platform: no matching optional package
    modelDir: () => "/assets/e5-small",
    modelReady() {
      throw new Error("unsupported platforms should not inspect the model")
    },
    startModelDownload() {
      throw new Error("unsupported platforms should not download the model")
    },
    spawn() {
      throw new Error("unsupported platforms should not spawn witness")
    },
  }
  const warnings = []
  const realWarn = console.warn
  console.warn = (msg) => warnings.push(String(msg))
  const { mod, restore } = await loadPlugin(harness)
  try {
    const hooks = await mod.default()
    const input = {}
    await hooks.config(input)
    await hooks.event({ event: { type: "session.idle" } })
    await hooks.dispose()
    assert.deepEqual(input, {})
    // #54 I4: the inert plugin must warn once (not silently do nothing), so the
    // user does not falsely believe witness is capturing.
    assert.equal(warnings.length, 1, "inert plugin should warn exactly once at init")
    assert.match(warnings[0], /plugin inactive/)
    assert.match(warnings[0], /unsupported platform/)
    assert.match(warnings[0], /nothing is being captured/)
  } finally {
    console.warn = realWarn
    await restore()
  }
})

test("npm plugin does not warn when a binary is available", async () => {
  const events = []
  const harness = {
    modelDir: () => "/assets/e5-small",
    modelReady: () => true,
    startModelDownload() {
      throw new Error("download should not start when model is ready")
    },
    spawn(args) {
      events.push(`spawn:${args.join(" ")}`)
      return makeProc(`proc-${events.length}`, events, { autoExit: 0 })
    },
  }
  const warnings = []
  const realWarn = console.warn
  console.warn = (msg) => warnings.push(String(msg))
  const { mod, restore } = await loadPlugin(harness)
  try {
    const hooks = await mod.default()
    await hooks.dispose()
    assert.equal(warnings.length, 0, "an active plugin (WITNESS_BIN present) must stay silent")
  } finally {
    console.warn = realWarn
    await restore()
  }
})

test("npm plugin reconciles on init/idle, ignores message.updated, and auto-registers MCP only when absent", async () => {
  const events = []
  const importEnvs = []
  const harness = {
    modelDir: () => "/assets/e5-small",
    modelReady: () => true,
    promptsDir: () => "/pkg/prompts",
    startModelDownload() {
      throw new Error("download should not start when model is ready")
    },
    spawn(args, options) {
      events.push(`spawn:${args.join(" ")}`)
      if (args[1] === "import") {
        importEnvs.push(options?.env)
        return makeProc(`import-${events.length}`, events, { autoExit: 0 })
      }
      return makeProc(`proc-${events.length}`, events, { autoExit: 0 })
    },
  }
  const { mod, restore } = await loadPlugin(harness)
  try {
    assert.equal(mod.default, mod.Witness)
    assert.equal(mod.default, mod.ClaudeWitness)

    const hooks = await mod.default()
    await Promise.resolve()
    assert.equal(events.filter((event) => event.includes(" import ")).length, 1)
    // The spawned distill/import subprocess must carry the prompts override, or
    // the binary's exe-relative probe misses the main package's prompts/.
    assert.equal(importEnvs[0]?.WITNESS_PROMPTS, "/pkg/prompts")
    assert.equal(importEnvs[0]?.WITNESS_ASSETS, "/assets/e5-small")
    assert.equal(importEnvs[0]?.WITNESS_RUNNER, "opencode")

    await hooks.event({ event: { type: "message.updated" } })
    assert.equal(events.filter((event) => event.includes(" import ")).length, 1)

    await hooks.event({ event: { type: "session.status", properties: { sessionID: "ses_current", status: { type: "idle" } } } })
    await Promise.resolve()
    assert.equal(events.filter((event) => event.includes(" import ")).length, 2)
    assert.ok(events.some((event) => event.includes("--session ses_current")))

    const input = {}
    await hooks.config(input)
    assert.equal(input.mcp.witness.type, "local")
    assert.deepEqual(input.mcp.witness.command, ["/shim/witness", "mcp"])
    assert.deepEqual(input.mcp.witness.environment, {
      WITNESS_ASSETS: "/assets/e5-small",
      WITNESS_PROMPTS: "/pkg/prompts",
      WITNESS_RUNNER: "opencode",
    })
    assert.equal(input.mcp.witness.enabled, true)

    const existing = { mcp: { witness: { type: "local", command: ["custom"] } } }
    await hooks.config(existing)
    assert.deepEqual(existing.mcp.witness, { type: "local", command: ["custom"] })

    await hooks.dispose()
  } finally {
    await restore()
  }
})

test("npm plugin waits for idle batches, deduplicates modern/legacy pairs, and drains later arrivals", async () => {
  const events = []
  const imports = []
  const harness = {
    modelDir: () => "/assets/e5-small",
    modelReady: () => true,
    startModelDownload() {
      throw new Error("download should not start when model is ready")
    },
    spawn(args) {
      if (args[1] === "import") {
        events.push(`spawn:${args.join(" ")}`)
        const proc = makeProc(`import-${imports.length + 1}`, events)
        imports.push(proc)
        return proc
      }
      return makeProc("distill", events, { autoExit: 0 })
    },
  }
  const { mod, restore } = await loadPlugin(harness)
  try {
    const hooks = await mod.default()
    assert.equal(imports.length, 1, "initial full reconcile should start immediately")
    assert.doesNotMatch(events[0], /--session/, "initial reconcile must be global")

    await hooks.event({ event: { type: "session.status", properties: { sessionID: "ses_busy", status: { type: "busy" } } } })
    await hooks.event({ event: { type: "session.status", properties: { sessionID: "ses_retry", status: { type: "retry" } } } })
    assert.equal(imports.length, 1, "non-idle statuses must not import")

    let modernResolved = false
    let legacyResolved = false
    const modern = hooks.event({ event: { type: "session.status", properties: { sessionID: "ses_a", status: { type: "idle" } } } })
    const legacy = hooks.event({ event: { type: "session.idle", properties: { sessionID: "ses_a" } } })
    const other = hooks.event({ event: { type: "session.idle", properties: { sessionID: "ses_b" } } })
    modern.then(() => { modernResolved = true })
    legacy.then(() => { legacyResolved = true })
    assert.equal(imports.length, 1, "idle events must queue behind the active import")
    await Promise.resolve()
    assert.equal(modernResolved, false, "queued idle must wait for its import")
    assert.equal(legacyResolved, false, "deduplicated legacy idle must wait for the same import")

    imports[0].finish()
    await new Promise((resolve) => setImmediate(resolve))
    assert.equal(imports.length, 2)
    assert.match(events.at(-1), /--session ses_a/)
    assert.match(events.at(-1), /--session ses_b/)
    assert.equal((events.at(-1).match(/--session ses_a/g) || []).length, 1, "modern and legacy idle are one logical import")
    assert.equal(modernResolved, false, "active idle import must keep its promise pending")

    const later = hooks.event({ event: { type: "session.status", properties: { sessionID: "ses_c", status: { type: "idle" } } } })
    let sameSessionResolved = false
    const sameSession = hooks.event({ event: { type: "session.idle", properties: { sessionID: "ses_a" } } })
    sameSession.then(() => { sameSessionResolved = true })
    assert.equal(imports.length, 2, "new work must not start concurrently")
    imports[1].finish()
    await new Promise((resolve) => setImmediate(resolve))
    await Promise.all([modern, legacy, other])
    assert.equal(modernResolved, true)
    assert.equal(legacyResolved, true)
    assert.equal(sameSessionResolved, false, "same-session work queued during an active batch must remain pending")
    assert.equal(imports.length, 3, "idle arriving during import must run afterward")
    assert.match(events.at(-1), /--session ses_c/)
    assert.match(events.at(-1), /--session ses_a/)

    imports[2].finish()
    await Promise.all([later, sameSession])
    await hooks.dispose()
  } finally {
    await restore()
  }
})

test("npm plugin retries model download with bounded exponential backoff on lock contention and failure", async () => {
  const events = []
  const timers = []
  const cleared = []
  const realSetTimeout = globalThis.setTimeout
  const realClearTimeout = globalThis.clearTimeout
  let attempts = 0
  const harness = {
    modelDir: () => "/assets/e5-small",
    modelReady: () => false,
    startModelDownload(_packageRoot, options) {
      attempts += 1
      events.push(`download:${attempts}`)
      if (attempts === 1) return null
      if (attempts === 2) {
        const child = makeProc("download-child", events)
        queueMicrotask(() => {
          child.finish(1)
          options.onExit?.(1)
        })
        return {
          child,
          stop() {
            child.kill()
          },
        }
      }
      return null
    },
    spawn(args) {
      if (args[1] === "import" || args[1] === "distill") return makeProc(args[1], events, { autoExit: 0 })
      return makeProc("other", events, { autoExit: 0 })
    },
  }
  globalThis.setTimeout = (fn, delay) => {
    const timer = { fn, delay }
    timers.push(timer)
    return timer
  }
  globalThis.clearTimeout = (timer) => {
    cleared.push(timer)
  }

  const { mod, restore } = await loadPlugin(harness)
  try {
    const hooks = await mod.default()
    assert.equal(timers[0]?.delay, 1000)

    timers[0].fn()
    await Promise.resolve()
    await Promise.resolve()
    assert.equal(timers[1]?.delay, 2000)

    await hooks.dispose()
    assert.deepEqual(cleared.map((timer) => timer.delay), [2000, 5000])
  } finally {
    await restore()
    globalThis.setTimeout = realSetTimeout
    globalThis.clearTimeout = realClearTimeout
  }
})

test("npm plugin dispose drains startup and idle imports before stopping downloader and auto workers", async () => {
  const events = []
  const timers = []
  const realSetTimeout = globalThis.setTimeout
  const realClearTimeout = globalThis.clearTimeout
  let importCount = 0
  const importArgs = []
  const imports = []
  const harness = {
    modelDir: () => "/assets/e5-small",
    modelReady: () => false,
    startModelDownload() {
      const child = makeProc("download-child", events)
      return {
        child,
        stop() {
          events.push("download-stop")
          child.kill()
        },
      }
    },
    spawn(args) {
      if (args[1] === "import") {
        importCount += 1
        importArgs.push(args)
        events.push(`spawn:import-${importCount}`)
        const proc = makeProc(`import-${importCount}`, events)
        imports.push(proc)
        return proc
      }
      if (args[1] === "distill") {
        events.push("spawn:distill-stop")
        return makeProc("distill-stop", events, { autoExit: 0 })
      }
      return makeProc("other", events, { autoExit: 0 })
    },
  }
  globalThis.setTimeout = (fn, delay) => {
    const timer = { fn, delay }
    timers.push(timer)
    return timer
  }
  globalThis.clearTimeout = (timer) => {
    events.push(`clear:${timer.delay}`)
  }

  const { mod, restore } = await loadPlugin(harness)
  try {
    const hooks = await mod.default()
    const pending = hooks.event({ event: { type: "session.idle", properties: { sessionID: "ses_pending" } } })
    timers.push({ delay: 1000 })
    let disposed = false
    const dispose = hooks.dispose().then(() => { disposed = true })
    await Promise.resolve()
    assert.equal(disposed, false)
    assert.equal(events.includes("kill:import-1"), false, "graceful disposal must not kill the active startup import")

    // Finish the global startup import: the pending idle session must still drain
    // before disposal is allowed to stop the downloader or auto worker.
    imports[0].finish()
    await new Promise((resolve) => setImmediate(resolve))
    assert.equal(imports.length, 2)
    assert.ok(importArgs[1].includes("ses_pending"))
    assert.equal(disposed, false)
    assert.equal(events.includes("kill:import-2"), false, "graceful disposal must not kill the active idle import")

    imports[1].finish()
    await pending
    await dispose

    const downloadStop = events.indexOf("download-stop")
    const distillStop = events.indexOf("spawn:distill-stop")
    assert.notEqual(downloadStop, -1)
    assert.notEqual(distillStop, -1)
    assert.ok(downloadStop < distillStop)
  } finally {
    await restore()
    globalThis.setTimeout = realSetTimeout
    globalThis.clearTimeout = realClearTimeout
  }
})

test("npm plugin kills a hung import after the dispose grace period", async () => {
  const events = []
  const timers = []
  const realSetTimeout = globalThis.setTimeout
  const realClearTimeout = globalThis.clearTimeout
  let startupImport
  const harness = {
    modelDir: () => "/assets/e5-small",
    modelReady: () => true,
    startModelDownload() {
      throw new Error("download should not start when model is ready")
    },
    spawn(args) {
      if (args[1] === "import") {
        startupImport = makeProc("hung-import", events)
        return startupImport
      }
      return makeProc("distill-stop", events, { autoExit: 0 })
    },
  }
  globalThis.setTimeout = (fn, delay) => {
    const timer = { fn, delay }
    timers.push(timer)
    return timer
  }
  globalThis.clearTimeout = () => {}

  const { mod, restore } = await loadPlugin(harness)
  try {
    const hooks = await mod.default()
    let disposed = false
    const dispose = hooks.dispose().then(() => { disposed = true })
    await Promise.resolve()
    assert.equal(disposed, false)
    assert.equal(timers[0]?.delay, 5000)

    timers[0].fn()
    await dispose
    assert.equal(startupImport.killed, true)
    assert.ok(events.includes("kill:hung-import"))
  } finally {
    await restore()
    globalThis.setTimeout = realSetTimeout
    globalThis.clearTimeout = realClearTimeout
  }
})
