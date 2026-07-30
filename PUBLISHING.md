# Publishing `lema-mcp` to npm

`npx lema-mcp` resolves a prebuilt Go binary shipped as a per-platform
`optionalDependency` (the esbuild / turbo / Biome pattern). npm installs only the
package matching the user's OS/arch; the `lema-mcp` launcher execs it. Publishing
public npm packages is **free** — no per-download or per-run cost.

## One-time

- `npm login` with the account that will own `lema-mcp`.
- **2FA + scripted publish:** if the account has 2FA (it should), `npm publish` from a
  script fails with `E403 … Two-factor authentication or granular access token with
  bypass 2fa … required`, because it can't prompt for an OTP mid-loop. Create an
  **automation token** (npmjs.com → Access Tokens → Generate New → Classic → Automation;
  or a Granular token with Read+Write on all packages + bypass-2FA) and point npm at it:
  `npm config set //registry.npmjs.org/:_authToken=<TOKEN>`. Automation tokens bypass 2FA.
- Confirm the name is free: `npm view lema-mcp` → a 404 means it's available.

## Each release

**TL;DR:** bump the version (step 1), then `npm login && bash scripts/publish.sh` —
it runs steps 2–4 in the required order (build → platform packages → launcher).
Verify with `npx lema-mcp@latest demo`. **Then, and only then, push the `v*` tag**
(step 6).

> ### Order matters twice
>
> **Within npm:** platform packages before the launcher — the launcher's
> `optionalDependencies` must already resolve. `scripts/publish.sh` handles this.
>
> **Across npm and the tag: publish to npm BEFORE tagging.** The `v*` tag triggers
> `mcp-registry-publish`, and the MCP registry validates that npm already serves
> that version — it answers with an opaque `400 … version 'X' was not found`
> otherwise. While `NPM_PUBLISH_ENABLED` is `false`, CI cannot populate npm for
> you, so a tag pushed first *cannot* satisfy the registry.
>
> This is not hypothetical: **`v0.22.0` and `v0.22.1` both failed this way.** The
> GitHub Release succeeded both times, `publish to npm` was skipped, and
> `publish to MCP registry` failed on validation. The workflow now checks this up
> front and tells you the order instead of surfacing the registry's 400.
>
> Recovering a tag you already pushed: publish to npm, then re-run the failed
> `publish to MCP registry` job. Nothing needs reverting and the tag stays valid.

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
   npx lema-mcp@latest demo      # the 30-second never-reopen walkthrough
   npx lema-mcp@latest init      # writes .mcp.json + AGENTS.md + the hook
   ```
   then point a coding agent at the repo and ask "why did we choose X?".
6. **Tag LAST** — cuts the GitHub Release and publishes to the MCP registry, which
   validates against the npm version you just published (see "Order matters twice"):
   ```bash
   git tag vX.Y.Z && git push origin vX.Y.Z
   ```
7. **Bump consumers.** The monorepo pins an exact version in `.claude/settings.json`
   (`npx -y lema-mcp@X.Y.Z` across the guard, nudge and Stop hooks) — a fix does not
   reach any session until that pin moves. Do this only once npm serves the version:
   the pin resolves at hook-run time, so pointing it at an unpublished version breaks
   every hook it names.

## MCP registry (discovery)

Listing `lema-mcp` in the public [MCP registry](https://registry.modelcontextprotocol.io)
makes it discoverable by coding agents and federates to PulseMCP / Glama / Smithery.
It is **discovery only** — the server still runs from npm / your own host. `server.json`
(repo root) is the manifest; the `mcp-registry-publish` job in `release.yml` publishes it
on a `v*` tag.

It is **off until you opt in** (mirrors the npm-publish gate):

1. **Namespace.** We use `io.github.lemahq/lema-mcp`, which GitHub OIDC from this repo
   authenticates automatically — no secret to add.
2. **Regenerate against the current schema.** Schemas drift; run once locally:
   `mcp-publisher init` (overwrites `server.json` with the current `$schema`), re-apply
   our name/description/package fields, then `mcp-publisher validate ./server.json`.
3. **Verify the CI install/login lines** in `release.yml` against the current
   [registry release](https://github.com/modelcontextprotocol/registry/releases).
4. **Flip it on:** set repo variable `MCP_REGISTRY_PUBLISH_ENABLED=true`. The next
   `v*` tag pins `server.json`'s version to the tag, validates, logs in via OIDC, and
   publishes. Verify discovery:
   `curl -s "https://registry.modelcontextprotocol.io/v0/servers?search=lema" | jq '.servers[].name'`.

## Notes

- Pure-Go, CGO-free → cross-compiled from any host; no platform build machines.
- **Windows:** the launcher already maps `win32-x64`; add a `npm/win32-x64`
  package + a `build` line when there's demand.
- The binaries are gitignored (built at publish time), so the repo stays small.
- **README on npm:** the launcher's `package.json` has a `prepack` that copies the
  root `README.md` + `LICENSE` into `npm/lema-mcp/` so the npm page renders them
  (npm only reads a README that sits in the published package dir). Both copies are
  gitignored — edit the **root** `README.md`, never `npm/lema-mcp/README.md`.
- The Go source under `internal/` is mechanically synced from the monorepo by
  `scripts/extract-lema-mcp.sh`; do not hand-edit it here. `cmd/lema-mcp` is
  OWNED by this repo since the pivot B2 entry gate (D1 7978b83e) retired the
  monorepo copy — edit it here directly. The npm packaging and README are also
  dist-native and maintained here.
