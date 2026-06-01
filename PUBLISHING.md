# Publishing `lema-mcp` to npm

`npx lema-mcp` resolves a prebuilt Go binary shipped as a per-platform
`optionalDependency` (the esbuild / turbo / Biome pattern). npm installs only the
package matching the user's OS/arch; the `lema-mcp` launcher execs it. Publishing
public npm packages is **free** — no per-download or per-run cost.

## One-time

- `npm login` with the account that will own `lema-mcp`.
- Confirm the name is free: `npm view lema-mcp` → a 404 means it's available.

## Each release

1. **Bump the version** to match in all of: `npm/lema-mcp/package.json`, every
   `npm/<platform>/package.json`, and the `optionalDependencies` pins in
   `npm/lema-mcp/package.json` (they must all be identical, e.g. `0.4.0`). Keep it
   in sync with the Go server version in `cmd/lema-mcp/main.go`.
2. **Build the binaries:** `bash scripts/build-npm.sh`
3. **Publish the platform packages FIRST** (the launcher's optionalDependencies
   must already exist on the registry):
   ```bash
   for d in darwin-arm64 darwin-x64 linux-x64 linux-arm64; do
     (cd "npm/$d" && npm publish --access public)
   done
   ```
4. **Publish the launcher LAST:**
   ```bash
   (cd npm/lema-mcp && npm publish --access public)
   ```
5. **Verify on a clean machine:**
   ```bash
   npx lema-mcp@latest init      # writes .mcp.json + AGENTS.md + the hook
   ```
   then point a coding agent at the repo and ask "why did we choose X?".

## Notes

- Pure-Go, CGO-free → cross-compiled from any host; no platform build machines.
- **Windows:** the launcher already maps `win32-x64`; add a `npm/win32-x64`
  package + a `build` line when there's demand.
- The binaries are gitignored (built at publish time), so the repo stays small.
- The Go source under `cmd/` + `internal/` is mechanically synced from the
  monorepo by `scripts/extract-lema-mcp.sh`; do not hand-edit it here. The npm
  packaging and README are dist-native and ARE maintained here.
