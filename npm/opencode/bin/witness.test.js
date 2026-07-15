import assert from "node:assert/strict"
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises"
import os from "node:os"
import path from "node:path"
import test from "node:test"
import { pathToFileURL } from "node:url"

const sourcePath = new URL("./witness.js", import.meta.url)
const source = (await readFile(sourcePath, "utf8"))
  .replace('import { spawnSync } from "node:child_process"', 'const { spawnSync } = globalThis.__witnessCliHarness.childProcess')
  .replace('import { existsSync } from "node:fs"', 'const { existsSync } = globalThis.__witnessCliHarness.fs')
  .replace('import { modelDir, promptsDir } from "./model.js"', 'const { modelDir, promptsDir } = globalThis.__witnessCliHarness.model')
  .replace('import { platformPackage, platformWitnessBin, supportedPlatforms } from "./platform.js"', 'const { platformPackage, platformWitnessBin, supportedPlatforms } = globalThis.__witnessCliHarness.platform')

async function runCLI(argv, harness) {
  const dir = await mkdtemp(path.join(os.tmpdir(), "witness-opencode-cli-"))
  await mkdir(path.join(dir, "bin"), { recursive: true })
  const script = path.join(dir, "bin", "witness.mjs")
  await writeFile(script, source)

  const previous = {
    argv: process.argv,
    exit: process.exit,
    error: console.error,
    harness: globalThis.__witnessCliHarness,
    witnessBin: process.env.WITNESS_BIN,
  }
  const errors = []
  const sentinel = new Error("exit")
  sentinel.name = "WitnessCLIExit"

  globalThis.__witnessCliHarness = harness
  // Hermetic env: bin/witness.js now reads WITNESS_BIN (the CLI override, #54
  // minor). Clear any ambient value so a developer dogfooding with WITNESS_BIN set
  // doesn't flip tests that assume the platform-resolved binary; a test that wants
  // the override sets it explicitly (and this snapshot restores it after).
  if (Object.hasOwn(harness, "witnessBin")) process.env.WITNESS_BIN = harness.witnessBin
  else delete process.env.WITNESS_BIN
  process.argv = argv
  process.exit = (code) => {
    sentinel.code = code
    throw sentinel
  }
  console.error = (msg) => {
    errors.push(String(msg))
  }

  try {
    await import(`${pathToFileURL(script).href}?t=${Date.now()}-${Math.random()}`)
    return { code: 0, errors, spawnCalls: harness.spawnCalls || [] }
  } catch (err) {
    if (err === sentinel) return { code: sentinel.code, errors, spawnCalls: harness.spawnCalls || [] }
    throw err
  } finally {
    process.argv = previous.argv
    process.exit = previous.exit
    console.error = previous.error
    globalThis.__witnessCliHarness = previous.harness
    if (previous.witnessBin === undefined) delete process.env.WITNESS_BIN
    else process.env.WITNESS_BIN = previous.witnessBin
    await rm(dir, { recursive: true, force: true })
  }
}

test("npm CLI gives a clear install/uninstall error before looking for bundled binaries", async () => {
  const harness = {
    fs: {
      existsSync() {
        throw new Error("should not probe dist for install/uninstall")
      },
    },
    model: {
      modelDir() {
        return "/assets/e5-small"
      },
      promptsDir() {
        return "/pkg/prompts"
      },
    },
    platform: {
      platformPackage() {
        throw new Error("should not resolve platform for install/uninstall")
      },
      platformWitnessBin() {
        throw new Error("should not resolve binary for install/uninstall")
      },
      supportedPlatforms() {
        return "supported"
      },
    },
    childProcess: {
      spawnSync() {
        throw new Error("should not spawn install/uninstall")
      },
    },
  }
  const result = await runCLI([process.execPath, "witness", "install", "opencode"], harness)
  assert.equal(result.code, 1)
  assert.match(result.errors[0], /source-checkout commands/)
})

test("npm CLI still forwards non-install commands to the bundled binary with witness env defaults", async () => {
  const spawnCalls = []
  const harness = {
    spawnCalls,
    fs: {
      existsSync() {
        return true
      },
    },
    model: {
      modelDir() {
        return "/assets/e5-small"
      },
      promptsDir() {
        return "/pkg/prompts"
      },
    },
    platform: {
      platformPackage() {
        return "@witness-ai/opencode-darwin-arm64"
      },
      platformWitnessBin() {
        return "/packages/darwin-arm64/bin/witness"
      },
      supportedPlatforms() {
        return "supported"
      },
    },
    childProcess: {
      spawnSync(bin, args, options) {
        spawnCalls.push({ bin, args, options })
        return { status: 0 }
      },
    },
  }
  const result = await runCLI([process.execPath, "witness", "profile"], harness)
  assert.equal(result.code, 0)
  assert.equal(spawnCalls.length, 1)
  assert.equal(spawnCalls[0].bin, "/packages/darwin-arm64/bin/witness")
  assert.deepEqual(spawnCalls[0].args, ["profile"])
  assert.equal(spawnCalls[0].options.env.WITNESS_ASSETS, "/assets/e5-small")
  assert.equal(spawnCalls[0].options.env.WITNESS_PROMPTS, "/pkg/prompts")
  assert.equal(spawnCalls[0].options.env.WITNESS_RUNNER, "opencode")
  assert.equal(spawnCalls[0].options.env.WITNESS_NPM_PACKAGE, "1")
})

test("npm CLI honors an explicit WITNESS_BIN override even when no platform package resolves", async () => {
  const spawnCalls = []
  const harness = {
    spawnCalls,
    witnessBin: "/custom/dev/witness", // runCLI sets process.env.WITNESS_BIN to this
    fs: {
      existsSync() {
        throw new Error("an explicit WITNESS_BIN must be used verbatim, not existsSync-gated")
      },
    },
    model: { modelDir: () => "/assets/e5-small", promptsDir: () => "/pkg/prompts" },
    platform: {
      platformPackage: () => "", // unsupported platform: no bundled binary
      platformWitnessBin: () => "",
      supportedPlatforms: () => "supported",
    },
    childProcess: {
      spawnSync(bin, args, options) {
        spawnCalls.push({ bin, args, options })
        return { status: 0 }
      },
    },
  }
  const result = await runCLI([process.execPath, "witness", "doctor"], harness)
  assert.equal(result.code, 0)
  assert.equal(spawnCalls.length, 1)
  assert.equal(spawnCalls[0].bin, "/custom/dev/witness")
})

test("npm CLI reports the supported matrix when no platform package is available", async () => {
  const harness = {
    fs: { existsSync: () => false },
    model: { modelDir: () => "/assets/e5-small" },
    platform: {
      platformPackage: () => "",
      platformWitnessBin: () => "",
      supportedPlatforms: () => "macOS Apple Silicon and Linux x86-64",
    },
    childProcess: { spawnSync: () => { throw new Error("should not spawn") } },
  }
  const result = await runCLI([process.execPath, "witness", "doctor"], harness)
  assert.equal(result.code, 1)
  assert.match(result.errors[0], /unsupported platform/)
  assert.match(result.errors[0], /macOS Apple Silicon and Linux x86-64/)
})
