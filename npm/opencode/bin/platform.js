import { createRequire } from "node:module"
import path from "node:path"

const PACKAGES = {
  "darwin-arm64": "@witness-ai/opencode-darwin-arm64",
  "linux-x64": "@witness-ai/opencode-linux-x64",
}

export function platformPackage(platform = process.platform, arch = process.arch) {
  return PACKAGES[`${platform}-${arch}`] || ""
}

export function platformWitnessBin(platform = process.platform, arch = process.arch, resolve = createRequire(import.meta.url).resolve) {
  const name = platformPackage(platform, arch)
  if (!name) return ""
  try {
    return path.join(path.dirname(resolve(`${name}/package.json`)), "bin", "witness")
  } catch {
    return ""
  }
}

export function supportedPlatforms() {
  return "macOS Apple Silicon (darwin/arm64) and Linux x86-64 (linux/x64)"
}
