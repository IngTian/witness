import assert from "node:assert/strict"
import { createHash } from "node:crypto"
import { existsSync, mkdtempSync, mkdirSync, readFileSync, rmSync, statSync, utimesSync, writeFileSync } from "node:fs"
import http from "node:http"
import os from "node:os"
import path from "node:path"
import { afterEach, test } from "node:test"
import { execFile } from "node:child_process"
import { promisify } from "node:util"
import { fileURLToPath } from "node:url"
import { __test, downloadModel, modelReady, startModelDownload } from "./model.js"

const execFileAsync = promisify(execFile)
const scriptPath = fileURLToPath(new URL("./download-model.js", import.meta.url))
const envKeys = [
  "WITNESS_ASSETS",
  "WITNESS_HOME",
  "WITNESS_MODEL_BASE_URL",
  "WITNESS_MODEL_SHA256",
  "WITNESS_TOKENIZER_SHA256",
  "WITNESS_MODEL_MIN_BYTES",
  "WITNESS_TOKENIZER_MIN_BYTES",
  "WITNESS_MODEL_INACTIVITY_TIMEOUT_MS",
  "XDG_DATA_HOME",
]
const originalEnv = new Map(envKeys.map((key) => [key, process.env[key]]))
const tempDirs = []

function tempDir() {
  const dir = mkdtempSync(path.join(os.tmpdir(), "witness-model-test-"))
  tempDirs.push(dir)
  return dir
}

function restoreEnv() {
  for (const key of envKeys) {
    const value = originalEnv.get(key)
    if (value === undefined) delete process.env[key]
    else process.env[key] = value
  }
}

function sha256(text) {
  return createHash("sha256").update(text).digest("hex")
}

afterEach(async () => {
  restoreEnv()
  for (const dir of tempDirs.splice(0)) rmSync(dir, { recursive: true, force: true })
})

test("downloadModel verifies existing files and writes a marker before ready", async () => {
  const root = tempDir()
  const assets = path.join(root, "assets")
  mkdirSync(assets, { recursive: true })
  writeFileSync(path.join(assets, "model.onnx"), "model-bytes")
  writeFileSync(path.join(assets, "tokenizer.json"), "tokenizer-bytes")
  process.env.WITNESS_ASSETS = assets
  process.env.WITNESS_MODEL_BASE_URL = "http://127.0.0.1:9/mirror"
  process.env.WITNESS_MODEL_SHA256 = sha256("model-bytes")
  process.env.WITNESS_TOKENIZER_SHA256 = sha256("tokenizer-bytes")
  process.env.WITNESS_MODEL_MIN_BYTES = "1"
  process.env.WITNESS_TOKENIZER_MIN_BYTES = "1"

  assert.equal(modelReady(root), false)
  await downloadModel(root)

  assert.equal(modelReady(root), true)
  const marker = JSON.parse(readFileSync(__test.verifiedMarkerPath(assets), "utf8"))
  assert.equal(marker.model.sha256, process.env.WITNESS_MODEL_SHA256)
  assert.equal(marker.tokenizer.sha256, process.env.WITNESS_TOKENIZER_SHA256)

  // Replacing the model with a DIFFERENT-SIZED file trips the size gate in the
  // marker, so it reads as not-ready. (A same-size swap is not caught here by
  // design — modelReady is a fast presence gate; the real sha256 integrity check
  // lives in downloadModel/validateFile. Content identity no longer depends on
  // mtime; see "verified marker omits mtime …" below.)
  writeFileSync(path.join(assets, "model.onnx"), "different-length-bytes")
  assert.equal(modelReady(root), false)
})

test("downloadModel tightens default data directories to 0700", async () => {
  const home = tempDir()
  const root = path.join(home, "witness")
  const assets = path.join(root, "assets", "e5-small")
  mkdirSync(assets, { recursive: true })
  writeFileSync(path.join(assets, "model.onnx"), "model-bytes")
  writeFileSync(path.join(assets, "tokenizer.json"), "tokenizer-bytes")
  process.env.XDG_DATA_HOME = home
  delete process.env.WITNESS_ASSETS
  process.env.WITNESS_MODEL_BASE_URL = "http://127.0.0.1:9/mirror"
  process.env.WITNESS_MODEL_SHA256 = sha256("model-bytes")
  process.env.WITNESS_TOKENIZER_SHA256 = sha256("tokenizer-bytes")
  process.env.WITNESS_MODEL_MIN_BYTES = "1"
  process.env.WITNESS_TOKENIZER_MIN_BYTES = "1"

  await downloadModel(root)

  assert.equal(statSync(path.join(home, "witness")).mode & 0o777, 0o700)
  assert.equal(statSync(path.join(home, "witness", "assets")).mode & 0o777, 0o700)
  assert.equal(statSync(assets).mode & 0o777, 0o700)
})

test("downloadModel rejects custom mirrors without explicit hashes", async () => {
  const root = tempDir()
  process.env.WITNESS_ASSETS = path.join(root, "assets")
  process.env.WITNESS_MODEL_BASE_URL = "http://127.0.0.1:9/mirror"
  process.env.WITNESS_MODEL_MIN_BYTES = "1"
  process.env.WITNESS_TOKENIZER_MIN_BYTES = "1"

  await assert.rejects(downloadModel(root), /WITNESS_MODEL_SHA256 and WITNESS_TOKENIZER_SHA256/)
})

test("downloadModel times out on inactivity and cleans up .part files", async () => {
  const root = tempDir()
  const assets = path.join(root, "assets")
  process.env.WITNESS_ASSETS = assets
  process.env.WITNESS_MODEL_MIN_BYTES = "4"
  process.env.WITNESS_TOKENIZER_MIN_BYTES = "1"
  process.env.WITNESS_MODEL_INACTIVITY_TIMEOUT_MS = "50"
  process.env.WITNESS_MODEL_SHA256 = "3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"
  process.env.WITNESS_TOKENIZER_SHA256 = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
  const server = http.createServer((req, res) => {
    if (req.url === "/onnx/model.onnx") {
      res.writeHead(200)
      res.write("abc")
      return
    }
    res.writeHead(200)
    res.end("hello")
  })
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve))
  process.env.WITNESS_MODEL_BASE_URL = `http://127.0.0.1:${server.address().port}`

  try {
    await assert.rejects(downloadModel(root), /download stalled|aborted|socket hang up/)
    // The temp file is pid-namespaced (model.onnx.<pid>.part) to prevent two
    // downloaders interleaving into one shared name; the failed download must
    // still leave no .part behind.
    assert.equal(existsSync(path.join(assets, `model.onnx.${process.pid}.part`)), false)
    assert.equal(existsSync(path.join(assets, "model.onnx")), false)
  } finally {
    await new Promise((resolve, reject) => server.close((err) => (err ? reject(err) : resolve())))
  }
})

test("foreground downloader does not release a lock it does not own", async () => {
  const root = tempDir()
  const assets = path.join(root, "assets")
  mkdirSync(assets, { recursive: true })
  writeFileSync(path.join(assets, "model.onnx"), "model-bytes")
  writeFileSync(path.join(assets, "tokenizer.json"), "tokenizer-bytes")
  process.env.WITNESS_ASSETS = assets
  process.env.WITNESS_MODEL_BASE_URL = "http://127.0.0.1:9/mirror"
  process.env.WITNESS_MODEL_SHA256 = sha256("model-bytes")
  process.env.WITNESS_TOKENIZER_SHA256 = sha256("tokenizer-bytes")
  process.env.WITNESS_MODEL_MIN_BYTES = "1"
  process.env.WITNESS_TOKENIZER_MIN_BYTES = "1"
  await downloadModel(root)

  const lock = path.join(assets, ".download.lock")
  writeFileSync(lock, "real-token\n")

  await execFileAsync(process.execPath, [scriptPath, "--foreground", root, lock, "wrong-token"], { env: process.env })
  assert.equal(existsSync(lock), true)
})

test("startModelDownload returns null instead of throwing when the data dir cannot be created", () => {
  const root = tempDir()
  // Point the data dir at a path whose parent is a FILE, so mkdirSync throws
  // ENOTDIR synchronously inside acquireLock → ensureModelDir. A throw here would
  // reject the OpenCode plugin factory promise and break the session; the
  // best-effort contract requires a null return instead.
  const notADir = path.join(root, "regular-file")
  writeFileSync(notADir, "not a directory")
  process.env.WITNESS_ASSETS = path.join(notADir, "assets", "e5-small")
  process.env.WITNESS_MODEL_BASE_URL = "http://127.0.0.1:9/mirror"
  process.env.WITNESS_MODEL_SHA256 = sha256("model-bytes")
  process.env.WITNESS_TOKENIZER_SHA256 = sha256("tokenizer-bytes")
  process.env.WITNESS_MODEL_MIN_BYTES = "1"
  process.env.WITNESS_TOKENIZER_MIN_BYTES = "1"

  assert.doesNotThrow(() => {
    const result = startModelDownload(root)
    assert.equal(result, null)
  })
})

test("acquireLock reclaims a lock whose same-host owner pid is dead, but not a live one", async () => {
  const root = tempDir()
  const assets = path.join(root, "assets")
  mkdirSync(assets, { recursive: true })
  process.env.WITNESS_ASSETS = assets
  const lock = path.join(assets, ".download.lock")

  // Lock held by a live pid on this host (our own) must NOT be reclaimable — this
  // is the normal "another downloader is running" case.
  writeFileSync(lock, `live-token ${process.pid} ${os.hostname()}\n`)
  assert.equal(__test.lockStale(lock), false)
  assert.equal(__test.acquireLock(assets), null)

  // Lock held by a dead pid on this host is reclaimed immediately (no 12h wait) —
  // this is the crashed-downloader wedge the liveness probe fixes. PID 2^31-1 is
  // effectively never a real process.
  const deadPID = 2147483647
  writeFileSync(lock, `dead-token ${deadPID} ${os.hostname()}\n`)
  assert.equal(__test.lockStale(lock), true)
  const acquired = __test.acquireLock(assets)
  assert.ok(acquired, "expected to reclaim a dead-owner lock")
  assert.equal(readFileSync(lock, "utf8").trim().split(/\s+/)[1], String(process.pid))
})

test("acquireLock does not reclaim a fresh lock from another host on pid liveness alone", async () => {
  const root = tempDir()
  const assets = path.join(root, "assets")
  mkdirSync(assets, { recursive: true })
  process.env.WITNESS_ASSETS = assets
  const lock = path.join(assets, ".download.lock")

  // A cross-host lock (e.g. a networked data dir): our local pid check is
  // meaningless, so it must fall back to the 12h mtime rule and stay held while
  // fresh. Using a pid that happens to be dead locally must not reclaim it.
  writeFileSync(lock, `remote-token 2147483647 some-other-host\n`)
  assert.equal(__test.lockStale(lock), false)
  assert.equal(__test.acquireLock(assets), null)
})

test("reapForeignParts removes other pids' .part files but keeps our own and the real files", async () => {
  const root = tempDir()
  const assets = path.join(root, "assets")
  mkdirSync(assets, { recursive: true })
  const mine = path.join(assets, `model.onnx.${process.pid}.part`)
  const foreign = path.join(assets, "model.onnx.999999.part")
  const real = path.join(assets, "model.onnx")
  writeFileSync(mine, "mine")
  writeFileSync(foreign, "orphan from a crashed downloader")
  writeFileSync(real, "real bytes")

  __test.reapForeignParts(assets)

  assert.equal(existsSync(foreign), false, "orphan foreign .part should be reaped")
  assert.equal(existsSync(mine), true, "our own in-progress .part must survive")
  assert.equal(existsSync(real), true, "the real model file must never be touched")
})

test("verified marker omits mtime so an external touch does not force a re-hash", async () => {
  const root = tempDir()
  const assets = path.join(root, "assets")
  mkdirSync(assets, { recursive: true })
  writeFileSync(path.join(assets, "model.onnx"), "model-bytes")
  writeFileSync(path.join(assets, "tokenizer.json"), "tokenizer-bytes")
  process.env.WITNESS_ASSETS = assets
  process.env.WITNESS_MODEL_BASE_URL = "http://127.0.0.1:9/mirror"
  process.env.WITNESS_MODEL_SHA256 = sha256("model-bytes")
  process.env.WITNESS_TOKENIZER_SHA256 = sha256("tokenizer-bytes")
  process.env.WITNESS_MODEL_MIN_BYTES = "1"
  process.env.WITNESS_TOKENIZER_MIN_BYTES = "1"

  await downloadModel(root)
  assert.equal(modelReady(root), true)

  const marker = JSON.parse(readFileSync(__test.verifiedMarkerPath(assets), "utf8"))
  assert.equal(marker.model.mtimeMs, undefined, "marker must not record mtime")
  assert.equal(marker.tokenizer.mtimeMs, undefined, "marker must not record mtime")

  // Touch the model's mtime far into the future; a byte-identical file must still
  // read as ready (no forced 470MB re-hash/re-download).
  const future = new Date(statSync(path.join(assets, "model.onnx")).mtimeMs + 60_000)
  utimesSync(path.join(assets, "model.onnx"), future, future)
  assert.equal(modelReady(root), true)
})
