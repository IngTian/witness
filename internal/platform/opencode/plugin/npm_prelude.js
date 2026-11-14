// GENERATED: npm/opencode/witness.js is this prelude + internal/platform/opencode/plugin/witness.js.
// Edit those two files, then: WITNESS_WRITE_NPM_PLUGIN=1 go test ./internal/platform/opencode/plugin/
import { existsSync } from "node:fs"
import { fileURLToPath } from "node:url"
import { modelDir, platformWitnessBin, promptsDir } from "./bin/platform.js"

const npmBin = platformWitnessBin()
const WITNESS_NPM = {
  bin: process.env.WITNESS_BIN || (existsSync(npmBin) ? npmBin : ""),
  env: {
    WITNESS_ASSETS: modelDir(),
    WITNESS_PROMPTS: promptsDir(fileURLToPath(new URL(".", import.meta.url))),
    WITNESS_RUNNER: "opencode",
  },
}

