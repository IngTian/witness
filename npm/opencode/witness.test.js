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
    `export function platformWitnessBin() { return globalThis.__witnessTestHarness.platformBin?.() || "" }`,
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
  const { mod, restore } = await loadPlugin(harness)
  try {
    const hooks = await mod.default()
    const input = {}
    await hooks.config(input)
    await hooks.event({ event: { type: "session.idle" } })
    await hooks.dispose()
    assert.deepEqual(input, {})
  } finally {
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

    await hooks.event({ event: { type: "session.idle" } })
    await Promise.resolve()
    assert.equal(events.filter((event) => event.includes(" import ")).length, 2)

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
    assert.deepEqual(cleared.map((timer) => timer.delay), [2000])
  } finally {
    await restore()
    globalThis.setTimeout = realSetTimeout
    globalThis.clearTimeout = realClearTimeout
  }
})

test("npm plugin dispose kills in-flight imports, stops downloader, clears retry timer, then stops auto workers", async () => {
  const events = []
  const timers = []
  const realSetTimeout = globalThis.setTimeout
  const realClearTimeout = globalThis.clearTimeout
  let importCount = 0
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
        events.push(`spawn:import-${importCount}`)
        return makeProc(`import-${importCount}`, events)
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
    await hooks.event({ event: { type: "session.idle" } })
    timers.push({ delay: 1000 })
    await hooks.dispose()

    const kill1 = events.indexOf("kill:import-1")
    const kill2 = events.indexOf("kill:import-2")
    const downloadStop = events.indexOf("download-stop")
    const distillStop = events.indexOf("spawn:distill-stop")
    assert.notEqual(kill1, -1)
    assert.notEqual(kill2, -1)
    assert.notEqual(downloadStop, -1)
    assert.notEqual(distillStop, -1)
    assert.ok(kill1 < downloadStop)
    assert.ok(kill2 < downloadStop)
    assert.ok(downloadStop < distillStop)
  } finally {
    await restore()
    globalThis.setTimeout = realSetTimeout
    globalThis.clearTimeout = realClearTimeout
  }
})
