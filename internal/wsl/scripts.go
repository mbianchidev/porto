package wsl

const dockerWrapper = `#!/bin/sh
set -eu
root="$HOME/.local/share/porto/wsl"
"$root/tools/bin/porto-wsl" ensure "$root/integration.json"
export DOCKER_HOST="$("$root/tools/bin/porto-wsl" endpoint "$root/integration.json")"
export DOCKER_CONFIG="$root/docker-config"
export PATH="$root/bin:$PATH"
unset DOCKER_CONTEXT DOCKER_TLS DOCKER_TLS_VERIFY DOCKER_CERT_PATH
exec "$root/tools/bin/docker" "$@"
`

const credentialWrapper = `#!/bin/sh
set -eu
root="$HOME/.local/share/porto/wsl"
exec "$root/tools/bin/porto-wsl" credential "$root/integration.json" "$@"
`

const installScript = `set -eu
umask 077
owner="$1"
root="$HOME/.local/share/porto/wsl"
mkdir -p "$HOME/.local/share/porto"
if [ -e "$root" ]; then
  [ ! -L "$root" ] && [ -f "$root/owner" ] && [ "$(cat "$root/owner")" = "$owner" ] || {
    echo 'WSL tooling directory is not owned by this integration; refusing replacement' >&2; exit 1;
  }
fi
for profile in "$HOME/.profile" "$HOME/.bashrc" "$HOME/.zshrc"; do
  [ ! -L "$profile" ] || { echo 'A shell profile is a symbolic link; source Porto environment explicitly instead of automatic setup' >&2; exit 1; }
done
stage="$(mktemp -d "$HOME/.local/share/porto/.wsl-stage.XXXXXX")"
backup=""
complete=false
cleanup() {
  if [ "$complete" = false ] && [ -n "$backup" ]; then
    rm -rf -- "$root"
    mv -- "$backup" "$root"
    backup=""
  fi
  rm -rf -- "$stage"
  if [ -n "$backup" ]; then rm -rf -- "$backup"; fi
}
trap cleanup EXIT
tar --no-same-owner -xf - -C "$stage"
[ "$(cat "$stage/owner")" = "$owner" ] || { echo 'WSL tool bundle ownership mismatch' >&2; exit 1; }
"$stage/tools/bin/porto-wsl" version
if [ -e "$root" ]; then
  "$root/tools/bin/porto-wsl" stop "$root/integration.json"
  backup="$(mktemp -d "$HOME/.local/share/porto/.wsl-previous.XXXXXX")"
  rmdir -- "$backup"
  mv -- "$root" "$backup"
fi
mv -- "$stage" "$root"
stage="$(mktemp -d "$HOME/.local/share/porto/.wsl-stage.XXXXXX")"
for profile in "$HOME/.profile" "$HOME/.bashrc" "$HOME/.zshrc"; do
  if [ "$profile" != "$HOME/.profile" ] && [ ! -f "$profile" ]; then continue; fi
  if [ -f "$profile" ] && grep -Fq "# Porto WSL $owner" "$profile"; then continue; fi
  printf '\n%s\n%s\n' "# Porto WSL $owner" '. "$HOME/.local/share/porto/wsl/environment" # Porto WSL environment' >> "$profile"
done
complete=true
printf '%s\n' 'WSL tooling enabled for this distribution. Open a new shell. Existing engine mounts need an explicit apply/restart.'
`

const uninstallScript = `set -eu
umask 077
owner="$1"
root="$HOME/.local/share/porto/wsl"
if [ ! -e "$root" ]; then exit 0; fi
[ ! -L "$root" ] && [ -f "$root/owner" ] && [ "$(cat "$root/owner")" = "$owner" ] || {
  echo 'WSL tooling ownership changed; refusing removal' >&2; exit 1;
}
"$root/tools/bin/porto-wsl" stop "$root/integration.json"
for profile in "$HOME/.profile" "$HOME/.bashrc" "$HOME/.zshrc"; do
  if [ ! -f "$profile" ]; then continue; fi
  [ ! -L "$profile" ] || { echo 'Shell profile ownership changed; remove its Porto WSL hook manually' >&2; exit 1; }
  temporary="$(mktemp "$profile.porto.XXXXXX")"
  awk -v marker="# Porto WSL $owner" '$0 == marker { skip=1; next } skip && $0 == ". \"$HOME/.local/share/porto/wsl/environment\" # Porto WSL environment" { skip=0; next } { skip=0; print }' "$profile" > "$temporary"
  cat "$temporary" > "$profile"
  rm -f -- "$temporary"
done
rm -rf -- "$root"
printf '%s\n' 'Removed only the selected distribution tooling. Linux engine data, named volumes, and other distributions were not changed.'
`
