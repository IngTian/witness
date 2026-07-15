#!/usr/bin/env node
import { spawnSync } from "node:child_process"
import { existsSync } from "node:fs"
import path from "node:path"
import { fileURLToPath } from "node:url"
import { modelDir, promptsDir } from "./model.js"
import { platformPackage, platformWitnessBin, supportedPlatforms } from "./platform.js"

// This wrapper lives in <package>/bin/, so the package root (holding prompts/) is
// one level up.
const PACKAGE_ROOT = path.dirname(path.dirname(fileURLToPath(import.meta.url)))

const command = process.argv[2] || ""

if (command === "install" || command === "uninstall") {
  console.error("witness: `install` and `uninstall` are source-checkout commands. With @witness-ai/opencode, add the plugin to opencode.json and let the plugin auto-register MCP.")
  process.exit(1)
}

const packageName = platformPackage()
// Honor an explicit WITNESS_BIN override, just like the plugin (witness.js) does —
// used verbatim (no existsSync gate) so it can point at a dev build or a binary the
// platform-package probe can't resolve. Without this the CLI ignored the very
// override the plugin honored, so `WITNESS_BIN=... witness doctor` was inconsistent
// with what the plugin runs (issue #54 minor).
const override = process.env.WITNESS_BIN
const bin = override || platformWitnessBin()

if (!bin || (!override && !existsSync(bin))) {
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
// The binary is in a separate per-platform package, so it can't self-locate the
// prompts bundled in THIS package — point it at them (mirrors WITNESS_ASSETS).
env.WITNESS_PROMPTS ||= promptsDir(PACKAGE_ROOT)
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
