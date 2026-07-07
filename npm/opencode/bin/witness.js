#!/usr/bin/env node
import { spawnSync } from "node:child_process"
import { existsSync } from "node:fs"
import path from "node:path"
import { fileURLToPath } from "node:url"
import { modelDir } from "./model.js"

function binaryName() {
  const os = { darwin: "darwin", linux: "linux", win32: "windows" }[process.platform]
  const arch = { x64: "amd64", arm64: "arm64" }[process.arch]
  if (!os || !arch) return ""
  return `witness-${os}-${arch}${os === "windows" ? ".exe" : ""}`
}

const packageRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..")
const name = binaryName()
const bin = name ? path.join(packageRoot, "dist", name) : ""

if (!bin || !existsSync(bin)) {
  console.error(`witness: no bundled binary for ${process.platform}/${process.arch}`)
  process.exit(1)
}

const result = spawnSync(bin, process.argv.slice(2), {
  stdio: "inherit",
  env: { ...process.env, WITNESS_ASSETS: process.env.WITNESS_ASSETS || modelDir() },
})

if (result.error) {
  console.error(`witness: failed to run bundled binary: ${result.error.message}`)
  process.exit(1)
}
process.exit(result.status ?? 0)
