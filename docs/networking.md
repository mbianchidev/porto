# Local networking

Porto assigns stable ports to projects and routes friendly local hostnames over HTTP and HTTPS.

## Endpoints

| Purpose | Default endpoint |
| --- | --- |
| Dashboard and API | `http://127.0.0.1:37623` |
| Project HTTP router | `http://127.0.0.1:37680` |
| Project HTTPS router | `https://127.0.0.1:37681` |
| Portless HTTPS helper | `https://127.0.0.1:443` |

Use `http://<project>.porto.localhost:37680` without any setup. With the macOS HTTPS helper installed, use `https://<project>.porto.localhost/`.

Project names retain valid dots. For example, `devoidofbeauty.com` is routed as `devoidofbeauty.com.porto.localhost`, not `devoidofbeauty-com.porto.localhost`.

## Portless HTTPS on macOS

Install the trusted HTTPS helper once:

```sh
porto https install
```

The command uses the native macOS administrator authorization dialog only to install a dedicated IPv4 and IPv6 loopback TCP forwarder. The root-owned launchd helper forwards raw TCP from `127.0.0.1:443` to Porto's unprivileged TLS router on `127.0.0.1:37681`. It does not read certificate keys, open Porto's database, or launch projects.

The command also trusts Porto's local certificate authority in the current user's login keychain. Re-run it after upgrading the Porto binary to refresh the helper.

Use `porto https status` to inspect installation, listener, and trust state. `porto https uninstall` removes the forwarder, while `porto cert untrust` removes certificate trust. The zero-configuration HTTP route remains available whether or not the helper is installed.

## Certificates

The daemon creates a persistent ECDSA local certificate authority and a renewable server certificate in the `certificates` folder under Porto's platform-specific user configuration directory. On Linux, the default paths are:

```text
~/.config/porto/certificates/porto.local.pem
~/.config/porto/certificates/porto.local-key.pem
~/.config/porto/certificates/porto-root-ca.pem
~/.config/porto/certificates/porto-root-ca-key.pem
```

Use `porto cert path` to print the active paths or `porto cert generate` to replace the base server certificate. Renewal updates a running daemon immediately, and the daemon checks daily for certificates within 30 days of expiry.

The base certificate covers `porto.local`, `*.porto.local`, `porto.localhost`, `*.porto.localhost`, `localhost`, and loopback IP addresses. Dotted project names receive isolated in-memory certificates containing only their exact hostname, selected through TLS SNI. Server certificate renewal keeps the same trusted local authority, so browser trust persists. Private keys are written with owner-only permissions on Unix.

## DNS and custom forwarding

Prefer `<project>.porto.localhost`, which normally resolves to loopback without configuration. Porto also accepts `.porto.local`, but `.local` is reserved for mDNS and requires exact entries in local DNS or the hosts file:

```text
127.0.0.1 porto.local api.porto.local devoidofbeauty.com.porto.local
```

Hosts files do not support wildcards, so add each project hostname separately. For a one-off request without changing DNS:

```sh
curl --resolve api.porto.local:37681:127.0.0.1 https://api.porto.local:37681
```

The HTTP router accepts both `<project>.porto.localhost` and `<project>.porto.local`. Without the portless helper, HTTPS remains available on port `37681`. Use `PORTO_TLS_ADDR` and `PORTO_TLS_PUBLIC_PORT` for custom forwarding.

Never run the Porto daemon with `sudo`, because managed projects inherit the daemon's privileges.
