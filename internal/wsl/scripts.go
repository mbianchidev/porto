package wsl

const dockerWrapper = `#!/bin/sh
set -eu
root="$HOME/.local/share/porto/wsl"
"$root/tools/bin/porto-wsl" ensure "$root/integration.json"
export DOCKER_HOST="$("$root/tools/bin/porto-wsl" endpoint "$root/integration.json")"
export DOCKER_CONFIG="$HOME/.config/porto/wsl-docker"
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
  [ ! -L "$profile" ] || { echo 'Automatic WSL shell setup requires regular profile files; a symbolic-linked profile was preserved and no tooling was installed' >&2; exit 1; }
  [ ! -e "$profile" ] || [ -f "$profile" ] || { echo 'A shell profile is not a regular file; no tooling was installed' >&2; exit 1; }
done
transaction="$(mktemp -d "$HOME/.local/share/porto/.wsl-stage.XXXXXX")"
stage="$transaction/new"
backup="$transaction/previous"
mkdir "$stage"
had_old=false
if [ -d "$root" ]; then had_old=true; fi
publish_attempted=false
complete=false
cleanup() {
  if [ "$complete" = false ]; then
    if [ -d "$backup" ] || { [ "$had_old" = false ] && [ "$publish_attempted" = true ]; }; then
      if [ -e "$root" ]; then
        [ ! -L "$root" ] && [ "$(cat "$root/owner")" = "$owner" ] || {
          echo 'Tooling changed during rollback; preserving it and the transaction backup' >&2; return 1;
        }
        rm -rf -- "$root"
      fi
      if [ -d "$backup" ]; then mv -- "$backup" "$root"; fi
    fi
    index=0
    for profile in "$HOME/.profile" "$HOME/.bashrc" "$HOME/.zshrc"; do
      index=$((index + 1))
      if [ ! -f "$transaction/touched-$index" ]; then continue; fi
      [ ! -L "$profile" ] || { echo 'Profile changed during rollback; preserving user changes' >&2; return 1; }
      temporary="$(mktemp "$profile.porto.XXXXXX")"
      awk -v marker="# Porto WSL $owner" '$0 == marker { skip=1; next } skip && $0 == ". \"$HOME/.local/share/porto/wsl/environment\" # Porto WSL environment" { skip=0; next } { skip=0; print }' "$profile" > "$temporary"
      cat "$temporary" > "$profile"
      rm -f -- "$temporary"
    done
  fi
  rm -rf -- "$transaction"
}
trap cleanup EXIT
tar --no-same-owner -xf - -C "$stage"
[ "$(cat "$stage/owner")" = "$owner" ] || { echo 'WSL tool bundle ownership mismatch' >&2; exit 1; }
"$stage/tools/bin/porto-wsl" version
if [ -e "$root" ]; then
  "$root/tools/bin/porto-wsl" stop "$root/integration.json"
  mv -- "$root" "$backup"
fi
publish_attempted=true
mv -- "$stage" "$root"
configuration="$HOME/.config/porto/wsl-docker"
if [ -e "$configuration" ]; then
  [ ! -L "$configuration" ] && [ -f "$configuration/.porto-owner" ] &&
    [ "$(cat "$configuration/.porto-owner")" = 'Porto WSL Docker configuration v1' ] || {
      echo 'Existing WSL Docker configuration is not owned by Porto; refusing replacement' >&2; exit 1;
    }
else
  mkdir -p "$configuration"
  printf '%s\n' 'Porto WSL Docker configuration v1' > "$configuration/.porto-owner"
  printf '%s\n' '{"credsStore":"porto"}' > "$configuration/config.json"
fi
index=0
for profile in "$HOME/.profile" "$HOME/.bashrc" "$HOME/.zshrc"; do
  index=$((index + 1))
  if [ "$profile" != "$HOME/.profile" ] && [ ! -f "$profile" ]; then continue; fi
  if [ -f "$profile" ] && grep -Fq "# Porto WSL $owner" "$profile"; then continue; fi
  : > "$transaction/touched-$index"
  printf '\n%s\n%s\n' "# Porto WSL $owner" '. "$HOME/.local/share/porto/wsl/environment" # Porto WSL environment' >> "$profile"
done
complete=true
printf '%s\n' 'WSL tooling enabled for this distribution. Open a new shell. Existing engine mounts need an explicit apply/restart.'
`

const uninstallScript = `set -eu
umask 077
owner="$1"
root="$HOME/.local/share/porto/wsl"
if [ -e "$root" ]; then
  [ ! -L "$root" ] && [ -f "$root/owner" ] && [ "$(cat "$root/owner")" = "$owner" ] || {
    echo 'WSL tooling ownership changed; refusing removal' >&2; exit 1;
  }
  "$root/tools/bin/porto-wsl" stop "$root/integration.json"
fi
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
