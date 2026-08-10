#!/usr/bin/env bash
#
# Cross-platform build script for the Beuvian Desktop Agent (macOS / Linux host).
#
# The POSIX counterpart to build-agent.ps1. Both exist because contributors are on
# all three platforms and a single script would need a shell that is not present
# everywhere; they produce byte-identical artifacts from the same commit.
#
# Usage:
#   ./scripts/build-agent.sh                     # host platform only (fast path)
#   ./scripts/build-agent.sh --target all        # all six release artifacts
#   ./scripts/build-agent.sh --target linux
#   ./scripts/build-agent.sh --version v0.1.0
#
# Options:
#   --target   host | all | windows | darwin | linux   (default: host)
#   --version  version string to stamp (default: git describe, else "dev")
#   --output   output directory                        (default: ./dist)

# -e exit on error, -u error on unset variables, -o pipefail so a failure inside a
# pipeline is not masked by a successful tail. All three matter: without them a
# failed compile is followed by a "success" message and a stale binary.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AGENT_DIR="${REPO_ROOT}/agent"
VERSION_PKG="github.com/bhuvan0808/beuviancode/shared/version"

TARGET="host"
VERSION=""
OUTPUT_DIR="${REPO_ROOT}/dist"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --target)  TARGET="${2:?--target requires a value}"; shift 2 ;;
    --version) VERSION="${2:?--version requires a value}"; shift 2 ;;
    --output)  OUTPUT_DIR="${2:?--output requires a value}"; shift 2 ;;
    -h|--help) sed -n '3,20p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done

# Colour only when writing to a terminal, so piping to a file or a CI log does not
# fill it with escape sequences.
if [[ -t 1 ]]; then
  C_CYAN=$'\033[36m'; C_GREEN=$'\033[32m'; C_RED=$'\033[31m'; C_OFF=$'\033[0m'
else
  C_CYAN=""; C_GREEN=""; C_RED=""; C_OFF=""
fi

command -v go >/dev/null 2>&1 || {
  echo "Go is not installed or not on PATH. Install Go 1.26+: https://go.dev/dl/" >&2
  exit 1
}

# --- Build metadata -----------------------------------------------------------
# The date comes from the commit, not `date`, so rebuilding a commit reproduces an
# identical binary.
GIT_VERSION="dev"
GIT_COMMIT="none"
GIT_DATE="unknown"

if command -v git >/dev/null 2>&1 && git -C "$REPO_ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  GIT_VERSION="$(git -C "$REPO_ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"
  GIT_COMMIT="$(git -C "$REPO_ROOT" rev-parse HEAD 2>/dev/null || echo none)"
  GIT_DATE="$(git -C "$REPO_ROOT" show -s --format=%cI HEAD 2>/dev/null || echo unknown)"
else
  echo "warning: not a git repository; building without version metadata" >&2
fi

[[ -n "$VERSION" ]] && GIT_VERSION="$VERSION"

echo "${C_CYAN}Toolchain : $(go version)${C_OFF}"
echo "${C_CYAN}Version   : ${GIT_VERSION}${C_OFF}"
echo "${C_CYAN}Commit    : ${GIT_COMMIT}${C_OFF}"
echo "${C_CYAN}Output    : ${OUTPUT_DIR}${C_OFF}"
echo

# --- Target matrix ------------------------------------------------------------
declare -a MATRIX
case "$(echo "$TARGET" | tr '[:upper:]' '[:lower:]')" in
  all)
    MATRIX=(
      "windows/amd64/.exe" "windows/arm64/.exe"
      "darwin/amd64/"      "darwin/arm64/"
      "linux/amd64/"       "linux/arm64/"
    ) ;;
  windows) MATRIX=("windows/amd64/.exe" "windows/arm64/.exe") ;;
  darwin)  MATRIX=("darwin/amd64/" "darwin/arm64/") ;;
  linux)   MATRIX=("linux/amd64/" "linux/arm64/") ;;
  host)
    # Ask the toolchain rather than parsing uname, which reports different
    # architecture names across platforms (x86_64 vs amd64, aarch64 vs arm64).
    MATRIX=("$(go env GOOS)/$(go env GOARCH)/") ;;
  *) echo "unknown target: $TARGET (want host|all|windows|darwin|linux)" >&2; exit 2 ;;
esac

mkdir -p "$OUTPUT_DIR"

LDFLAGS="-w -s"
LDFLAGS+=" -X ${VERSION_PKG}.Version=${GIT_VERSION}"
LDFLAGS+=" -X ${VERSION_PKG}.Commit=${GIT_COMMIT}"
LDFLAGS+=" -X ${VERSION_PKG}.Date=${GIT_DATE}"

# GOWORK=off so resolution goes through each module's replace directive, exactly as
# CI and Docker do. Building through the workspace could succeed here while a clean
# single-module clone fails.
export GOWORK=off
export CGO_ENABLED=0

declare -a BUILT=()
FAILURES=0

for entry in "${MATRIX[@]}"; do
  IFS='/' read -r goos goarch ext <<< "$entry"
  name="beuvian-agent-${goos}-${goarch}${ext}"
  out="${OUTPUT_DIR}/${name}"

  printf 'Building %-42s ' "$name"

  if (cd "$AGENT_DIR" && GOOS="$goos" GOARCH="$goarch" \
        go build -trimpath -ldflags "$LDFLAGS" -o "$out" ./cmd/beuvian-agent); then
    # BSD stat (macOS) and GNU stat (Linux) take different flags, so fall back.
    size=$(stat -f%z "$out" 2>/dev/null || stat -c%s "$out" 2>/dev/null || echo 0)
    printf '%sok (%s MB)%s\n' "$C_GREEN" "$(awk -v b="$size" 'BEGIN{printf "%.2f", b/1048576}')" "$C_OFF"
    BUILT+=("$out")
  else
    printf '%sFAILED%s\n' "$C_RED" "$C_OFF"
    FAILURES=$((FAILURES + 1))
  fi
done

# --- Checksums ----------------------------------------------------------------
if [[ ${#BUILT[@]} -gt 1 ]]; then
  echo
  echo "${C_CYAN}Writing SHA256SUMS${C_OFF}"
  (
    cd "$OUTPUT_DIR"
    # macOS ships shasum, Linux ships sha256sum. Support both rather than
    # requiring coreutils on macOS.
    if command -v sha256sum >/dev/null 2>&1; then
      sha256sum beuvian-agent-* > SHA256SUMS
    else
      shasum -a 256 beuvian-agent-* > SHA256SUMS
    fi
  )
fi

echo
if [[ $FAILURES -gt 0 ]]; then
  echo "${C_RED}${FAILURES} build(s) failed${C_OFF}" >&2
  exit 1
fi
echo "${C_GREEN}Built ${#BUILT[@]} binary/binaries into ${OUTPUT_DIR}${C_OFF}"

# A host build is meant to be run immediately, so prove it works.
if [[ "$TARGET" == "host" && ${#BUILT[@]} -eq 1 ]]; then
  echo
  "${BUILT[0]}" -version
fi
