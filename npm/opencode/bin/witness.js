#!/usr/bin/env node
import { spawnSync } from "node:child_process"
import { existsSync } from "node:fs"
import { modelDir } from "./model.js"
import { platformPackage, platformWitnessBin, supportedPlatforms } from "./platform.js"

const command = process.argv[2] || ""

if (command === "install" || command === "uninstall") {
  console.error("witness: `install` and `uninstall` are source-checkout commands. With @witness-ai/opencode, add the plugin to opencode.json and let the plugin auto-register MCP.")
  process.exit(1)
}

const packageName = platformPackage()
const bin = platformWitnessBin()

if (!bin || !existsSync(bin)) {
  const reason = packageName ? `optional package ${packageName} is not installed` : `unsupported platform ${process.platform}/${process.arch}`
  console.error(`witness: ${reason}; supported platforms: ${supportedPlatforms()}`)
  process.exit(1)
}

// Default the model dir and the distillation runner for this OpenCode package, so
// a manual `witness doctor` / `distill start` from the npm install behaves the
// same as the plugin-kicked worker (OpenCode, not the template default claude).
// Both are non-clobbering fallbacks; an explicit `witness install` still wins
// (runner_bound), and an already-set env is respected.
const env = { ...process.env }
env.WITNESS_ASSETS ||= modelDir()
env.WITNESS_RUNNER ||= "opencode"
env.WITNESS_NPM_PACKAGE = "1"

const result = spawnSync(bin, process.argv.slice(2), {
  stdio: "inherit",
  env,
})

if (result.error) {
  console.error(`witness: failed to run bundled binary: ${result.error.message}`)
  process.exit(1)
}
process.exit(result.status ?? 0)
