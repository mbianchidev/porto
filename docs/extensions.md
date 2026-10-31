# Permission-scoped tooling extensions

Extensions add separately packaged tooling views, resource details and explicit
actions without rebuilding Porto. Existing sql-not-so-lite, KillSwitch, Sendbox
and image-analysis integrations keep their existing behavior. They are not
silently converted into extensions.

The supported API is **`porto.extensions/v1`**. There is no marketplace account,
automatic repository discovery, background activation or automatic update
execution. Installation and updates leave packages disabled until explicit
capability approval.

## Trust and installation

Use **Extensions > Install or update a verified package** or:

```sh
porto extension install --sha256 VERIFIED_DIGEST --developer package.porto-extension
porto extension list
porto extension enable --approve EXTENSION_ID grants.json
porto extension invoke --environment local EXTENSION_ID ACTION_ID
porto extension disable EXTENSION_ID
porto extension remove --confirm EXTENSION_ID
```

Flags precede positional arguments. Developer installation is explicit consent to
an unsigned local package, not a claim of catalog trust. Compare the digest with
a trusted source, not only the package's own digest field.

For signed packages, verify the publisher's Ed25519 public key out of band and
explicitly trust it before installation. Package integrity, publisher identity,
API compatibility and the module's supported imports are checked before the
installation record changes. A signature identifies an approved publisher; it
does not prove that the code is safe or authorize host capabilities.

Updates must increase the semantic version and retain the same developer or
trusted-publisher provenance. A different publisher requires explicit removal
and a fresh install. Updates reset approval and grants, close old action
contexts, and never automatically run new code. Invalid or interrupted updates
retain the previously committed installation and grants.

## Isolation and limits

Porto uses maintained **wazero 1.12** WebAssembly execution and **WASI preview 1**,
not Node.js `vm`, an in-process JavaScript plugin, or unrestricted Electron
objects. Every action gets a fresh module instance, only its invocation JSON on
stdin, and bounded stdout/stderr. There are no filesystem preopens, inherited
host environment variables, native host imports or directly opened network
sockets.

The module emits one JSON broker request. Porto checks its declared permission,
current approved grant, installation generation, explicit target environment
and operation constraints before executing a side effect. A module cannot
choose another environment, retrieve registry/agent credentials, install another
extension or call arbitrary daemon APIs through a runtime grant.

| Resource | Version 1 bound |
| --- | --- |
| Installed extensions | 64 |
| Concurrent invocations | 2; no automatic queue |
| Package / module / static UI | 16 MiB / 8 MiB / 512 KiB |
| Module memory | 64 MiB per active instance |
| Preparation / action | 30 seconds / 10 seconds |
| Invocation arguments / stdout / stderr | 64 KiB / 64 KiB / 16 KiB |
| Broker runtime/file/network response | 1 MiB |
| Named extension secret | 64 KiB in the OS credential store |
| Explicit native process | 5-second execution deadline and 64 KiB per output stream |

Compiled code is reused for the current immutable artifact, but module memory,
stdin/output and authority are not shared between invocations. Updates/removal
release old compiled artifacts. Cancellation, disabling, changing grants,
publisher revocation and daemon shutdown cancel active contexts. A crash or
timeout records a visible error; another invocation requires an explicit action.
Cancellation cannot undo a mutation already admitted by its target.

Views are **static HTML**, served in an opaque sandboxed iframe with no scripts,
same-origin access, forms, frames, workers, host bridge or network access.
Node integration is off; Electron's bridge and IPC checks are restricted to the
real main dashboard document. Version 1 does not offer a JavaScript/postMessage
SDK inside extension HTML. Host-rendered controls invoke declared actions and
display their results as escaped data outside the iframe. Treat static views as
untrusted content, not a way to ask users for credentials.

Native process grants are a separate, high-trust exception: the exact approved
program/argument vector runs as the host user with a minimal environment, a
temporary working directory, bounded output and process-group cleanup. They are
not WebAssembly-sandboxed and cannot safely confine an intentionally hostile
native program. They are unavailable on Windows until Job Object confinement is
supported; portable WASI actions, runtime/file/secret/network brokers and static
views work across supported desktop platforms.

## Manifest and contribution contract

An authoring directory contains `manifest.json`, `view.html`, and `action.wasm`.
The manifest requires a reverse-domain ID, a full semantic version and at least
one explicit action:

```json
{
  "apiVersion": "porto.extensions/v1",
  "id": "dev.example.inventory",
  "name": "Inventory tool",
  "version": "1.0.0",
  "description": "Inspect an explicitly selected environment.",
  "permissions": ["runtime.read"],
  "views": [
    { "id": "overview", "title": "Inventory", "location": "tools" },
    {
      "id": "container",
      "title": "Container detail",
      "location": "resource",
      "resourceKind": "container"
    }
  ],
  "actions": [
    { "id": "inventory", "title": "Read inventory", "destructive": false },
    {
      "id": "inspect",
      "title": "Inspect this container",
      "resourceKind": "container",
      "destructive": false
    }
  ]
}
```

IDs for views, actions and resource kinds are lowercase letters/digits/hyphens,
beginning with a letter, up to 64 characters. Tool views have no resource kind;
resource views require one. Built-in contribution hosts include `container`,
`image`, `pod`, and lowercased discovered Kubernetes kinds. There are at most
32 actions and 16 views. Requested permissions must be unique supported names.

An action's stdin is one JSON object:

```json
{
  "action": "inspect",
  "environmentId": "<explicit ID, or local>",
  "resourceKind": "container",
  "resourceId": "<immutable container ID>",
  "args": {},
  "confirm": false
}
```

The environment comes from Porto's selected target or the headless
`--environment` argument. A resource action must match its declared resource
kind. Destructive action labels and every runtime-control, host-write or native
process request require explicit invocation confirmation at the backend.

Emit exactly one JSON broker request on stdout, then exit successfully:

```json
{
  "capability": "runtime.read",
  "method": "GET",
  "path": "/api/docker/containers"
}
```

The host returns `{environmentId, action, data}` to the user. Broker results are
not fed back into a long-lived guest; version 1 deliberately supports one broker
operation per invocation. Pure computation can return
`{"capability":"result","data":{...}}` without host permissions.

## Grants and backend enforcement

The grant document contains exact scopes, not a wildcard daemon credential:

```json
{
  "permissions": ["runtime.read"],
  "environments": ["local"],
  "files": [],
  "commands": [],
  "secrets": [],
  "destinations": []
}
```

| Capability | Broker fields and enforcement |
| --- | --- |
| `runtime.read` | `method: "GET"`, relative `/api/...` `path`; selected environment must be granted. Sensitive inspections, reveals, streams, terminals, infrastructure and credential/extension management APIs are denied. |
| `runtime.control` | `POST` lifecycle actions on existing immutable hexadecimal container IDs in granted environments: start, stop, restart, pause, unpause, kill, remove, remove-force. No builds, creates, exec, host paths, definitions or arbitrary manifests. Resource-bound actions must retain their exact container ID. |
| `host.files.read` / `host.files.write` | Exact approved absolute existing regular `file`; no directories or symlinks. `content` is base64 for writes, and read results return base64 `content`. Opening checks file identity before truncating/writing. |
| `process.exec` | Exact absolute `executable` and exact `args` array from `commands`; no inherited credentials or arbitrary additional arguments. High-trust native execution, not a portable sandbox. |
| `secrets.read` | Exact extension-owned `secret` name, separately stored and granted. Returns `{value}`; cannot address another extension, registry or environment's vault account. |
| `network.fetch` | `url` under an explicitly granted HTTPS origin. GET only, no ambient credentials/proxy/cookies, redirects or private/link-local/metadata addresses. DNS results are checked and the actual dial uses a checked address. Returns base64 `content` and `contentType`. |

File, process and named-secret access refers to the **dashboard daemon's host**,
not a remote host selected in the environment picker. Environment grants are
immutable IDs rather than names. New or renamed profiles do not inherit grants
from an old ID.

Store a named extension secret through the dashboard or an owner-only request
file containing `{extension, name, value}`:

```sh
porto extension secret secret-request.json
```

Delete that temporary request file afterward. Removing an extension deletes its
owned vault records and packages, not explicitly accessed host files, containers
or workload data.

## Build the reference integration

The separately packaged example lives in
[`examples/extensions/resource-summary`](../examples/extensions/resource-summary).
It uses the same public manifest, WASI and broker contract as external authors.
From the repository root, with the source-build prerequisites installed:

```sh
go build -o porto ./cmd/porto
GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 \
  go build -trimpath \
  -o examples/extensions/resource-summary/action.wasm \
  ./examples/extensions/resource-summary
./porto extension pack \
  --dir examples/extensions/resource-summary \
  --out /private/path/resource-summary.porto-extension
```

The pack command prints the canonical SHA-256. Review the source and artifact,
then install the separate package, approve
[`grants.json`](../examples/extensions/resource-summary/grants.json), and invoke:

```sh
./porto extension install --developer --sha256 VERIFIED_DIGEST \
  /private/path/resource-summary.porto-extension
./porto extension enable --approve dev.porto.resource-summary \
  examples/extensions/resource-summary/grants.json
./porto extension invoke --environment local \
  dev.porto.resource-summary inventory
```

The static view appears on the Extensions page; selecting a container exposes
its resource contribution. The `inspect` action uses the safe immutable
`/api/docker/containers/ID/identity` endpoint rather than leaking raw
environment-variable values through a read-only inspection.

For a signed package, create and protect a publisher key outside the checkout:

```sh
porto extension keygen \
  --private /private/path/publisher.private \
  --public /private/path/publisher.public
porto extension pack \
  --dir examples/extensions/resource-summary \
  --out /private/path/resource-summary-signed.porto-extension \
  --signing-key /private/path/publisher.private
```

Keep the private key offline and out of Git. The installable JSON envelope
contains `content: {manifest, ui, module}`, `sha256`, and optional base64
`publicKey`/`signature`. `module` is base64 WebAssembly. The digest covers Go's
canonical JSON encoding of `content`; Ed25519 signs the 64-byte lowercase ASCII
digest string. Use the pack command rather than relying on arbitrary JSON field
ordering or whitespace from another serializer.

## Minimal signed catalog discovery

There is no background marketplace lookup. Explicitly trust a verified
publisher key with a `{publicKey, label}` request file:

```sh
porto extension trust publisher.json
porto extension catalog-import signed-catalog.json
porto extension catalog
porto extension catalog-install --publisher BASE64_PUBLIC_KEY \
  --sha256 VERIFIED_DIGEST EXTENSION_ID
```

A format-1 signed index contains `version`, `entries`, `publicKey`, and
`signature`. Entries contain `id`, `name`, `version`, `description`, HTTPS `url`,
`sha256`, and `publisher`. Each entry must use the index's trusted publisher.
The signature covers the canonical JSON `{version, entries}` returned by
Porto's `CatalogPayload` contract. The index is bounded to 128 entries per
publisher and 256 retained entries.

Create an unsigned `{version: 1, entries: [...]}` JSON index, then sign it with
the same offline publisher key used for its packages:

```sh
porto extension catalog-sign \
  --index unsigned-catalog.json \
  --signing-key /private/path/publisher.private \
  --out /private/path/signed-catalog.json
```

The command fills the publisher fields and signs the exact canonical payload.
Each artifact download is bound to the selected ID, digest and publisher, not
merely the first same-named entry from a different catalog.

The dashboard shows imported trusted entries and explicitly confirms each
download. It verifies artifact digest, identity, version and publisher before
installation, then still requires capability approval. Revoking publisher trust
disables its extensions, cancels actions and removes its entries from discovery.

## Compatibility, diagnostics and recovery

Docker Extensions was evaluated: it uses a different image/`metadata.json`
package, JavaScript SDK and host/VM backend model. Porto's static HTML plus
WASI/broker API is **not Docker Extensions-compatible** and is not labeled as a
drop-in SDK. No Docker Extension image or repository manifest is automatically
executed.

The Extensions page reports compatibility, provenance, grants, generation,
last-run state and errors. Denied permissions are enforced even if a caller
bypasses the UI. Altered package files and stale iframe generations are refused.
Repair an incompatible manifest/module, use the same verified publisher for an
update, and explicitly approve the new installation's grants.

```sh
porto extension cancel EXTENSION_ID
porto extension disable EXTENSION_ID
porto extension remove --confirm EXTENSION_ID
```

An interrupted update never changes the committed package pointer. A canceled or
failed side effect is not automatically replayed or rolled back: inspect its
exact environment/resource before retrying. If the dashboard is unusable, use
the headless disable/remove commands; the daemon does not execute extension code
on startup or merely because files exist.

## Loopback API summary

These routes are local management surfaces and are never forwarded by the
remote agent. Unknown JSON fields, extra JSON values and oversized requests are
rejected. Errors use the management `{code, message}` format.

| Method and path | Contract |
| --- | --- |
| `GET /api/extensions` | Installed metadata, compatibility and diagnostics; no secret values |
| `POST /api/extensions` | `{package, expectedSHA256, developer}`; verify/install/update, 201 disabled installation |
| `PUT /api/extensions/ID/configuration` | `{enabled, approve, grants}`; explicit approval and generation change |
| `POST /api/extensions/ID/invocations` | Invocation object above; scoped `{environmentId, action, data}` result |
| `POST /api/extensions/ID/cancellation` | Cancel active module/broker contexts |
| `DELETE /api/extensions/ID` | Disable, cancel and remove owned package/credential state |
| `GET /api/extensions/ID/ui?generation=N` | Verified, approved static HTML with opaque sandbox policy |
| `POST /api/extensions/ID/secrets` | `{name, value}` stored in that extension's OS-vault namespace |
| `GET` / `POST /api/extensions/publishers` | Inspect trust; `{publicKey, label}` to trust or `{publicKey, revoke:true}` to revoke |
| `GET` / `POST /api/extensions/catalog` | Inspect imported discovery entries or verify/import a signed index |
| `POST /api/extensions/catalog/install` | `{id, sha256, publisher}`; explicit trusted entry download and verification |

Invalid manifests/artifacts return 400, denied capabilities/trust 403, stale
generation/provenance/version conflicts 409, timeouts 408, concurrency limits 429,
oversized data 413 and unavailable platform capabilities 501. No error is
converted into an empty successful result.
