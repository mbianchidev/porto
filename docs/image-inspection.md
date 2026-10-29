# Inspect and analyze local images

Porto desktop packages include [Dive](https://github.com/wagoodman/dive), a
terminal interface for exploring an image's read-only layer stack and the files
added, modified, or removed by each build step.

In Porto desktop, select an image and open the **Layers** tab, or use the
**Inspect image layers** action beside an image or container. Dive runs inside
Porto's embedded terminal and targets the selected image through Porto's Docker
endpoint.

The embedded terminal currently requires a PTY-capable macOS or Linux host. On
Windows, use `porto docker dive IMAGE` in a terminal.

Run Dive through Porto so it always targets Porto's Docker endpoint:

```sh
porto docker dive alpine:latest
porto docker dive registry.example.com/team/app@sha256:...
porto docker dive --container running-api
```

The install scripts also create a `dive` command when that name is unused.
Direct `dive IMAGE` follows normal Docker environment variables, so prefer
`porto docker dive` unless the Porto Docker endpoint is currently activated as
your default.

## Why inspect layers

Layer inspection helps developers:

- find package-manager caches, temporary build files, copied source, and other
  hidden image bloat
- verify that multi-stage builds leave compilers and development dependencies
  behind
- identify the exact layer that introduced or failed to remove a sensitive file
- trace file additions, removals, and overwrites while debugging a build
- reconstruct the image's instruction history when the original Dockerfile is
  unavailable

Deleting a secret in a later layer does not remove it from the earlier layer
where it was first added. Rotate any exposed credential; image cleanup alone is
not sufficient.

Use arrow keys to select layers and browse files. Press `Tab` to switch panes,
`Ctrl+L` for the selected layer, `Ctrl+A` for aggregated changes, and `Q` or
`Ctrl+C` to exit.

For non-interactive image-efficiency checks:

```sh
CI=true porto docker dive your-image:tag
```

Dive analyzes image contents and wasted space. It is not a vulnerability
scanner and does not claim that an image is free from CVEs.

## Local SBOM and Grype analysis

Select an image and open **SBOM and vulnerabilities**. Porto exports the selected
Linux OCI image from its own runtime, verifies the immutable manifest,
configuration, platform and layer checksums, and catalogs the current
(`squashed`) filesystem with [Syft](https://github.com/anchore/syft). It passes
that SBOM to [Grype](https://github.com/anchore/grype) for local advisory matching.
Image contents and source code are never uploaded, and analysis does not pull
missing registry layers or modify images, containers or their lifecycle.

Desktop packages pin Apache-2.0 Syft **1.55.0** and Grype **0.121.0** using
per-platform release-archive SHA-256 checksums. Licenses and provenance are in
`runtime/licenses` and `runtime/VERSIONS`; startup verifies their presence and
versions. Source/CLI installations need `syft` and `grype` on the daemon's
`PATH`. Scanner updates are reviewed package updates, not background binary
replacement. Both tools have native macOS/Linux AMD64/ARM64 and Windows AMD64
packages. Syft also supports Windows ARM64; upstream Grype does not publish a
native Windows ARM64 binary, so that target reports unavailable rather than
substituting x64. Native Windows image analysis is not currently supported.

Results retain the index digest separately from the verified platform manifest.
Cache identity includes both, the exact selected platform, both scanner
versions, database content checksum/schema/build timestamp, maximum age and
analysis options. A changed tag, platform, scanner or database therefore cannot
reuse another variant's findings. **Force rescan** bypasses the cache.

The report includes package PURLs/types, licenses, source paths and layer
attribution where Syft provides it, distribution, advisory namespace/link,
severity, match type, affected version and recorded fixed versions. Layer
digests do not prove a base-image name or a safe replacement tag; unknown
ancestry and unattributed packages remain explicitly unknown. Fixed-version
guidance is evidence from the selected database, not a promise that an arbitrary
new image tag is safe.

### Database, freshness and offline policy

Scans are always offline. Automatic database, scanner and external-source update
checks are disabled in isolated scanner configuration. There is no hosted
account requirement. Database storage is under
`$PORTO_HOME/image-analysis/database` (the usual Porto state directory when
`PORTO_HOME` is unset); the UI shows its location, size, schema, build time and
availability.

Use **Import offline database** for an existing Grype v6 archive, or explicitly
choose **Download database**. The download fetches the HTTPS v6 listing and
checksum-verified archive from `grype.anchore.io`, sends no image identifiers,
rejects redirects outside that host, and is bounded to five minutes and at most
512 MiB. The CLI can choose a smaller download bound. Imports and updates are
serialized; in-progress scans finish against the old database before replacement.
Failed downloads/checksums are not installed. Grype validates the imported
database before analysis.

The default maximum age is 120 hours, configurable per scan from 1 to 8760
hours. A missing, unsupported or invalid database prevents matching. Explicit
**Allow stale database matching** can still produce findings, but the result
remains stale and cannot pass a CLI threshold check. Reports are historical:
their matched database identity stays visible, and they become stale as the
maximum age expires or when another database is installed.

### States and limits

Queued/running, succeeded, partial, stale, failed and canceled states are
distinct. Unknown OS distribution, no recognized packages or scanner warnings
produce partial coverage, not a clean zero. Scanner crashes, missing platforms,
legacy archives without verifiable OCI manifests, absent layers and checksum
failures are actionable failures. Distroless images with recognized packages
can be analyzed; unavailable OS/package coverage stays visible. Existing image
pull, removal and Dive do not depend on scanner/database availability.

There are at most two active scans and 16 admitted jobs, with deduplication for
the same image/platform/options, a ten-minute deadline, cancellation, an 8 GiB
image-export limit, bounded tool output, two tool workers and a 1 GiB Go memory
target per scanner process. Temporary archives are private and removed after
completion/cancellation. Cached reports and SBOMs are private local files;
new scans are blocked when saved analysis storage reaches the 1 GiB admission
limit (already admitted jobs can finish within their individual bounds).
**Clear saved analyses** removes reports/SBOMs/cache entries only when no scan
is active, preserving the database and runtime images. Export needed reports
first.
SPDX/CycloneDX exports contain software inventory, not registry credentials.
Do not treat any scanner, a zero match count or a layer inspector as a guarantee
that an image has no vulnerabilities.

### CLI and machine-readable exports

Flags precede positional arguments:

```sh
porto image-analysis database status
porto image-analysis database update --confirm-network --max-download-mib 256
porto image-analysis database import /path/to/offline-database.tar.zst
porto image-analysis scan --platform linux/arm64 --fail-on high app:dev
porto image-analysis scan --max-db-age-hours 24 --force app:dev
porto image-analysis report REPORT_ID
porto image-analysis export --format spdx-json REPORT_ID > sbom.spdx.json
porto image-analysis export --format cyclonedx-json REPORT_ID > sbom.cdx.json
porto image-analysis export --format syft-json REPORT_ID > sbom.syft.json
porto image-analysis cancel REPORT_ID
```

`scan` waits, writes the final JSON report to stdout, and exits **0** only for
complete successful analysis below the chosen threshold, **1** for findings at
or above it, and **2** for invalid input, tool/database failure, partial/stale
coverage or cancellation. Without `--fail-on`, complete matching can return 0
with findings; inspect the JSON rather than interpreting 0 as "no CVEs".
Ctrl+C requests cancellation of the daemon scan.

Synthetic package/advisory, digest/platform, cache, database, cancellation and
failure tests live in `internal/imageanalysis`. Native scanner acceptance uses
`PORTO_TEST_ANALYSIS_BIN=/path/to/verified/bin go test ./internal/imageanalysis`.
Adding `PORTO_TEST_ANALYSIS_DATABASE=1` explicitly permits a bounded public
database download into the test's temporary state for real offline Grype
acceptance. No customer images or data are used.
