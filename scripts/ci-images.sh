#!/usr/bin/env bash
#
# The Docker Hub images CI pulls, and their copies on GHCR.
#
# Docker Hub limits anonymous pulls per IP address, and a GitHub-hosted runner
# shares its IP with other tenants' jobs, so a CI run that pulls from Docker
# Hub fails whenever that shared budget is spent. CI pulls these images from
# copies on GHCR instead. The mirror-images workflow keeps the copies current
# (sync); each CI job pulls the ones it needs before it runs (prefetch).
#
# Usage:
#   ./scripts/ci-images.sh list     [tests|compose]...   print the Docker Hub refs
#   ./scripts/ci-images.sh sync     [tests|compose]...   copy missing or moved images to the mirror (needs crane)
#   ./scripts/ci-images.sh prefetch [tests|compose]...   pull each copy and tag it under its Docker Hub name
#
# No group means both. The refs are read from where they are declared, so
# there is no list here to keep in step:
#   tests    the pgtest image, the storage test's SeaweedFS image, and the
#            Ryuk image of the pinned testcontainers-go (pgtest relies on Ryuk
#            to remove each test binary's containers, so it stays on in CI)
#   compose  the image: lines of docker-compose.yml, and the FROM lines and
#            # syntax= frontend of docker/Dockerfile (what
#            scripts/compose-smoke.sh pulls and builds)
#
# prefetch never fails its job: an image with no copy is left for testcontainers
# or compose to pull from Docker Hub, as they would without this script.
# Testcontainers, compose and BuildKit all use an image already present under
# its name rather than pulling it.

set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

MIRROR="${CI_IMAGE_MIRROR:-ghcr.io/sky-ai-eng/mirror}"
# CI runners are linux/amd64; the copies hold that platform alone.
PLATFORM=linux/amd64

die() { echo "ci-images: $*" >&2; exit 1; }

# go_const prints the string value of `const name = "..."` in file.
go_const() {
  local file=$1 name=$2 v
  v=$(sed -nE "s/^const ${name}[[:space:]]*=[[:space:]]*\"([^\"]+)\".*/\1/p" "$file")
  [ -n "$v" ] || die "no const ${name} in ${file}"
  echo "$v"
}

ryuk_ref() {
  local dir v
  dir=$(go mod download -json github.com/testcontainers/testcontainers-go |
    sed -nE 's/^[[:space:]]*"Dir": "([^"]+)",?$/\1/p')
  [ -n "$dir" ] || die "cannot locate the testcontainers-go module"
  v=$(grep -rhoE --include='*.go' 'ReaperDefaultImage = "[^"]+"' "$dir" | sed -n 1p | cut -d'"' -f2)
  [ -n "$v" ] || die "no ReaperDefaultImage in ${dir}"
  echo "$v"
}

tests_refs() {
  go_const internal/db/pgtest/harness.go Image
  go_const internal/storage/object_test.go seaweedImage
  ryuk_ref
}

compose_refs() {
  local refs stages
  refs=$(sed -nE 's/^[[:space:]]+image:[[:space:]]*([^[:space:]#]+).*/\1/p' docker-compose.yml)
  [ -n "$refs" ] || die "no image: lines in docker-compose.yml"
  echo "$refs"
  refs=$(sed -nE 's/^FROM[[:space:]]+(--platform=[^[:space:]]+[[:space:]]+)?([^[:space:]]+).*/\2/p' docker/Dockerfile)
  [ -n "$refs" ] || die "no FROM lines in docker/Dockerfile"
  # A FROM naming an earlier stage is not an image.
  stages=$(sed -nE 's/^FROM[[:space:]].*[[:space:]][Aa][Ss][[:space:]]+([^[:space:]]+).*/\1/p' docker/Dockerfile)
  if [ -n "$stages" ]; then
    refs=$(grep -vxF -f <(printf '%s\n' "$stages") <<<"$refs" || true)
  fi
  echo "$refs"
  # BuildKit pulls the frontend a syntax directive names before it reads the
  # file. A directive is valid only in the comments that open the file.
  sed -nE '1,/^[^#]/s/^#[[:space:]]*syntax=([^[:space:]]+).*/\1/p' docker/Dockerfile
}

# list_refs prints the Docker Hub refs of the groups named, deduplicated. A ref
# naming another registry is not Docker Hub's to limit, so it is left out. A
# ref this cannot read (a build arg, say) fails, so a declaration this script
# no longer understands is noticed instead of silently dropped; so does a ref
# pinned by digest, which docker tag cannot name, so prefetch could never
# serve it. Failures are returned rather than left to set -e, so they hold
# in a caller's if or || too.
list_refs() {
  local groups=("$@") g out refs="" ref first
  [ ${#groups[@]} -gt 0 ] || groups=(tests compose)
  for g in "${groups[@]}"; do
    case $g in
      tests) out=$(tests_refs) || return 1 ;;
      compose) out=$(compose_refs) || return 1 ;;
      *)
        echo "ci-images: unknown group: $g (want tests or compose)" >&2
        return 1
        ;;
    esac
    refs+="${out}"$'\n'
  done
  # Fed by process substitution rather than a pipe, so the loop runs in this
  # shell and its return is the function's.
  while read -r ref; do
    [ -n "$ref" ] || continue
    case $ref in
      *'$'*)
        echo "ci-images: cannot read image ref: $ref" >&2
        return 1
        ;;
      *@*)
        echo "ci-images: a ref pinned by digest is not supported: $ref" >&2
        return 1
        ;;
    esac
    first=${ref%%/*}
    if [ "$first" != "$ref" ] && { [[ $first == *.* ]] || [[ $first == *:* ]] || [ "$first" = localhost ]; }; then
      continue
    fi
    echo "$ref"
  done < <(sort -u <<<"$refs")
}

# mirror_ref maps a Docker Hub ref to its copy, spelling out library/ for an
# official image: node:22-alpine -> $MIRROR/library/node:22-alpine.
mirror_ref() {
  local ref=$1 name path
  name=${ref%%:*}
  path=$name
  [[ $path == */* ]] || path=library/$path
  echo "${MIRROR}/${path}${ref#"$name"}"
}

# pinned is a ref whose tag names one release (15.1.0.147, v2.189.0). Its copy
# is never refreshed, which spares Docker Hub a request per image per run; a
# tag that tracks releases (22-alpine, 3.20) is re-checked.
pinned() {
  [[ ${1##*:} =~ ^v?[0-9]+\.[0-9]+\.[0-9]+ ]]
}

sync_mirror() {
  command -v crane >/dev/null || die "sync needs crane"
  local refs ref dst have want failed=0
  refs=$(list_refs "$@")
  for ref in $refs; do
    dst=$(mirror_ref "$ref")
    have=$(crane digest "$dst" 2>/dev/null || true)
    if [ -n "$have" ] && pinned "$ref"; then
      echo "  = ${ref} (copy exists)"
      continue
    fi
    if ! want=$(crane digest --platform "$PLATFORM" "$ref"); then
      echo "  ✗ ${ref}: cannot read it from Docker Hub" >&2
      failed=1
      continue
    fi
    if [ "$want" = "$have" ]; then
      echo "  = ${ref} (copy is current)"
      continue
    fi
    if ! crane copy --platform "$PLATFORM" "$ref" "$dst"; then
      echo "  ✗ ${ref}: copy to ${dst} failed" >&2
      failed=1
      continue
    fi
    echo "  + ${ref} -> ${dst}"
  done
  # Every image was tried; a failure is reported once the rest are copied.
  return "$failed"
}

prefetch() {
  local refs ref src err registry=${MIRROR%%/*}
  if ! refs=$(list_refs "$@"); then
    echo "::warning::ci-images: cannot list the images; Docker Hub will be used for all of them"
    return 0
  fi
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    echo "$GITHUB_TOKEN" | docker login "$registry" -u "${GITHUB_ACTOR:-github-actions}" --password-stdin >/dev/null 2>&1 ||
      echo "ci-images: cannot log in to ${registry}; pulling anonymously"
  fi
  for ref in $refs; do
    src=$(mirror_ref "$ref")
    # stderr alone is kept, for the warning to say why a pull failed.
    if err=$(docker pull -q "$src" 2>&1 >/dev/null) && err=$(docker tag "$src" "$ref" 2>&1); then
      echo "  ✓ ${ref} (from ${src})"
    else
      echo "::warning::ci-images: cannot use ${src} for ${ref} (${err%%$'\n'*}); Docker Hub will be used"
    fi
  done
}

cmd=${1:-}
[ -n "$cmd" ] || die "usage: $0 list|sync|prefetch [tests|compose]..."
shift
case $cmd in
  list) list_refs "$@" ;;
  sync) sync_mirror "$@" ;;
  prefetch) prefetch "$@" ;;
  *) die "unknown command: $cmd (want list, sync or prefetch)" ;;
esac
