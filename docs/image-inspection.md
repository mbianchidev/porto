# Inspect Docker image layers

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
scanner and does not claim that an image is free from CVEs. Local SBOM and
Grype-based vulnerability analysis remains tracked in
[issue #74](https://github.com/mbianchidev/porto/issues/74).
