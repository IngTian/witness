import assert from "node:assert/strict"
import path from "node:path"
import test from "node:test"

import { platformPackage, platformWitnessBin, supportedPlatforms } from "./platform.js"

test("maps only supported npm platforms", () => {
  assert.equal(platformPackage("darwin", "arm64"), "@witness-ai/opencode-darwin-arm64")
  assert.equal(platformPackage("linux", "x64"), "@witness-ai/opencode-linux-x64")
  assert.equal(platformPackage("darwin", "x64"), "")
  assert.equal(platformPackage("linux", "arm64"), "")
  assert.equal(platformPackage("win32", "x64"), "")
})

test("resolves the witness binary from the installed optional package", () => {
  const resolve = (specifier) => path.join("/packages", specifier)
  assert.equal(
    platformWitnessBin("linux", "x64", resolve),
    path.join("/packages", "@witness-ai/opencode-linux-x64", "bin", "witness"),
  )
})

test("returns no binary when the optional package is unavailable", () => {
  assert.equal(platformWitnessBin("darwin", "arm64", () => { throw new Error("missing") }), "")
  assert.match(supportedPlatforms(), /darwin\/arm64.*linux\/x64/)
})
