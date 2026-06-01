#!/usr/bin/env node
// Launcher for `npx lema-mcp`: resolve the prebuilt Go binary for this platform
// (shipped as an optionalDependency — the esbuild/turbo pattern) and exec it with
// the same args and stdio. The server itself is pure Go; this shim is the npm
// front door so a user with only Node can run it — no Go toolchain, no account.
'use strict';

const { spawnSync } = require('child_process');
const path = require('path');

// process.arch uses Node's names (x64, arm64); map "<platform>-<arch>" to the
// per-platform package that carries the matching binary.
const PKG = {
  'darwin-arm64': 'lema-mcp-darwin-arm64',
  'darwin-x64': 'lema-mcp-darwin-x64',
  'linux-x64': 'lema-mcp-linux-x64',
  'linux-arm64': 'lema-mcp-linux-arm64',
  'win32-x64': 'lema-mcp-win32-x64',
};

const key = `${process.platform}-${process.arch}`;
const pkg = PKG[key];
if (!pkg) {
  console.error(
    `lema-mcp: no prebuilt binary for ${key}.\n` +
      `Build from source instead: go install github.com/lemahq/lema-mcp/cmd/lema-mcp@latest`
  );
  process.exit(1);
}

const binName = process.platform === 'win32' ? 'lema-mcp.exe' : 'lema-mcp';

let binPath;
try {
  // Resolve via the platform package's manifest, then locate its binary — robust
  // regardless of install layout (hoisted or nested node_modules).
  binPath = path.join(path.dirname(require.resolve(`${pkg}/package.json`)), 'bin', binName);
} catch {
  console.error(
    `lema-mcp: the platform package "${pkg}" is not installed.\n` +
      `Reinstall lema-mcp, or build from source: go install github.com/lemahq/lema-mcp/cmd/lema-mcp@latest`
  );
  process.exit(1);
}

const result = spawnSync(binPath, process.argv.slice(2), { stdio: 'inherit' });
if (result.error) {
  console.error(`lema-mcp: failed to start ${binPath}: ${result.error.message}`);
  process.exit(1);
}
process.exit(result.status === null ? 1 : result.status);
