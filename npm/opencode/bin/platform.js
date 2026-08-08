import { createRequire } from "node:module"
import path from "node:path"

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
