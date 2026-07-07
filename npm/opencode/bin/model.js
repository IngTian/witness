import { spawn } from "node:child_process"
import { createWriteStream, existsSync, mkdirSync, openSync, statSync, unlinkSync, closeSync } from "node:fs"
import { rename } from "node:fs/promises"
import { createHash } from "node:crypto"
import { get as httpGet } from "node:http"
import { get as httpsGet } from "node:https"
import os from "node:os"
import path from "node:path"
import { fileURLToPath } from "node:url"

// Pin the exact HuggingFace revision (not a floating `main`): the SHA-256s below
// are that revision's git-LFS oids, verified against the pointers. A floating ref
// would silently drift the day upstream republishes the model — then every hash
// check fails. Keep this commit + hashes in lockstep with internal/model
// (fetch.go pinnedRevision) and scripts/fetch-model.sh.
const MODEL_REVISION = "614241f622f53c4eeff9890bdc4f31cfecc418b3"
const MODEL_MIN_BYTES = 400_000_000
const TOKENIZER_MIN_BYTES = 1_000_000
// Per-file integrity anchors (lowercase hex SHA-256). Verified on freshly
// downloaded bytes before the atomic rename — closes the MITM/corruption hole a
// size-only check leaves open (a tampered blob of the right length would pass).
const MODEL_SHA256 = "ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665"
const TOKENIZER_SHA256 = "0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39"
const DEFAULT_BASE_URL = "https://huggingface.co/intfloat/multilingual-e5-small/resolve/" + MODEL_REVISION
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

async function downloadFile(baseURL, remotePath, outName, minBytes, sha256, dir) {
  const dst = path.join(dir, outName)
  if (fileSize(dst) >= minBytes) return
  const tmp = `${dst}.part`
  try {
    unlinkSync(tmp)
  } catch {}
  const url = `${baseURL.replace(/\/+$/, "")}/${remotePath}`
  const res = await request(url)
  const hash = createHash("sha256")
  await new Promise((resolve, reject) => {
    const out = createWriteStream(tmp, { mode: 0o644 })
    res.on("data", (chunk) => hash.update(chunk))
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
  // Verify the content hash before publishing the file. Skipped only when a
  // custom mirror is in use (WITNESS_MODEL_BASE_URL) since a mirror legitimately
  // serves the same bytes but we cannot assume the caller pinned OUR revision —
  // the size guard still applies there.
  if (sha256) {
    const digest = hash.digest("hex")
    if (digest !== sha256) {
      try {
        unlinkSync(tmp)
      } catch {}
      throw new Error(`${outName} sha256 mismatch: got ${digest}, want ${sha256} (corrupted or tampered download)`)
    }
  }
  await rename(tmp, dst)
}

export async function downloadModel(packageRoot) {
  const dir = modelDir(packageRoot)
  mkdirSync(dir, { recursive: true })
  // A custom mirror may pin a different revision, so only enforce the pinned
  // SHA-256 against the default (revision-pinned) host.
  const mirror = process.env.WITNESS_MODEL_BASE_URL
  const baseURL = mirror || DEFAULT_BASE_URL
  const modelHash = mirror ? "" : MODEL_SHA256
  const tokHash = mirror ? "" : TOKENIZER_SHA256
  await downloadFile(baseURL, "onnx/model.onnx", "model.onnx", MODEL_MIN_BYTES, modelHash, dir)
  await downloadFile(baseURL, "tokenizer.json", "tokenizer.json", TOKENIZER_MIN_BYTES, tokHash, dir)
}
