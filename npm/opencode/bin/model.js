import { spawn } from "node:child_process"
import { createReadStream, createWriteStream, existsSync, mkdirSync, openSync, readdirSync, statSync, unlinkSync, closeSync, chmodSync, readFileSync, utimesSync, writeFileSync } from "node:fs"
import { rename } from "node:fs/promises"
import { createHash, randomUUID } from "node:crypto"
import { get as httpGet } from "node:http"
import { get as httpsGet } from "node:https"
import { Transform } from "node:stream"
import { pipeline } from "node:stream/promises"
import os from "node:os"
import path from "node:path"
import { fileURLToPath } from "node:url"

// Pin the exact HuggingFace revision (not a floating `main`): the SHA-256s below
// are that revision's git-LFS oids, verified against the pointers. A floating ref
// would silently drift the day upstream republishes the model — then every hash
// check fails. Keep this commit + hashes in lockstep with scripts/fetch-model.sh.
const MODEL_REVISION = "614241f622f53c4eeff9890bdc4f31cfecc418b3"
const MODEL_MIN_BYTES = 400_000_000
const TOKENIZER_MIN_BYTES = 1_000_000
// Per-file integrity anchors (lowercase hex SHA-256). Downloads and unmarked
// existing files are verified before a small file-identity marker is published.
const MODEL_SHA256 = "ca456c06b3a9505ddfd9131408916dd79290368331e7d76bb621f1cba6bc8665"
const TOKENIZER_SHA256 = "0b44a9d7b51c3c62626640cda0e2c2f70fdacdc25bbbd68038369d14ebdf4c39"
const DEFAULT_BASE_URL = "https://huggingface.co/intfloat/multilingual-e5-small/resolve/" + MODEL_REVISION
const LOCK_STALE_MS = 12 * 60 * 60 * 1000
const DATA_DIR_NAMES = ["witness", "claude-witness"]
const VERIFIED_MARKER = ".verified.json"
const DEFAULT_INACTIVITY_TIMEOUT_MS = 30_000

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

function envInt(name, fallback) {
  const raw = process.env[name]
  if (!raw) return fallback
  const value = Number(raw)
  return Number.isFinite(value) && value > 0 ? value : fallback
}

function modelMinBytes() {
  return envInt("WITNESS_MODEL_MIN_BYTES", MODEL_MIN_BYTES)
}

function tokenizerMinBytes() {
  return envInt("WITNESS_TOKENIZER_MIN_BYTES", TOKENIZER_MIN_BYTES)
}

function inactivityTimeoutMs() {
  return envInt("WITNESS_MODEL_INACTIVITY_TIMEOUT_MS", DEFAULT_INACTIVITY_TIMEOUT_MS)
}

function configuredMirrorHashes() {
  const baseURL = process.env.WITNESS_MODEL_BASE_URL || DEFAULT_BASE_URL
  if (!process.env.WITNESS_MODEL_BASE_URL) {
    return { baseURL, model: MODEL_SHA256, tokenizer: TOKENIZER_SHA256 }
  }
  const model = String(process.env.WITNESS_MODEL_SHA256 || "").trim().toLowerCase()
  const tokenizer = String(process.env.WITNESS_TOKENIZER_SHA256 || "").trim().toLowerCase()
  if (!model || !tokenizer) {
    throw new Error("custom WITNESS_MODEL_BASE_URL requires WITNESS_MODEL_SHA256 and WITNESS_TOKENIZER_SHA256")
  }
  return { baseURL, model, tokenizer }
}

function expectedHashes() {
  try {
    return configuredMirrorHashes()
  } catch {
    return null
  }
}

function ensureDirMode(dir) {
  try {
    chmodSync(dir, 0o700)
  } catch {}
}

function ensureModelDir(dir) {
  if (!process.env.WITNESS_ASSETS) {
    const root = dataRoot()
    const assets = path.join(root, "assets")
    mkdirSync(root, { recursive: true, mode: 0o700 })
    ensureDirMode(root)
    mkdirSync(assets, { recursive: true, mode: 0o700 })
    ensureDirMode(assets)
  }
  mkdirSync(dir, { recursive: true, mode: 0o700 })
  ensureDirMode(dir)
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

function sha256File(file) {
  return new Promise((resolve, reject) => {
    const hash = createHash("sha256")
    const input = createReadStream(file)
    input.on("data", (chunk) => hash.update(chunk))
    input.on("error", reject)
    input.on("end", () => resolve(hash.digest("hex")))
  })
}

function verifiedMarkerPath(dir) {
  return path.join(dir, VERIFIED_MARKER)
}

function verifiedFile(file, sha256) {
  // Identity = sha256 + size only. mtime was deliberately dropped: it changes on
  // any external touch (backup restore, rsync, editor stat) of a byte-identical
  // model and would force a needless full 470MB re-hash/re-download. sha256 already
  // proves content; size is a cheap pre-check.
  return { sha256, size: statSync(file).size }
}

function verifiedMarkerContent(dir, hashes) {
  return `${JSON.stringify({
    model: verifiedFile(path.join(dir, "model.onnx"), hashes.model),
    tokenizer: verifiedFile(path.join(dir, "tokenizer.json"), hashes.tokenizer),
  })}\n`
}

function hasVerifiedMarker(dir, hashes) {
  try {
    return readFileSync(verifiedMarkerPath(dir), "utf8") === verifiedMarkerContent(dir, hashes)
  } catch {
    return false
  }
}

async function writeVerifiedMarker(dir, hashes) {
  const file = verifiedMarkerPath(dir)
  const tmp = `${file}.${process.pid}.part`
  writeFileSync(tmp, verifiedMarkerContent(dir, hashes), { mode: 0o600 })
  await rename(tmp, file)
}

async function validateFile(file, minBytes, sha256) {
  if (fileSize(file) < minBytes) return false
  return (await sha256File(file)) === sha256
}

async function ensureVerified(dir, hashes) {
  const modelFile = path.join(dir, "model.onnx")
  const modelOK = await validateFile(modelFile, modelMinBytes(), hashes.model)
  if (!modelOK) {
    try {
      unlinkSync(verifiedMarkerPath(dir))
    } catch {}
    try {
      unlinkSync(modelFile)
    } catch {}
    return false
  }
  const tokenizerFile = path.join(dir, "tokenizer.json")
  const tokenizerOK = await validateFile(tokenizerFile, tokenizerMinBytes(), hashes.tokenizer)
  if (!tokenizerOK) {
    try {
      unlinkSync(verifiedMarkerPath(dir))
    } catch {}
    try {
      unlinkSync(tokenizerFile)
    } catch {}
    return false
  }
  await writeVerifiedMarker(dir, hashes)
  return true
}

export function modelReady(packageRoot) {
  const dir = modelDir(packageRoot)
  const hashes = expectedHashes()
  if (!hashes) return false
  if (fileSize(path.join(dir, "model.onnx")) < modelMinBytes() || fileSize(path.join(dir, "tokenizer.json")) < tokenizerMinBytes()) {
    return false
  }
  if (hasVerifiedMarker(dir, hashes)) return true
  return false
}

// The lock file records "<token> <pid> <hostname>" so a stale lock can be reaped
// two ways: (1) same-host liveness — if the recorded pid is gone, the owner
// crashed and we reclaim immediately (fixes the up-to-12h wedge a hard-killed
// downloader used to cause); (2) a 12h wall-clock fallback for a cross-host lock
// (a networked data dir) or an unreadable body, where pid liveness is meaningless.
function lockOwnerDead(lock) {
  let body
  try {
    body = readFileSync(lock, "utf8")
  } catch {
    return false // can't read it; fall back to the mtime rule
  }
  const [, pidRaw, host] = body.trim().split(/\s+/)
  const pid = Number(pidRaw)
  if (!Number.isInteger(pid) || pid <= 0 || host !== os.hostname()) return false
  try {
    process.kill(pid, 0) // signal 0: liveness probe, sends nothing
    return false // owner still alive
  } catch (err) {
    return err.code === "ESRCH" // no such process — owner is gone
  }
}

function lockStale(lock) {
  return lockOwnerDead(lock) || Date.now() - fileMtime(lock) > LOCK_STALE_MS
}

function acquireLock(dir) {
  ensureModelDir(dir)
  const lock = path.join(dir, ".download.lock")
  const token = randomUUID()
  try {
    const fd = openSync(lock, "wx")
    writeFileSync(fd, `${token} ${process.pid} ${os.hostname()}\n`, { encoding: "utf8" })
    closeSync(fd)
    return { lock, token }
  } catch {
    if (lockStale(lock)) {
      try {
        unlinkSync(lock)
      } catch {}
      return acquireLock(dir)
    }
    return null
  }
}

export function startModelDownload(packageRoot, options = {}) {
  if (process.env.WITNESS_SKIP_MODEL_DOWNLOAD === "1" || modelReady(packageRoot)) return null
  const dir = modelDir(packageRoot)
  // acquireLock → ensureModelDir → mkdirSync can throw synchronously (read-only
  // or unwritable data dir). This is a best-effort kickoff whose contract is
  // "return null if it can't start", and it runs on the OpenCode plugin's init/
  // idle path — an unhandled throw here would reject the plugin factory promise
  // and break the session, violating "capture must never break a session".
  let lock
  try {
    lock = acquireLock(dir)
  } catch {
    return null
  }
  if (!lock) return null
  const env = { ...process.env }
  // Plugin-owned downloads must die with OpenCode. The child also watches this
  // PID so a hard-killed OpenCode process does not leave a long model download
  // running on battery; detached=true is reserved for explicit CLI use.
  if (options.detached !== true) env.WITNESS_MODEL_PARENT_PID = String(process.pid)
  let child
  try {
    child = spawn(process.execPath, [scriptPath, "--foreground", packageRoot, lock.lock, lock.token], {
      detached: options.detached === true,
      stdio: "ignore",
      env,
    })
  } catch {
    try {
      unlinkSync(lock.lock)
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
    const timeout = inactivityTimeoutMs()
    const stalled = () => new Error(`download stalled for ${timeout}ms`)
    const req = get(url, (res) => {
      clearTimeout(timer)
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
    const timer = setTimeout(() => req.destroy(stalled()), timeout)
    req.on("error", (err) => {
      clearTimeout(timer)
      reject(err)
    })
  })
}

async function downloadFile(baseURL, remotePath, outName, minBytes, sha256, dir) {
  const dst = path.join(dir, outName)
  if (await validateFile(dst, minBytes, sha256)) return
  try {
    unlinkSync(dst)
  } catch {}
  // PID-namespace the temp file. A stale-lock reap can hand the lock to a second
  // downloader while a >12h (or crashed-but-not-yet-reaped) first one is mid-write;
  // a shared `${dst}.part` would then interleave two byte streams into one file and
  // pass the size check with corrupt contents. Per-pid parts can't collide, and the
  // sha256 gate still rejects a partial file before it is renamed into place.
  const tmp = `${dst}.${process.pid}.part`
  const url = `${baseURL.replace(/\/+$/, "")}/${remotePath}`
  try {
    const res = await request(url)
    const hash = createHash("sha256")
    const timeout = inactivityTimeoutMs()
    let timer
    const resetTimer = () => {
      clearTimeout(timer)
      timer = setTimeout(() => res.destroy(new Error(`download stalled for ${timeout}ms`)), timeout)
    }
    resetTimer()
    try {
      await pipeline(
        res,
        new Transform({
          transform(chunk, _encoding, callback) {
            resetTimer()
            hash.update(chunk)
            callback(null, chunk)
          },
        }),
        createWriteStream(tmp, { mode: 0o600 }),
      )
    } finally {
      clearTimeout(timer)
    }
    const got = fileSize(tmp)
    if (got < minBytes) {
      throw new Error(`${outName} is only ${got} bytes; download incomplete or not the real file`)
    }
    const digest = hash.digest("hex")
    if (digest !== sha256) {
      throw new Error(`${outName} sha256 mismatch: got ${digest}, want ${sha256} (corrupted or tampered download)`)
    }
    await rename(tmp, dst)
  } catch (err) {
    try {
      unlinkSync(tmp)
    } catch {}
    throw err
  }
}

// reapForeignParts removes leftover *.part temp files from OTHER pids. Safe
// because downloadModel only runs while holding the download lock, so we are the
// sole legitimate writer: any part not named for our pid is an orphan from a
// crashed prior owner (per-pid parts replaced the old self-cleaning shared name).
// If a >12h stale-lock reap ever overlapped a still-live cross-host writer,
// unlinking its open .part just drops the dirent — that writer's final rename
// fails cleanly (ENOENT) rather than producing a corrupt file.
function reapForeignParts(dir) {
  const mine = `.${process.pid}.part`
  let entries
  try {
    entries = readdirSync(dir)
  } catch {
    return
  }
  for (const name of entries) {
    if (!name.endsWith(".part") || name.endsWith(mine)) continue
    try {
      unlinkSync(path.join(dir, name))
    } catch {}
  }
}

export async function downloadModel(packageRoot) {
  const dir = modelDir(packageRoot)
  ensureModelDir(dir)
  const hashes = configuredMirrorHashes()
  if (modelReady(packageRoot)) return
  reapForeignParts(dir)
  if (await ensureVerified(dir, hashes)) return
  await downloadFile(hashes.baseURL, "onnx/model.onnx", "model.onnx", modelMinBytes(), hashes.model, dir)
  await downloadFile(hashes.baseURL, "tokenizer.json", "tokenizer.json", tokenizerMinBytes(), hashes.tokenizer, dir)
  await writeVerifiedMarker(dir, hashes)
}

export const __test = {
  verifiedMarkerPath,
  acquireLock,
  lockStale,
  reapForeignParts,
}
