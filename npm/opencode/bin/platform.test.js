import assert from "node:assert/strict"
import fs from "node:fs"
import path from "node:path"
import test from "node:test"
import { fileURLToPath } from "node:url"

import { platformPackage, platformWitnessBin, supportedPlatforms } from "./platform.js"

const HERE = path.dirname(fileURLToPath(import.meta.url))
const REPO = path.resolve(HERE, "..", "..", "..")

test("maps only supported npm platforms", () => {
  assert.equal(platformPackage("darwin", "arm64"), "@witness-ai/opencode-darwin-arm64")
  assert.equal(platformPackage("linux", "x64"), "@witness-ai/opencode-linux-x64")
  assert.equal(platformPackage("win32", "x64"), "@witness-ai/opencode-win32-x64")
  assert.equal(platformPackage("win32", "arm64"), "@witness-ai/opencode-win32-arm64")
  // Still unsupported — no binary is published for these.
  assert.equal(platformPackage("darwin", "x64"), "")
  assert.equal(platformPackage("linux", "arm64"), "")
  assert.equal(platformPackage("win32", "ia32"), "")
})

test("resolves the witness binary from the installed optional package", () => {
  const resolve = (specifier) => path.join("/packages", specifier)
  assert.equal(
    platformWitnessBin("linux", "x64", resolve),
    path.join("/packages", "@witness-ai/opencode-linux-x64", "bin", "witness"),
  )
})

// The .exe suffix is the difference between the Windows packages working and being dead weight.
//
// Both consumers gate the resolved path on existsSync (bin/witness.js, and the plugin in
// witness.js via `existsSync(platformBin) ? platformBin : ""`). A path without .exe does not
// exist on Windows, so WITNESS_BIN stays empty, every hook early-returns, and the plugin loads
// while capturing NOTHING — the same silent-inert failure class as v0.7.2's model download.
test("appends .exe on Windows and only on Windows", () => {
  const resolve = (specifier) => path.join("/packages", specifier)
  assert.equal(
    platformWitnessBin("win32", "x64", resolve),
    path.join("/packages", "@witness-ai/opencode-win32-x64", "bin", "witness.exe"),
  )
  assert.equal(
    platformWitnessBin("win32", "arm64", resolve),
    path.join("/packages", "@witness-ai/opencode-win32-arm64", "bin", "witness.exe"),
  )
  // POSIX targets must NOT gain a suffix.
  for (const [platform, arch] of [["darwin", "arm64"], ["linux", "x64"]]) {
    assert.equal(path.basename(platformWitnessBin(platform, arch, resolve)), "witness")
  }
})

test("returns no binary when the optional package is unavailable", () => {
  assert.equal(platformWitnessBin("darwin", "arm64", () => { throw new Error("missing") }), "")
  assert.equal(platformWitnessBin("win32", "x64", () => { throw new Error("missing") }), "")
})

test("advertises every supported platform", () => {
  const text = supportedPlatforms()
  for (const needle of ["darwin/arm64", "linux/x64", "win32/x64", "win32/arm64"]) {
    assert.ok(text.includes(needle), `supportedPlatforms() must mention ${needle}: ${text}`)
  }
})

// Every entry in PACKAGES must have a real package directory, and every platform package
// directory must appear in PACKAGES. A published binary that the resolver cannot name is
// unreachable; a name with no package is a guaranteed install failure. Neither half is
// detectable at runtime on a platform CI does not run, so pin it here.
test("PACKAGES and npm/platform/ agree, and each declares the right binary name", () => {
  const platformJs = fs.readFileSync(path.join(HERE, "platform.js"), "utf8")
  const mapped = [...platformJs.matchAll(/"(@witness-ai\/opencode-[a-z0-9-]+)"/g)].map((m) => m[1])
  assert.ok(mapped.length >= 4, `expected at least 4 mapped packages, got ${mapped.length}`)

  const dirs = fs.readdirSync(path.join(REPO, "npm", "platform"))
  const onDisk = dirs.map((d) => {
    const pkg = JSON.parse(fs.readFileSync(path.join(REPO, "npm", "platform", d, "package.json"), "utf8"))
    return { dir: d, ...pkg }
  })

  assert.deepEqual(
    [...mapped].sort(),
    onDisk.map((p) => p.name).sort(),
    "PACKAGES in platform.js must match the package dirs under npm/platform/ exactly",
  )

  for (const pkg of onDisk) {
    const wantsExe = pkg.os?.includes("win32")
    const bin = wantsExe ? "bin/witness.exe" : "bin/witness"
    assert.ok(
      pkg.files?.includes(bin),
      `${pkg.name} must ship ${bin} (files: ${JSON.stringify(pkg.files)}) — npm omits anything not listed`,
    )
  }
})

// The main package must declare every platform package as an optional dependency, at the
// same version. A missing entry means npm never installs that binary, so the platform is
// mapped and unreachable — install succeeds and witness is silently inert.
test("optionalDependencies cover every platform package at one version", () => {
  const main = JSON.parse(fs.readFileSync(path.join(REPO, "npm", "opencode", "package.json"), "utf8"))
  const dirs = fs.readdirSync(path.join(REPO, "npm", "platform"))
  for (const dir of dirs) {
    const pkg = JSON.parse(fs.readFileSync(path.join(REPO, "npm", "platform", dir, "package.json"), "utf8"))
    assert.equal(
      main.optionalDependencies?.[pkg.name],
      pkg.version,
      `${pkg.name} must be an optionalDependency of @witness-ai/opencode at ${pkg.version}`,
    )
  }
})
