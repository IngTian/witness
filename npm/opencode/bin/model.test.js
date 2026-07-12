import assert from "node:assert/strict"
import { createHash } from "node:crypto"
import { existsSync, mkdtempSync, mkdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs"
import http from "node:http"
import os from "node:os"
import path from "node:path"
import { afterEach, test } from "node:test"
import { execFile } from "node:child_process"
import { promisify } from "node:util"
import { fileURLToPath } from "node:url"
import { __test, downloadModel, modelReady } from "./model.js"

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

  writeFileSync(path.join(assets, "model.onnx"), "other-bytes")
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
    assert.equal(existsSync(path.join(assets, "model.onnx.part")), false)
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
