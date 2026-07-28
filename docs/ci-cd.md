# Continuous integration and releases

Porto's automation lives in `.github/workflows`. Every workflow declares the least privilege it needs and checks out the repository without persisted Git credentials.

## Workflows

| Workflow | File | Trigger | Purpose |
| --- | --- | --- | --- |
| CI | `ci.yml` | pull requests, pushes to `main`, manual | Formatting, vet, tests, cross-compilation, dashboard lint and build |
| CodeQL | `codeql.yml` | pull requests, pushes to `main`, weekly | Static analysis for `go` and `javascript-typescript` |
| Security audit | `security.yml` | pushes to `main`, weekly, manual | `govulncheck` and `npm audit` |
| Release | `release.yml` | tags matching `v*`, manual | Builds, packages, and publishes release archives |

## CI details

The `go` job runs on `ubuntu-latest`, `macos-latest`, and `windows-latest` with the current stable toolchain, plus one extra `ubuntu-latest` job on `1.25.x` to guard the minimum version declared in `go.mod`. Linux runs `gofmt -l`, `go mod tidy -diff`, and `go test ./... -race`; the other platforms run the plain test suite.

The `cross-build` job compiles `./cmd/porto` with `CGO_ENABLED=0` for every release target (`linux`, `darwin`, and `windows` on `amd64` and `arm64`), so a broken platform build fails the pull request instead of the release.

The `ui` job installs with `npm ci` and runs `npm run lint` and `npm run build` on Node 22.12, 24, and 26. Node 22.12 is the lowest version Vite and oxlint accept.

Dependency updates arrive through `.github/dependabot.yml`, which groups Go modules, dashboard packages, and GitHub Actions into weekly pull requests.

## Cutting a release

1. Make sure `main` is green.
2. Tag the commit and push the tag:

   ```sh
   git tag -a v1.2.3 -m "Porto v1.2.3"
   git push origin v1.2.3
   ```

3. The `Release` workflow runs the Go test suite, lints and builds the dashboard, packages every target, and publishes a GitHub release with generated notes.

Tags must match `vX.Y.Z` or `vX.Y.Z-suffix`; a suffix such as `v1.2.3-rc.1` is published as a pre-release. Re-running a release manually from the Actions tab requires the tag to exist already, because publishing uses `gh release create --verify-tag`.

## Release artifacts

Each target produces one archive named `porto_<version>_<os>_<arch>` (`.tar.gz`, or `.zip` for Windows) with this layout:

```text
porto_1.2.3_darwin_arm64/
  porto            # porto.exe on Windows
  ui/dist/         # dashboard assets
  README.md
  LICENSE
```

The daemon resolves the dashboard from `$PORTO_UI_DIR`, `ui/dist` in the working directory, then `ui/dist` or `dist` next to the executable, so keep `ui/dist` beside the binary when installing. Binaries are built with `CGO_ENABLED=0 -trimpath -ldflags '-s -w'`.

A `SHA256SUMS` file covers every archive, and each archive gets a signed build provenance attestation. The archives themselves are not signed. Verify a download with:

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

Workflow files can be validated before pushing with:

```sh
go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/*.yml
```
