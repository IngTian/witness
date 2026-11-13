import { createRequire } from "node:module"
import os from "node:os"
import path from "node:path"
import { existsSync } from "node:fs"

const PACKAGES = {
  "darwin-arm64": "@witness-ai/opencode-darwin-arm64",
  "linux-x64": "@witness-ai/opencode-linux-x64",
  "win32-x64": "@witness-ai/opencode-win32-x64",
  "win32-arm64": "@witness-ai/opencode-win32-arm64",
}

export function platformPackage(platform = process.platform, arch = process.arch) {
  return PACKAGES[`${platform}-${arch}`] || ""
}

// binName is "witness.exe" on Windows and "witness" everywhere else.
//
// This is not cosmetic: it is the difference between the Windows packages working and being dead
// weight. The path built here is gated by an existsSync in both consumers (bin/witness.js and the
// plugin in witness.js), so a missing .exe suffix means the probe silently fails, WITNESS_BIN stays
// empty, and every hook early-returns — the plugin loads and captures nothing. Publishing a Windows
// binary while resolving it under a POSIX name would have shipped a package that could never run.
function binName(platform) {
  return platform === "win32" ? "witness.exe" : "witness"
}

export function platformWitnessBin(platform = process.platform, arch = process.arch, resolve = createRequire(import.meta.url).resolve) {
  const name = platformPackage(platform, arch)
  if (!name) return ""
  try {
    return path.join(path.dirname(resolve(`${name}/package.json`)), "bin", binName(platform))
  } catch {
    return ""
  }
}

export function supportedPlatforms() {
  return "macOS Apple Silicon (darwin/arm64), Linux x86-64 (linux/x64), and Windows x86-64 + ARM64 (win32/x64, win32/arm64)"
}

const DATA_DIR_NAMES = ["witness", "claude-witness"]

export function dataRoot() {
  if (process.env.WITNESS_HOME) return process.env.WITNESS_HOME
  const base = process.env.XDG_DATA_HOME || path.join(os.homedir(), ".local", "share")
  for (const name of DATA_DIR_NAMES) {
    const candidate = path.join(base, name)
    if (existsSync(candidate)) return candidate
  }
  return path.join(base, DATA_DIR_NAMES[0])
}

export function modelDir() {
  // Keep the large model in witness's user data dir, not inside the global npm
  // package. Package directories may be read-only, removed on upgrade, or shared
  // across package-manager installs; the data dir is stable and matches Go's
  // store root resolution, including the legacy claude-witness adoption rule.
  return process.env.WITNESS_ASSETS || path.join(dataRoot(), "assets", "e5-small")
}

// promptsDir returns the bundled prompt/lens templates shipped in THIS (main)
// package. Critical since the optionalDependencies split: the witness binary now
// lives in a SEPARATE per-platform package (@witness-ai/opencode-<plat>/bin), so
// the binary's exe-relative probe for prompts/ looks beside itself and misses the
// main package's prompts/ entirely — LoadDefault then fails and ALL distillation
// silently breaks. We pass this to the binary as WITNESS_PROMPTS (mirroring how
// WITNESS_ASSETS points at the model dir), which bundle.Dir honors first.
export function promptsDir(packageRoot) {
  return path.join(packageRoot, "prompts")
}
