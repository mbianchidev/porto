#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -lt 2 ]; then
  echo "usage: apply-runtime-patch.sh <extracted-source-directory> <patch>..." >&2
  exit 2
fi

source_root="$(cd -- "$1" && pwd -P)"
shift
patches=()
for patch in "$@"; do
  patches+=("$(cd -- "$(dirname -- "$patch")" && pwd -P)/$(basename -- "$patch")")
done

# git apply otherwise discovers an enclosing checkout and can silently skip
# every archive-relative path as outside its current working-tree prefix.
unset GIT_DIR GIT_WORK_TREE GIT_COMMON_DIR GIT_INDEX_FILE GIT_OBJECT_DIRECTORY GIT_PREFIX
export GIT_CEILING_DIRECTORIES="$(dirname -- "$source_root")"
cd "$source_root"
if git rev-parse --show-toplevel >/dev/null 2>&1; then
  echo "Runtime patches require an isolated extracted source tree, not an enclosing repository." >&2
  exit 1
fi

git apply --check "${patches[@]}"
git apply "${patches[@]}"
reverse=()
for ((index=${#patches[@]}-1; index>=0; index--)); do
  reverse+=("${patches[index]}")
done
git apply --reverse --check "${reverse[@]}"
