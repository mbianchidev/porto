# Local networking

Porto assigns stable ports to projects and routes friendly local hostnames over HTTP and HTTPS.

## Endpoints

| Purpose | Default endpoint |
| --- | --- |
| Dashboard and API | `http://127.0.0.1:37623` |
| Project HTTP router | `http://127.0.0.1:37680` |
| Project HTTPS router | `https://127.0.0.1:37681` |
| Portless HTTPS helper | `https://127.0.0.1:443` |

Use `http://<project>.porto.localhost:37680` without any setup. Managed
Kubernetes Service ports use the same router with stable names such as
`api-8080.default.dev.porto.localhost`. With the platform HTTPS helper installed,
use the equivalent `https://...porto.localhost/` URL without a port.

Project names retain valid dots. For example, `devoidofbeauty.com` is routed as `devoidofbeauty.com.porto.localhost`, not `devoidofbeauty-com.porto.localhost`.

## Trusted portless HTTPS

Install the trusted HTTPS helper once:

```sh
porto https install
porto https status
```

The same controls are in **Settings > Trusted local HTTPS**. Installation requests
administrator authorization only for the OS integration that needs it. The
forwarder accepts **IPv4 and IPv6 loopback only**, forwarding raw TCP to the
unprivileged TLS router at `127.0.0.1:37681`. It never reads certificate keys,
opens Porto's database, or launches workloads.

| Platform | Forwarder and ownership | Certificate trust |
| --- | --- | --- |
| macOS | Existing root-owned `dev.porto.https-forwarder` launchd helper | Current user's login keychain |
| Windows | Automatically started `PortoHTTPS` service running as `NT AUTHORITY\LocalService`; administrator-protected copy under `%ProgramData%\PortoHTTPS` | Current user's native Windows Root store, not the machine-wide store |
| systemd Linux | Owned `porto-https.socket` and `porto-https.service`; systemd binds both loopbacks and runs `systemd-socket-proxyd` with `DynamicUser=yes`, no capabilities and loopback-only egress | Distribution CA store plus the current user's Chromium NSS shared database |

Windows can display a separate native security confirmation for adding or
removing a root CA. Approve the current Porto CA when requested. Porto respects
the protected-root list and does not silently bypass that confirmation or
install the CA machine-wide.

On Debian/Ubuntu, install `ca-certificates` and `libnss3-tools` first. Fedora uses
`ca-certificates` and `nss-tools`. Linux also requires a running systemd host,
`systemd-socket-proxyd`, and the distribution's `update-ca-certificates` or
`update-ca-trust` integration. GUI authorization uses Polkit when available;
otherwise run the command in a terminal for its scoped `sudo` prompt. Never
start the daemon with `sudo`.

Linux uses Chromium's existing `~/.pki/nssdb` SQL database when present,
otherwise its current `~/.local/share/pki/nssdb` location. It initializes only an
empty database and adds a SHA-256-fingerprint-named CA with SSL-only trust.
Identity checks read only the SQL store's public certificate labels and DER
values, because NSS exports can include every certificate sharing a subject.
Native `certutil` still performs trust changes and validation; the private-key
database is not opened.
Legacy or incomplete databases are rejected rather than reset. Firefox,
Snap/Flatpak browsers, custom profiles, Java and applications with private trust
stores can require explicit CA import. Restart an already-open browser after
changing trust. No browser or client is told to ignore TLS errors.

Re-run installation after upgrading Porto to repair or refresh the owned
integration. Occupied port 443, unavailable IPv6 loopback, denied authorization,
trust failures and ownership conflicts are errors, not reasons to take over a
foreign listener, service or certificate. New partial installations roll back.
Linux uses protected system tools rather than user `PATH` commands for elevated
actions.

`porto https status` reports installation, both listeners, native/browser trust
and repair guidance. `porto https uninstall` removes the owned forwarder and URL
marker, **leaving trust in place intentionally**. Remove the current Porto CA
separately with `porto cert untrust`. Other certificates, including certificates
with the same subject but different fingerprints, are preserved. HTTP remains
available throughout.

## Certificates

The daemon creates a persistent ECDSA local certificate authority and a renewable server certificate in the `certificates` folder under Porto's platform-specific user configuration directory. On Linux, the default paths are:

```text
~/.config/porto/certificates/porto.local.pem
~/.config/porto/certificates/porto.local-key.pem
~/.config/porto/certificates/porto-root-ca.pem
~/.config/porto/certificates/porto-root-ca-key.pem
```

Use `porto cert path` to print the active paths or `porto cert generate` to replace the base server certificate. Renewal updates a running daemon immediately, and the daemon checks daily for certificates within 30 days of expiry.

The base certificate covers `porto.local`, `*.porto.local`, `porto.localhost`,
`*.porto.localhost`, `localhost`, and loopback IP addresses. Dotted project
names and Kubernetes service routes receive isolated in-memory certificates
containing only their exact hostname, selected through TLS SNI. Server
certificate renewal keeps the same trusted local authority, so browser trust
persists. Private keys are written with owner-only permissions on Unix.

## DNS and custom forwarding

Prefer `<project>.porto.localhost`, which normally resolves to loopback without configuration. Porto also accepts `.porto.local`, but `.local` is reserved for mDNS and requires exact entries in local DNS or the hosts file:

```text
127.0.0.1 porto.local api.porto.local devoidofbeauty.com.porto.local
```

Hosts files do not support wildcards, so add each project hostname separately. For a one-off request without changing DNS:

```sh
curl --resolve api.porto.local:37681:127.0.0.1 https://api.porto.local:37681
```

The HTTP router accepts both `<name>.porto.localhost` and
`<name>.porto.local` for project and Kubernetes service mappings. Without the
portless helper, HTTPS remains available on port `37681`. Use
`PORTO_TLS_ADDR` and `PORTO_TLS_PUBLIC_PORT` for custom forwarding. The installed
platform helper deliberately has a fixed, private target at `127.0.0.1:37681`;
installation refuses a different `PORTO_TLS_ADDR` rather than advertise a
nonfunctional portless URL.

Never run the Porto daemon with `sudo`, because managed projects inherit the daemon's privileges.

## Container HTTP names

Individual containers opt in to HTTP routing with a label selecting one
**published TCP container port**:

```sh
porto docker run --detach --name synthetic-api \
  --publish 127.0.0.1:18080:80 \
  --label dev.porto.http-port=80 nginx:alpine
```

The route is `http://synthetic-api.docker.porto.localhost:37680/`, or
`https://synthetic-api.docker.porto.localhost/` after HTTPS setup.
`dev.porto.hostname` optionally chooses a valid lowercase DNS name, including
dots. Compose can set the same labels. Existing project and Kubernetes routes
retain precedence; raw TCP mappings are not converted into HTTP routes.

Only an opted-in, running container with a fresh inventory and a current
loopback-reachable mapping is routed. Duplicate hostnames, stopped containers,
stale inventory and changed container/port identities are not proxied to a
possibly unrelated listener. Exact dotted SNI certificates use the same
persistent CA as projects.

## Addresses, direct connectivity and VPNs

Use **Inspect connectivity** in Settings or `porto networking inspect` for
current container/VM addresses, published endpoints, resolver results, host
interfaces, split routes and IPv4/IPv6 subnet overlap candidates.

| Backend | Direct private IP from the host | Supported connection |
| --- | --- | --- |
| Rootful local Linux containerd | Supported through its existing CNI bridge routes, subject to firewall/VPN policy | Container IP or published loopback port |
| Rootless local Linux containerd | Not host-routed | Published ports and opted-in HTTP routes |
| Lima/QEMU container engine on Windows, macOS or Linux | Guest container IPs remain private to the guest | Existing published loopback ports, HTTP routes and TCP forwards |
| Native Windows containerd/runhcs | Direct HNS-IP reachability is not advertised across host/VPN policies | Owned loopback NAT ports and opted-in HTTP/HTTPS names |
| Standalone Lima VM | Reported guest addresses do not imply a host route | Reported loopback SSH port and explicitly configured forwards |

Container HTTP names include both running Linux and native Windows inventories.
Use distinct names when both engines run: a hostname claimed by both backends is
disabled, not resolved by priority. Each request rechecks the selected backend's
current container identity, published port and inventory freshness.

An owned Linux bridge can overlap an inspected subnet intentionally. VPN
routes, especially broader split routes, can also overlap. Review the actual
interface and choose a non-conflicting Docker network subnet rather than
deleting VPN routes. Porto does not add LAN bindings, install transparent
tunnels, rewrite global DNS/WSL settings or claim universal VPN compatibility.
The inspection is refreshed from the OS; it does not treat an old snapshot as
proof of current reachability. Split-DNS and provider resolver problems remain
actionable errors. `.localhost` resolution in a browser can differ from the
host resolver; `curl --resolve` is a diagnostic, not a TLS bypass.

## Proxies and trusted registries

Porto retains inherited proxy environment settings until you explicitly save
network settings. **System and environment** mode then reads current static
macOS or Windows proxy settings when environment proxies are absent. Linux uses
the process environment; enter desktop-specific proxy settings explicitly.
PAC/WPAD evaluation is not implemented and produces a capability error instead
of silently choosing a direct connection.

**Settings > Proxy and registry trust** accepts HTTP, HTTPS and SOCKS5/SOCKS5h
URLs, bypass rules, explicit public PEM registry CA uploads and proxy-off mode.
Manual URL credentials go into the native credential vault, not the JSON
configuration. Leave an unchanged URL to keep its credential, or choose
**Clear saved proxy credentials on save**. Diagnostics redact URL userinfo.
Native vault failures are errors; there is no plaintext host fallback.
Local Porto domains, IPv4 loopback and IPv6 loopback always bypass proxies.

For CLI setup, supply a JSON object on stdin rather than put passwords in
command-line arguments or shell history:

```sh
porto networking configure <<'JSON'
{
  "mode": "manual",
  "httpProxy": "http://proxy.example:3128",
  "httpsProxy": "http://proxy.example:3128",
  "socksProxy": "",
  "noProxy": ".internal.example,10.0.0.0/8",
  "registryCAs": []
}
JSON
porto networking status
porto networking apply
```

Host registry clients and outbound runtime commands read the saved settings.
Explicitly configured registry pulls and pushes use Porto's maintained
containerd client and owner-side resolver. Containerd's server-side transfer
service cannot inherit a client's proxy credentials; Porto does not restart or
reconfigure an external containerd service to work around that boundary.
Ownership inspection, local cleanup and private transport do not depend on
proxy credential lookup. Applying to the existing, identity-verified Lima
engine requires idle BuildKit and restarts only its owned user service.
Proxy values enter the guest over private stdin, never subprocess arguments.
The guest's proxy environment and private CA bundle are owner-only files under
`~/.config/porto/network`; access to that VM is administrator-equivalent.
An unchanged CA is not replaced during server certificate renewal.

Host loopback HTTP/SOCKS proxies use `host.lima.internal` from the guest. A
TLS-encrypted loopback proxy needs a guest-reachable hostname covered by its
certificate; Porto does not repair a name mismatch by disabling verification.
Re-apply after changing OS proxy settings. External/local containerd and
BuildKit services remain administrator-owned: configure their builder
environment explicitly rather than letting Porto alter unrelated services.

Import a public CA through the file selector, or use
`porto networking ca-import < public-registry-ca.pem` and place its returned
`sha256:...` reference in the `registryCAs` array. The API accepts only imported
references, never caller-supplied host paths.

Registry CAs are validated, copied into a confined owned store and added to registry-client
or owned-builder trust only. They are not injected into containers, Kubernetes
pods or standalone VMs. Even Porto's development CA requires an explicit
selection. Remove the selected references, save and apply again to reverse that
owned trust change. Workload trust propagation remains a separate user action.

### Native Windows registry networking

Saving settings also applies them to a running, verified native worker.
Native install/start resolves credentials as the owning user and sends the
resolved settings and public CA bytes over its verified private pipe. The
LocalSystem worker never reads the user's vault or project configuration.
Its recoverable copy is Windows DPAPI-encrypted, bound to its owner identity,
and protected by the runtime's administrator/System ACL. It is not a plaintext
proxy file or command-line argument.

Select the Windows dashboard view to inspect or apply native networking.
Equivalent commands are:

```powershell
porto networking status --windows
porto networking inspect --windows
porto networking apply --windows
```

Apply again after an OS proxy change when using the native CLI without the
dashboard. Registry operations are blocked with repair guidance if the worker's
saved networking cannot be decrypted or validated; existing workloads remain
available. Removing selected CAs and applying again revokes their registry trust.
Linux settings and native settings do not change a workload's own trust store.
Raw bearer-token registry overrides remain unsupported for configured registry
preflight; use the owning user's supported registry credential flow.
