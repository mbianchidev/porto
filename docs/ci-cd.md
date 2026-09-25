# Continuous integration and releases

Porto's automation lives in `.github/workflows`. Every workflow declares the least privilege it needs and checks out the repository without persisted Git credentials.

## Workflows

| Workflow | File | Trigger | Purpose |
| --- | --- | --- | --- |
| CI | `ci.yml` | pull requests, pushes to `main`, manual | Formatting, vet, tests, cross-compilation, dashboard lint and build |
| CodeQL | `codeql.yml` | pull requests, pushes to `main`, weekly | Static analysis for `go` and `javascript-typescript` |
| Security audit | `security.yml` | pushes to `main`, weekly, manual | `govulncheck` and `npm audit` |
| Release | `release.yml` | tags matching `v*`, manual | Builds, packages, and publishes archives plus native macOS and Windows installers |

## CI details

The `go` job runs on `ubuntu-latest`, `macos-latest`, and `windows-latest` with the current stable toolchain, plus one extra `ubuntu-latest` job on `1.26.3` to guard the exact minimum version declared in `go.mod`. Linux runs `gofmt -l`, `go mod tidy -diff`, and `go test ./... -race`; the other platforms run the plain test suite.

The `cross-build` job compiles `./cmd/porto` with `CGO_ENABLED=0` for every release target (`linux`, `darwin`, and `windows` on `amd64` and `arm64`), so a broken platform build fails the pull request instead of the release.

The `ui` job installs with `npm ci` and runs `npm run lint`, `npm run build`,
and the desktop shell tests on Node 22.12, 24, and 26. Node 22.12 is the lowest
version Vite and oxlint accept. The Node 24 job audits the Electron lockfile
once, avoiding redundant registry requests and npm-version-specific tree checks.
Confirmed npm registry transport or server outages emit a warning instead of
failing the matrix; vulnerability reports and invalid lockfiles still fail.

Native runtime packaging executes `scripts/lima-runtime-smoke.cjs` against the
actual bundled Lima executable. It uses an isolated `LIMA_HOME` with synthetic
VM files to check dead-PID recovery, live-PID preservation, malformed-PID
diagnostics, and disk/configuration preservation without booting a VM.
Windows CI repeats these checks against the installed package in a path with
spaces, rather than only checking that its binaries exist.

Desktop logging tests run natively on Windows and macOS. The Windows installer
check also starts the installed daemon against a deliberately invalid,
isolated test database to verify that early failures reach `logs/porto.log`
at the default debug level, without duplicate records when stderr already
points at that file. No user database or running daemon is used.

Dependency updates arrive through `.github/dependabot.yml`, which groups Go modules, dashboard packages, and GitHub Actions into weekly pull requests.

## Cutting a release

1. Make sure `main` is green, checked out, clean, and tracking `origin/main`.
2. Optionally run the complete release validation without changing tracked files or Git history:

   ```sh
   ./release.sh 0.2.0 --dry-run
   ```

3. Prepare the release locally:

   ```sh
   ./release.sh 0.2.0
   ```

   The script accepts `0.2.0` or `v0.2.0`, validates strict SemVer, updates the Go CLI version plus the dashboard and Electron package manifests and lockfiles, runs the local checks below, creates a `chore(release): v0.2.0` commit when release metadata changed, and creates an annotated tag. Nothing is pushed by default.

4. Inspect the local commit and tag, then publish them explicitly:

   ```sh
   ./release.sh 0.2.0 --push
   ```

   Pass `--push` on the initial invocation to prepare and publish in one step. The script fetches first and atomically pushes `main` and the tag, so the release workflow cannot start from a tag without its release commit.

5. The `Release` workflow runs the Go test suite, lints and builds the dashboard, packages CLI/web and standalone desktop archives for every target, creates macOS DMGs and Windows NSIS EXE installers on native runners, and publishes a GitHub release with generated notes.

Tags must contain a strict SemVer prefixed with `v`. A suffix such as
`v1.2.3-rc.1` is published as a pre-release. Release assets are assembled and
verified while the GitHub release is still a draft, then published together.
Re-running a failed job can refresh that draft safely. A published release is
immutable: reruns fail instead of replacing assets that update clients may
already be downloading. A manual run from the Actions tab still requires the
tag to exist.

## Release artifacts

Each target produces a CLI/web archive named `porto_<version>_<os>_<arch>`
(`.tar.gz`, or `.zip` for Windows) with this layout:

```text
porto_1.2.3_darwin_arm64/
  porto            # porto.exe on Windows
  ui/dist/         # dashboard assets
  README.md
  LICENSE
```

The daemon resolves the dashboard from `$PORTO_UI_DIR`, `ui/dist` in the working directory, then `ui/dist` or `dist` next to the executable, so keep `ui/dist` beside the binary when installing. Release binaries are built with `CGO_ENABLED=0 -trimpath`; linker flags strip debug data and inject the release SemVer into `porto --version`.

Each target also produces `porto-desktop_<version>_<os>_<arch>`. Desktop
archives bundle the matching Porto binary, dashboard, icon, `kubectl`, Lima,
`k9s`, and supported `kind` clients. The app prepends those bundled tools to
the daemon's `PATH`, so they do not need separate installation.

For Windows Lima 2.2.0, the bundler checksum-verifies the stable source archive,
applies `scripts/patches/lima-2.2.0-windows-pid.patch`, and rebuilds only
`limactl.exe` as `v2.2.0+porto.1`. This is a narrow upstream backport, not an
upgrade to an unreleased Lima branch. The patch and Apache-2.0 license ship in
`runtime/licenses`, and `runtime/VERSIONS` records the patched version.
Other platforms keep their verified upstream binaries. Remove the backport
when moving to a stable Lima version containing the fix; keep the runtime
smoke checks as the release gate.

macOS releases additionally contain architecture-specific `.dmg` installers,
and Windows releases contain architecture-specific NSIS `.exe` installers.
The one-line install scripts select these native packages automatically;
portable archives remain available for manual and headless setups.
The desktop updater consumes these same exact asset names through the GitHub
Releases API; Linux updates use the portable desktop `.tar.gz` archive.
Every desktop package also embeds the complete release SemVer separately from
OS version metadata, so a release candidate can still detect the matching
stable release.

A `SHA256SUMS` file covers every archive and installer, and each asset gets a
signed build provenance attestation. The assets themselves are not
code-signed. Verify a download with:

```sh
sha256sum --check --ignore-missing SHA256SUMS  # shasum -a 256 --check on macOS
gh attestation verify porto_1.2.3_linux_amd64.tar.gz --repo mbianchidev/porto
```

## Running the checks locally

```sh
gofmt -l .
go mod tidy -diff
go vet ./...
go test ./...
go build ./cmd/porto
npm --prefix ui ci
npm --prefix ui run lint
npm --prefix ui run build
```

An opt-in multi-platform integration test uses an already running, Porto-owned
Lima engine on macOS or Linux. It applies engine emulation provisioning, then
runs uncached Dockerfile commands for eight Linux targets through a temporary
Porto Docker API socket and checks the exported architecture for each target.
It uses synthetic build inputs and does not restart or recreate the VM:

```sh
PORTO_DOCKER_MULTIPLATFORM_INTEGRATION=1 \
  go test ./internal/docker -run '^TestDockerMultiPlatformBuildIntegration$' -count=1 -v
```

Workflow files can be validated before pushing with:

```sh
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/*.yml
```
