#!/usr/bin/env bash
#
# Generate a GitHub release body from the commits between the previous tag and
# the release tag.
#
# Usage: generate-release-notes.sh <tag> [owner/repo]
#
# Prints the Markdown body to stdout. <tag> must exist locally (fetch tags and
# full history first, e.g. actions/checkout with fetch-depth: 0).
# owner/repo defaults to $GITHUB_REPOSITORY; if it is unknown the changelog
# link is omitted.
set -euo pipefail

TAG="${1:-}"
REPO="${2:-${GITHUB_REPOSITORY:-}}"

if [[ -z "$TAG" ]]; then
  echo "usage: $0 <tag> [owner/repo]" >&2
  exit 2
fi

if ! git rev-parse --verify --quiet "${TAG}^{commit}" >/dev/null; then
  echo "error: tag not found in this repository: ${TAG}" >&2
  exit 1
fi

# Previous tag: the nearest tag reachable from the parent of the release
# commit, so the release tag itself is never selected. Empty for a first
# release (no ancestor tag), which is handled without failing.
PREV=""
if git rev-parse --verify --quiet "${TAG}^{commit}^" >/dev/null; then
  PREV="$(git describe --tags --abbrev=0 "${TAG}^{commit}^" 2>/dev/null || true)"
fi
if [[ "$PREV" == "$TAG" ]]; then
  PREV=""
fi

RANGE="${TAG}"
if [[ -n "$PREV" ]]; then
  RANGE="${PREV}..${TAG}"
fi

# Neutralize characters that would start or alter inline Markdown. Commit
# subjects are single-line, so the escaping below only needs to handle inline
# syntax.
# tformat (not format) terminates every entry with a newline, so the last
# subject is not dropped by `read`.
escape_markdown() {
  sed -e 's/\\/\\\\/g' \
      -e 's/`/\\`/g' \
      -e 's/\*/\\*/g' \
      -e 's/_/\\_/g' \
      -e 's/\[/\\[/g' \
      -e 's/\]/\\]/g' \
      -e 's/</\\</g' \
      -e 's/>/\\>/g' \
      -e 's/|/\\|/g' \
      -e 's/#/\\#/g'
}

SUBJECTS=()
while IFS= read -r subject; do
  SUBJECTS+=("$subject")
done < <(git log --no-merges --reverse --pretty=tformat:'%s' "$RANGE")

printed=0
{
  printf "## What's Changed\n\n"
  for subject in ${SUBJECTS[@]+"${SUBJECTS[@]}"}; do
    # Skip empty / whitespace-only subjects.
    if [[ -z "${subject//[[:space:]]/}" ]]; then
      continue
    fi
    printf -- '- %s\n' "$(printf '%s' "$subject" | escape_markdown)"
    printed=1
  done

  if [[ "$printed" -eq 0 ]]; then
    printf -- '- No changes.\n'
  fi

  if [[ -n "$PREV" && -n "$REPO" ]]; then
    printf '\n**Full Changelog**: https://github.com/%s/compare/%s...%s\n' \
      "$REPO" "$PREV" "$TAG"
  fi
}
