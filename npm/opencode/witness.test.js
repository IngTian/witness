import assert from "node:assert/strict"
import { mkdtemp, mkdir, readFile, realpath, rm, writeFile } from "node:fs/promises"
import os from "node:os"
import path from "node:path"
import test from "node:test"
import { pathToFileURL } from "node:url"

// One body, two copies: npm's witness.js (npm_prelude.js + the body, generated) and the body
// `witness wire opencode` writes with its shim line prepended. Every behaviour test runs against
// BOTH, so a change that only works in one copy fails here. The fakes follow the v2 contract
// observed on OpenCode 2.2.7 and 2.3.0 (docs/superpowers/specs/2026-11-13-opencode-v2-evidence/
// plugin.md): setup runs once per Location and again on every hot reload, every instance sees
// every event on one bus, and an event's id is the same in every instance.
const BODY = await readFile(new URL("../../internal/platform/opencode/plugin/witness.js", import.meta.url), "utf8")
const PLUGINS = [
  { name: "npm", npm: true, source: await readFile(new URL("./witness.js", import.meta.url), "utf8") },
  { name: "embedded", npm: false, source: 'globalThis.WITNESS_SHIM = "/shim/witness"\n\n' + BODY },
]
const [NPM, EMBEDDED] = PLUGINS

const ENV_KEYS = ["WITNESS_BIN", "WITNESS_WORKER", "WITNESS_SKIP_MODEL_DOWNLOAD", "WITNESS_ASSETS", "WITNESS_PROMPTS", "WITNESS_RUNNER", "WITNESS_OPENCODE_BIN"]
const QUIET_MS = 5 * 60 * 1000
const STATE = "__witnessCaptureV2"

const settle = () => new Promise((resolve) => setImmediate(resolve))

// A spawned witness that has already exited, unless the test wants to hold it.
function exitedProc() {
  return { exited: Promise.resolve(0), unref() {} }
}

// A fake v2 event bus. ctx().event.subscribe is an async iterable that receives every matching
// publish in every instance and ends when its signal aborts; ctx().mcp.transform runs the editor
// callback on a Map, the way v2's MCP config transform does.
function makeBus({ mcp = new Map() } = {}) {
  const subs = new Set()
  const bus = {
    mcp,
    subscribed: [],
    transforms: 0,
    live: () => subs.size,
    ctx() {
      return {
        event: {
          subscribe({ types, signal }) {
            bus.subscribed.push(types)
            const sub = { types, queue: [], wake: null }
            subs.add(sub)
            signal?.addEventListener("abort", () => sub.wake?.(), { once: true })
            return (async function* () {
              try {
                while (!signal?.aborted) {
                  if (sub.queue.length) {
                    yield sub.queue.shift()
                    continue
                  }
                  await new Promise((resolve) => (sub.wake = resolve))
                  sub.wake = null
                }
              } finally {
                subs.delete(sub)
              }
            })()
          },
        },
        mcp: {
          async transform(fn) {
            bus.transforms += 1
            fn({ get: (k) => mcp.get(k), set: (k, v) => mcp.set(k, v) })
          },
        },
      }
    },
    async publish(event) {
      for (const sub of subs) if (!sub.types || sub.types.includes(event.type)) sub.queue.push(event), sub.wake?.()
      await settle()
    },
  }
  return bus
}

// Fake timers for the quiet period: setTimeout records, nothing fires until the test says so.
function fakeTimers() {
  const real = { setTimeout: globalThis.setTimeout, clearTimeout: globalThis.clearTimeout }
  const timers = []
  globalThis.setTimeout = (fn, delay) => {
    const t = { fn, delay, active: true, unref() {} }
    timers.push(t)
    return t
  }
  globalThis.clearTimeout = (t) => {
    if (t) t.active = false
  }
  return {
    active: () => timers.filter((t) => t.active),
    fire(t) {
      t.active = false
      t.fn()
    },
    restore() {
      Object.assign(globalThis, real)
    },
  }
}

// loadPlugin writes source as <pkg>/witness.js beside a fake bin/platform.js (the npm prelude
// imports it), installs a fake Bun.spawn that records every spawn, and imports the module.
// load() imports another copy into the same process, reload() re-evaluates the first file the
// way a hot reload does; globalThis state survives both, as it does in OpenCode.
async function loadPlugin(harness = {}, source = NPM.source) {
  // realpath: the module URL resolves macOS /var to /private/var, and promptsDir derives from it.
  const dir = await realpath(await mkdtemp(path.join(os.tmpdir(), "witness-opencode-plugin-")))
  await mkdir(path.join(dir, "bin"), { recursive: true })
  const platformBin = path.join(dir, "bin", "witness")
  await writeFile(platformBin, "")
  await writeFile(
    path.join(dir, "bin", "platform.js"),
    `
      import path from "node:path"
      const h = () => globalThis.__witnessTestHarness
      export function platformWitnessBin() { return h().platformBin ?? "" }
      export function platformPackage() { return "" }
      export function supportedPlatforms() { return "test platforms" }
      export function modelDir() { return "/assets/e5-small" }
      export function promptsDir(packageRoot) { return path.join(packageRoot, "prompts") }
    `,
  )
  const saved = Object.fromEntries(ENV_KEYS.map((k) => [k, process.env[k]]))
  for (const k of ENV_KEYS) delete process.env[k]
  Object.assign(process.env, harness.env || {})
  const spawns = []
  globalThis.__witnessTestHarness = { platformBin: harness.platformBin === false ? "" : platformBin }
  globalThis.Bun = {
    spawn(argv, options) {
      if (harness.spawn) return harness.spawn(argv, options, spawns)
      spawns.push({ bin: argv[0], args: argv.slice(1), env: options?.env })
      return exitedProc()
    },
  }
  let n = 0
  const importFile = (file) => import(`${pathToFileURL(file).href}?t=${Date.now()}-${n++}`)
  const main = path.join(dir, "witness.js")
  await writeFile(main, source)
  return {
    dir,
    spawns,
    bin: source === NPM.source ? platformBin : "/shim/witness",
    prompts: path.join(dir, "prompts"),
    mod: await importFile(main),
    reload: () => importFile(main),
    async load(other) {
      const file = path.join(dir, `plugin-${n}.js`)
      await writeFile(file, other)
      return importFile(file)
    },
    async restore() {
      const S = globalThis[STATE]
      if (S?.quiet) clearTimeout(S.quiet)
      delete globalThis[STATE]
      delete globalThis.WITNESS_SHIM
      delete globalThis.Bun
      delete globalThis.__witnessTestHarness
      for (const k of ENV_KEYS) {
        if (saved[k] === undefined) delete process.env[k]
        else process.env[k] = saved[k]
      }
      await rm(dir, { recursive: true, force: true })
    },
  }
}

const imports = (spawns) => spawns.filter((s) => s.args[0] === "import")
const startupImports = (spawns) => imports(spawns).filter((s) => !s.args.includes("--session"))
const sessionImports = (spawns) => imports(spawns).filter((s) => s.args.includes("--session")).map((s) => s.args.slice(s.args.indexOf("--session") + 1).filter((a) => a !== "--session"))
const kicks = (spawns) => spawns.filter((s) => s.args[0] === "worker-kick")
const turn = (id, type, sessionID) => ({ id, type: `session.execution.${type}`, data: sessionID ? { sessionID } : {} })

for (const variant of PLUGINS) {
  const it = (name, fn, harness) =>
    test(`${variant.name}: ${name}`, async () => {
      const p = await loadPlugin(harness, variant.source)
      try {
        await fn(p)
      } finally {
        await p.restore()
      }
    })

  it("default export is { id: \"witness\", setup } and setup resolves to a function", async (p) => {
    assert.deepEqual(Object.keys(p.mod), ["default"], "v2 decodes only the default export; v1's named exports are gone")
    assert.deepEqual(Object.keys(p.mod.default).sort(), ["id", "setup"])
    assert.equal(p.mod.default.id, "witness")
    // 2.2.7 calls any truthy setup result at unload, so it must be a function or nothing.
    assert.equal(typeof (await p.mod.default.setup(makeBus().ctx())), "function")
  })

  it("one startup import per process across two Locations and a hot reload", async (p) => {
    const bus = makeBus()
    await p.mod.default.setup(bus.ctx())
    await p.mod.default.setup(bus.ctx())
    const reloaded = await p.reload()
    await reloaded.default.setup(bus.ctx())
    await settle()
    assert.deepEqual(startupImports(p.spawns).map((s) => [s.bin, ...s.args]), [[p.bin, "import", "--agent", "opencode", "--quiet", "--no-kick"]])
  })

  it("a turn end imports that session once although every instance sees it", async (p) => {
    const bus = makeBus()
    await p.mod.default.setup(bus.ctx())
    await p.mod.default.setup(bus.ctx())
    await bus.publish(turn("evt_1", "succeeded", "ses_a"))
    assert.deepEqual(imports(p.spawns).at(-1).args, ["import", "--agent", "opencode", "--quiet", "--no-kick", "--session", "ses_a"])
    assert.deepEqual(sessionImports(p.spawns), [["ses_a"]])
    await bus.publish(turn("evt_2", "succeeded"))
    assert.deepEqual(sessionImports(p.spawns), [["ses_a"]], "an event without data.sessionID spawns nothing")
  })

  it("a parent's partial and final turn ends both import (2.3.0 background subagents)", async (p) => {
    const bus = makeBus()
    await p.mod.default.setup(bus.ctx())
    await p.mod.default.setup(bus.ctx())
    await bus.publish(turn("evt_s1", "started", "ses_P"))
    await bus.publish(turn("evt_s2", "started", "ses_C"))
    await bus.publish(turn("evt_1", "succeeded", "ses_P"))
    await bus.publish(turn("evt_2", "succeeded", "ses_C"))
    await bus.publish(turn("evt_s3", "started", "ses_P"))
    await bus.publish(turn("evt_3", "succeeded", "ses_P"))
    assert.deepEqual(sessionImports(p.spawns), [["ses_P"], ["ses_C"], ["ses_P"]])
  })

  it("failed and interrupted turns import too", async (p) => {
    const bus = makeBus()
    await p.mod.default.setup(bus.ctx())
    await bus.publish({ ...turn("evt_f", "failed", "ses_f"), data: { sessionID: "ses_f", error: { type: "provider.invalid-request" } } })
    await bus.publish({ ...turn("evt_i", "interrupted", "ses_i"), data: { sessionID: "ses_i", reason: "user" } })
    assert.deepEqual(sessionImports(p.spawns), [["ses_f"], ["ses_i"]])
  })

  it("the quiet-period worker-kick waits for the last running session", async (p) => {
    const timers = fakeTimers()
    try {
      const bus = makeBus()
      await p.mod.default.setup(bus.ctx())
      await p.mod.default.setup(bus.ctx())
      await bus.publish(turn("evt_s1", "started", "ses_P"))
      await bus.publish(turn("evt_s2", "started", "ses_C"))
      await bus.publish(turn("evt_1", "succeeded", "ses_P"))
      assert.deepEqual(timers.active(), [], "no kick may be armed while the child still runs")
      await bus.publish(turn("evt_2", "succeeded", "ses_C"))
      const armed = timers.active()
      assert.deepEqual(armed.map((t) => t.delay), [QUIET_MS])
      timers.fire(armed[0])
      assert.deepEqual(kicks(p.spawns).map((s) => [s.bin, ...s.args]), [[p.bin, "worker-kick"]])

      await bus.publish(turn("evt_3", "succeeded", "ses_D"))
      const again = timers.active()
      assert.equal(again.length, 1)
      await bus.publish(turn("evt_s4", "started", "ses_E"))
      assert.equal(again[0].active, false, "a new started clears the quiet period")
      assert.deepEqual(timers.active(), [])
      assert.equal(kicks(p.spawns).length, 1)
    } finally {
      timers.restore()
    }
  })

  it("subscribes to exactly the four session.execution types", async (p) => {
    const bus = makeBus()
    await p.mod.default.setup(bus.ctx())
    assert.equal(bus.subscribed.length, 1)
    assert.deepEqual([...bus.subscribed[0]].sort(), [
      "session.execution.failed",
      "session.execution.interrupted",
      "session.execution.started",
      "session.execution.succeeded",
    ])
  })

  it(
    "WITNESS_WORKER=1 makes setup a no-op",
    async (p) => {
      const bus = makeBus()
      assert.equal(await p.mod.default.setup(bus.ctx()), undefined)
      await bus.publish(turn("evt_1", "succeeded", "ses_a"))
      assert.deepEqual(p.spawns, [])
      assert.equal(bus.subscribed.length, 0)
      assert.equal(bus.transforms, 0)
    },
    { env: { WITNESS_WORKER: "1" } },
  )

  it("cleanup ends the subscription, spawns nothing and never throws", async (p) => {
    const bus = makeBus()
    const cleanup = await p.mod.default.setup(bus.ctx())
    await settle()
    assert.equal(bus.live(), 1)
    const before = p.spawns.length
    assert.doesNotThrow(() => cleanup())
    assert.doesNotThrow(() => cleanup())
    await settle()
    assert.equal(bus.live(), 0)
    await bus.publish(turn("evt_1", "succeeded", "ses_a"))
    assert.equal(p.spawns.length, before, "no worker stop, no import after cleanup")
  })

  // An inherited value is stale by definition: the OpenCode running the plugin is process.execPath.
  it(
    "every spawn passes WITNESS_OPENCODE_BIN = process.execPath",
    async (p) => {
      const timers = fakeTimers()
      try {
        const bus = makeBus()
        await p.mod.default.setup(bus.ctx())
        await bus.publish(turn("evt_1", "succeeded", "ses_a"))
        timers.fire(timers.active()[0])
        assert.ok(p.spawns.length >= 3, `startup import, turn-end import and kick, got ${p.spawns.length}`)
        for (const s of p.spawns) assert.equal(s.env?.WITNESS_OPENCODE_BIN, process.execPath, s.args.join(" "))
      } finally {
        timers.restore()
      }
    },
    { env: { WITNESS_OPENCODE_BIN: "/stale/opencode" } },
  )

  let throwing = true
  it(
    "a throwing Bun.spawn never breaks setup or the event loop",
    async (p) => {
      const bus = makeBus()
      assert.equal(typeof (await p.mod.default.setup(bus.ctx())), "function")
      await bus.publish(turn("evt_1", "succeeded", "ses_a"))
      throwing = false
      await bus.publish(turn("evt_2", "succeeded", "ses_b"))
      assert.deepEqual(sessionImports(p.spawns), [["ses_b"]], "the event loop survives a throwing spawn")
    },
    {
      spawn(argv, options, spawns) {
        if (throwing) throw new Error("spawn ENOENT")
        spawns.push({ bin: argv[0], args: argv.slice(1), env: options?.env })
        return exitedProc()
      },
    },
  )
}

// A user who tried npm and later ran `witness wire opencode`: OpenCode loads both copies.
test("the npm and the wired plugin share one state", async () => {
  const timers = fakeTimers()
  const p = await loadPlugin({}, NPM.source)
  try {
    const wired = await p.load(EMBEDDED.source)
    const bus = makeBus()
    await p.mod.default.setup(bus.ctx())
    await wired.default.setup(bus.ctx())
    await settle()
    assert.equal(startupImports(p.spawns).length, 1)
    await bus.publish(turn("evt_9", "succeeded", "ses_b"))
    assert.deepEqual(sessionImports(p.spawns), [["ses_b"]])
    const armed = timers.active()
    assert.equal(armed.length, 1)
    timers.fire(armed[0])
    assert.equal(kicks(p.spawns).length, 1)
  } finally {
    timers.restore()
    await p.restore()
  }
})

// load(harness) gives a fresh process: it restores the previous load first, so a second
// harness in one test does not inherit the first one's state.
const npmTest = (name, fn) =>
  test(`npm: ${name}`, async () => {
    let p
    const load = async (harness) => {
      await p?.restore()
      return (p = await loadPlugin(harness, NPM.source))
    }
    try {
      await fn(load)
    } finally {
      await p?.restore()
    }
  })

npmTest("registers mcp.witness through ctx.mcp.transform and leaves an existing entry alone", async (load) => {
  const p = await load()
  const bus = makeBus()
  await p.mod.default.setup(bus.ctx())
  // No `enabled` key: v2's MCP config has none. WITNESS_NPM_PACKAGE=1 lets `witness mcp` start
  // before the model lands.
  assert.deepEqual(bus.mcp.get("witness"), {
    type: "local",
    command: [p.bin, "mcp"],
    environment: {
      WITNESS_ASSETS: "/assets/e5-small",
      WITNESS_PROMPTS: p.prompts,
      WITNESS_RUNNER: "opencode",
      WITNESS_NPM_PACKAGE: "1",
      WITNESS_OPENCODE_BIN: process.execPath,
    },
  })
  const mine = { type: "local", command: ["custom"] }
  const other = makeBus({ mcp: new Map([["witness", mine]]) })
  await p.mod.default.setup(other.ctx())
  assert.equal(other.transforms, 1)
  assert.equal(other.mcp.get("witness"), mine)
  assert.deepEqual(mine, { type: "local", command: ["custom"] })
})

const fetches = (spawns) => spawns.filter((s) => s.args.join(" ") === "model fetch")

npmTest("runs witness model fetch once per process, not under WITNESS_SKIP_MODEL_DOWNLOAD=1", async (load) => {
  const p = await load()
  const bus = makeBus()
  await p.mod.default.setup(bus.ctx())
  await p.mod.default.setup(bus.ctx())
  await (await p.reload()).default.setup(bus.ctx())
  assert.deepEqual(fetches(p.spawns).map((s) => s.bin), [p.bin])

  const off = await load({ env: { WITNESS_SKIP_MODEL_DOWNLOAD: "1" } })
  await off.mod.default.setup(makeBus().ctx())
  assert.deepEqual(fetches(off.spawns), [])
  assert.equal(startupImports(off.spawns).length, 1, "capture still runs")
})

npmTest("is inert without a platform binary", async (load) => {
  const p = await load({ platformBin: false })
  const bus = makeBus()
  assert.equal(await p.mod.default.setup(bus.ctx()), undefined)
  await bus.publish(turn("evt_1", "succeeded", "ses_a"))
  assert.deepEqual(p.spawns, [])
  assert.equal(bus.subscribed.length, 0)
  assert.equal(bus.transforms, 0)
})

// ||= semantics: the package's defaults fill only what the user left unset.
npmTest("keeps the user's WITNESS_ASSETS, WITNESS_PROMPTS and WITNESS_RUNNER", async (load) => {
  const p = await load()
  await p.mod.default.setup(makeBus().ctx())
  const env = startupImports(p.spawns)[0].env
  assert.deepEqual([env.WITNESS_ASSETS, env.WITNESS_PROMPTS, env.WITNESS_RUNNER], ["/assets/e5-small", p.prompts, "opencode"])

  const mine = await load({ env: { WITNESS_ASSETS: "/mine/model", WITNESS_PROMPTS: "/mine/prompts", WITNESS_RUNNER: "claude" } })
  await mine.mod.default.setup(makeBus().ctx())
  assert.ok(mine.spawns.length > 1, "the model fetch and the startup import")
  for (const s of mine.spawns) {
    assert.deepEqual([s.env.WITNESS_ASSETS, s.env.WITNESS_PROMPTS, s.env.WITNESS_RUNNER], ["/mine/model", "/mine/prompts", "claude"], s.args.join(" "))
  }
})
