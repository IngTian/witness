#!/usr/bin/env node
import path from "node:path"
import { fileURLToPath } from "node:url"
import { unlinkSync } from "node:fs"
import { downloadModel, modelReady, startModelDownload } from "./model.js"

const args = process.argv.slice(2)
const defaultRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..")

if (args[0] === "--background") {
  startModelDownload(defaultRoot, { detached: true })
  process.exit(0)
}

const packageRoot = args[0] === "--foreground" ? args[1] || defaultRoot : args[0] || defaultRoot
const lock = args[0] === "--foreground" ? args[2] || "" : ""

function releaseLock() {
  if (!lock) return
  try {
    unlinkSync(lock)
  } catch {}
}

for (const signal of ["SIGINT", "SIGTERM"]) {
  process.once(signal, () => {
    releaseLock()
    process.exit(128 + (signal === "SIGINT" ? 2 : 15))
  })
}

const parentPID = Number(process.env.WITNESS_MODEL_PARENT_PID || "0")
if (parentPID > 0) {
  // OpenCode may be killed without running plugin dispose. Poll the parent so the
  // downloader still exits promptly and releases the lock instead of continuing a
  // 470MB transfer in the background.
  const parentWatch = setInterval(() => {
    try {
      process.kill(parentPID, 0)
    } catch {
      releaseLock()
      process.exit(0)
    }
  }, 5000)
  parentWatch.unref?.()
}

try {
  if (!modelReady(packageRoot)) await downloadModel(packageRoot)
} catch (err) {
  if (args[0] !== "--foreground") console.error(`witness: model download failed: ${err.message}`)
  process.exitCode = 1
} finally {
  releaseLock()
}
