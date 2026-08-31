#!/usr/bin/env bash

set -euo pipefail

usage() {
  cat <<'EOF'
Usage: ./release.sh [--dry-run] [--push] <version>

Prepare a Porto release from main. The version may be written as 0.2.0 or
v0.2.0. By default the release commit and annotated tag are created locally.
Pass --push to atomically push main and the tag to origin.

Options:
  --dry-run  Run preflight checks and validation without changing versions,
             creating a commit or tag, fetching, or pushing.
  --push     Push the prepared release commit and tag to origin.
  -h, --help Show this help.
EOF
}

fail() {
  printf 'release.sh: %s\n' "$*" >&2
  exit 1
}

is_strict_semver() {
  local candidate="$1"
  local identifier
  local prerelease
  local version_without_build
  local -a prerelease_identifiers

  if [[ ! "$candidate" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-([0-9A-Za-z-]+)(\.[0-9A-Za-z-]+)*)?(\+([0-9A-Za-z-]+)(\.[0-9A-Za-z-]+)*)?$ ]]; then
    return 1
  fi

  version_without_build="${candidate%%+*}"
  if [[ "$version_without_build" != *-* ]]; then
    return 0
  fi

  prerelease="${version_without_build#*-}"
  IFS='.' read -r -a prerelease_identifiers <<< "$prerelease"
  for identifier in "${prerelease_identifiers[@]}"; do
    if [[ "$identifier" =~ ^[0-9]+$ && "$identifier" != "0" && "$identifier" == 0* ]]; then
      return 1
    fi
  done
}

verify_package_versions() {
  EXPECTED_VERSION="$version" node <<'NODE'
const fs = require("node:fs");

const expected = process.env.EXPECTED_VERSION;
const packageJson = JSON.parse(fs.readFileSync("ui/package.json", "utf8"));
const packageLock = JSON.parse(fs.readFileSync("ui/package-lock.json", "utf8"));
const versions = [
  ["ui/package.json", packageJson.version],
  ["ui/package-lock.json", packageLock.version],
  ['ui/package-lock.json packages[""]', packageLock.packages?.[""]?.version],
];

for (const [source, actual] of versions) {
  if (actual !== expected) {
    console.error(`${source} has version ${JSON.stringify(actual)}; expected ${expected}`);
    process.exit(1);
  }
}
NODE
}

run_validation() {
  local unformatted

  echo "Running release validation"
  unformatted="$(gofmt -l .)"
  if [ -n "$unformatted" ]; then
    printf 'gofmt reported unformatted files:\n%s\n' "$unformatted" >&2
    return 1
  fi
  go mod tidy -diff
  go vet ./...
  go test ./... -count=1
  go build -o /dev/null ./cmd/porto
  npm --prefix ui ci
  npm --prefix ui run lint
  npm --prefix ui run build
  npm --prefix ui/electron ci
  npm --prefix ui/electron audit
  node --check ui/electron/main.js
  node --check ui/electron/preload.js
}

verify_only_package_changes() {
  local changed_file
  local status_line

  while IFS= read -r status_line; do
    changed_file="${status_line:3}"
    case "$changed_file" in
      ui/package.json | ui/package-lock.json) ;;
      *) fail "release validation produced an unexpected change: ${changed_file}" ;;
    esac
  done < <(git status --porcelain --untracked-files=normal)
}

push_release() {
  local ahead
  local changed_file
  local head_sha
  local remote_main
  local remote_tag_sha

  remote_main="$(git rev-parse refs/remotes/origin/main)"
  head_sha="$(git rev-parse HEAD)"
  git merge-base --is-ancestor "$remote_main" "$head_sha" ||
    fail "local main is not based on origin/main"

  ahead="$(git rev-list --count "${remote_main}..${head_sha}")"
  if [ "$ahead" -gt 1 ]; then
    fail "local main is ${ahead} commits ahead of origin/main; expected only the release commit"
  fi
  if [ "$ahead" -eq 1 ]; then
    [ "$(git log -1 --format=%s)" = "chore(release): ${tag}" ] ||
      fail "the commit ahead of origin/main is not the ${tag} release commit"
    while IFS= read -r changed_file; do
      case "$changed_file" in
        ui/package.json | ui/package-lock.json) ;;
        *) fail "the release commit contains unexpected file: ${changed_file}" ;;
      esac
    done < <(git diff --name-only "${remote_main}..${head_sha}")
  fi

  remote_tag_sha="$(
    git ls-remote --tags origin "refs/tags/${tag}^{}" |
      awk 'NR == 1 { print $1 }'
  )"
  if [ -n "$remote_tag_sha" ]; then
    [ "$remote_tag_sha" = "$head_sha" ] ||
      fail "origin/${tag} points to a different commit"
    [ "$remote_main" = "$head_sha" ] ||
      fail "origin/${tag} exists, but origin/main does not contain the release commit"
    echo "${tag} is already published"
    return
  fi

  echo "Pushing main and ${tag} to origin"
  git push --atomic origin "HEAD:refs/heads/main" "refs/tags/${tag}"
}

push=false
dry_run=false
version_input=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --push)
      push=true
      ;;
    --dry-run)
      dry_run=true
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    -*)
      fail "unknown option: $1"
      ;;
    *)
      [ -z "$version_input" ] || fail "only one version argument is allowed"
      version_input="$1"
      ;;
  esac
  shift
done

[ -n "$version_input" ] || {
  usage >&2
  exit 1
}

version="${version_input#v}"
is_strict_semver "$version" ||
  fail "'${version_input}' is not a valid semantic version"
tag="v${version}"

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
cd "$repo_root"
[ "$(git rev-parse --show-toplevel)" = "$repo_root" ] ||
  fail "release.sh must be run from the repository containing it"

branch="$(git symbolic-ref --quiet --short HEAD)" ||
  fail "releases must be prepared from a branch, not detached HEAD"
[ "$branch" = "main" ] ||
  fail "releases must be prepared from main (current branch: ${branch})"

[ -z "$(git status --porcelain --untracked-files=normal)" ] ||
  fail "the working tree must be clean before preparing a release"

upstream="$(git rev-parse --abbrev-ref --symbolic-full-name '@{upstream}' 2>/dev/null)" ||
  fail "main must track origin/main"
[ "$upstream" = "origin/main" ] ||
  fail "main must track origin/main (current upstream: ${upstream})"

git remote get-url origin >/dev/null 2>&1 ||
  fail "the origin remote is not configured"

if ! $dry_run; then
  echo "Fetching origin/main and tags"
  git fetch --quiet origin "refs/heads/main:refs/remotes/origin/main" --tags
fi

remote_main="$(git rev-parse refs/remotes/origin/main 2>/dev/null)" ||
  fail "origin/main is not available; fetch it before using --dry-run"
head_sha="$(git rev-parse HEAD)"

if git show-ref --verify --quiet "refs/tags/${tag}"; then
  $push || fail "tag ${tag} already exists locally"
  [ "$(git cat-file -t "refs/tags/${tag}")" = "tag" ] ||
    fail "${tag} is not an annotated tag"
  [ "$(git rev-parse "${tag}^{}")" = "$head_sha" ] ||
    fail "${tag} does not point to the current main commit"
  verify_package_versions
  run_validation
  [ -z "$(git status --porcelain --untracked-files=normal)" ] ||
    fail "release validation changed the working tree"
  if $dry_run; then
    echo "Dry run complete; ${tag} is ready to push"
  else
    push_release
  fi
  exit 0
fi

[ "$head_sha" = "$remote_main" ] ||
  fail "main must match origin/main before preparing a new release"

if $dry_run; then
  run_validation
  [ -z "$(git status --porcelain --untracked-files=normal)" ] ||
    fail "release validation changed the working tree"
  echo "Dry run complete; would prepare ${tag} locally"
  $push && echo "Dry run complete; would then push main and ${tag} to origin"
  exit 0
fi

echo "Updating dashboard package metadata to ${version}"
(
  cd ui
  npm version "$version" --no-git-tag-version --allow-same-version --ignore-scripts
)
verify_package_versions
run_validation
verify_only_package_changes

if ! git diff --quiet -- ui/package.json ui/package-lock.json; then
  git add -- ui/package.json ui/package-lock.json
  git commit --no-gpg-sign -m "chore(release): ${tag}"
fi

[ -z "$(git status --porcelain --untracked-files=normal)" ] ||
  fail "the working tree changed while preparing ${tag}"

git tag -a "$tag" -m "Porto ${tag}"
echo "Created annotated tag ${tag} at $(git rev-parse --short HEAD)"

if $push; then
  push_release
else
  echo "Release remains local. Publish it with: ./release.sh ${version} --push"
fi
