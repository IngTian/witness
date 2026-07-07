import { spawn } from "node:child_process"
import { createWriteStream, existsSync, mkdirSync, openSync, statSync, unlinkSync, closeSync } from "node:fs"
import { rename } from "node:fs/promises"
import { get as httpGet } from "node:http"
import { get as httpsGet } from "node:https"
import os from "node:os"
import path from "node:path"
import { fileURLToPath } from "node:url"

const MODEL_MIN_BYTES = 400_000_000
const TOKENIZER_MIN_BYTES = 1_000_000
const DEFAULT_BASE_URL = "https://huggingface.co/intfloat/multilingual-e5-small/resolve/main"
const LOCK_STALE_MS = 12 * 60 * 60 * 1000
const DATA_DIR_NAMES = ["witness", "claude-witness"]

const scriptPath = fileURLToPath(import.meta.url).replace(/model\.js$/, "download-model.js")

function dataRoot() {
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

function fileSize(file) {
  try {
    return statSync(file).size
  } catch {
    return 0
  }
}

function fileMtime(file) {
  try {
    return statSync(file).mtimeMs
  } catch {
    return Date.now()
  }
}

export function modelReady(packageRoot) {
  const dir = modelDir(packageRoot)
  return fileSize(path.join(dir, "model.onnx")) >= MODEL_MIN_BYTES && fileSize(path.join(dir, "tokenizer.json")) >= TOKENIZER_MIN_BYTES
}

function acquireLock(dir) {
  mkdirSync(dir, { recursive: true })
  const lock = path.join(dir, ".download.lock")
  try {
    const fd = openSync(lock, "wx")
    closeSync(fd)
    return lock
  } catch {
    if (Date.now() - fileMtime(lock) > LOCK_STALE_MS) {
      try {
        unlinkSync(lock)
      } catch {}
      return acquireLock(dir)
    }
    return ""
  }
}

export function startModelDownload(packageRoot, options = {}) {
  if (process.env.WITNESS_SKIP_MODEL_DOWNLOAD === "1" || modelReady(packageRoot)) return null
  const dir = modelDir(packageRoot)
  const lock = acquireLock(dir)
  if (!lock) return null
  const env = { ...process.env }
  // Plugin-owned downloads must die with OpenCode. The child also watches this
  // PID so a hard-killed OpenCode process does not leave a long model download
  // running on battery; detached=true is reserved for explicit CLI use.
  if (options.detached !== true) env.WITNESS_MODEL_PARENT_PID = String(process.pid)
  let child
  try {
    child = spawn(process.execPath, [scriptPath, "--foreground", packageRoot, lock], {
      detached: options.detached === true,
      stdio: "ignore",
      env,
    })
  } catch {
    try {
      unlinkSync(lock)
    } catch {}
    return null
  }
  if (options.detached === true) child.unref()
  if (typeof options.onExit === "function") {
    child.once("exit", (code) => options.onExit(code))
  }
  return {
    child,
    stop() {
      if (!child.killed && child.exitCode === null) child.kill()
    },
  }
}

function request(url, redirectLimit = 5) {
  return new Promise((resolve, reject) => {
    const get = url.startsWith("https:") ? httpsGet : httpGet
    const req = get(url, (res) => {
      if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location && redirectLimit > 0) {
        res.resume()
        resolve(request(new URL(res.headers.location, url).toString(), redirectLimit - 1))
        return
      }
      if (res.statusCode !== 200) {
        res.resume()
        reject(new Error(`HTTP ${res.statusCode} for ${url}`))
        return
      }
      resolve(res)
    })
    req.on("error", reject)
  })
}

async function downloadFile(baseURL, remotePath, outName, minBytes, dir) {
  const dst = path.join(dir, outName)
  if (fileSize(dst) >= minBytes) return
  const tmp = `${dst}.part`
  try {
    unlinkSync(tmp)
  } catch {}
  const url = `${baseURL.replace(/\/+$/, "")}/${remotePath}`
  const res = await request(url)
  await new Promise((resolve, reject) => {
    const out = createWriteStream(tmp, { mode: 0o644 })
    res.pipe(out)
    res.on("error", reject)
    out.on("error", reject)
    out.on("finish", resolve)
  })
  const got = fileSize(tmp)
  if (got < minBytes) {
    try {
      unlinkSync(tmp)
    } catch {}
    throw new Error(`${outName} is only ${got} bytes; download incomplete or not the real file`)
  }
  await rename(tmp, dst)
}

export async function downloadModel(packageRoot) {
  const dir = modelDir(packageRoot)
  mkdirSync(dir, { recursive: true })
  const baseURL = process.env.WITNESS_MODEL_BASE_URL || DEFAULT_BASE_URL
  await downloadFile(baseURL, "onnx/model.onnx", "model.onnx", MODEL_MIN_BYTES, dir)
  await downloadFile(baseURL, "tokenizer.json", "tokenizer.json", TOKENIZER_MIN_BYTES, dir)
}
