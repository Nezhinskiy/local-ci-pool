#!/usr/bin/env bash
# Scan what a push makes public against a deny-list of private names: the
# tracked files' names and contents, and for every commit in the pushed range
# its message and its author and committer names and emails, and every
# annotated tag's name, tagger and message. CI's deny-list job runs it; the list
# itself lives in an Actions secret.
#
# Environment:
#   DENYLIST    one fixed string per line, matched case-insensitively
#   RANGE_FROM  the commit before the push (empty or zeros: none)
#   RANGE_TO    the pushed commit
#
# It exits 0 when nothing matches, 1 when something does, and 2 when the scan
# cannot be trusted (an empty list, a failing tool). Matched text is never
# printed: the log of a public repository is public.
set -euo pipefail

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT
patterns="$scratch/patterns"
# One fixed string per line; blank lines would match everything.
printf '%s\n' "${DENYLIST:-}" | tr -d '\r' | { grep -v '^[[:space:]]*$' || [ $? -eq 1 ]; } > "$patterns"
if [ ! -s "$patterns" ]; then
  echo "::error::the DENYLIST secret is empty or not available; the scan cannot run"
  exit 2
fi

# Fail closed: grep exits 0 (match) or 1 (no match); any other status is a
# tool error and fails the job. Files are passed as arguments, so an
# unreadable one is grep's own exit 2 rather than a skipped redirect. Never
# call it in a pipeline: the exit must take the whole script down.
matches() {
  local rc=0
  grep -aFi -f "$patterns" -- "$@" > /dev/null || rc=$?
  case "$rc" in
    0) return 0 ;;
    1) return 1 ;;
    *)
      echo "::error::grep failed with exit status $rc; the scan is not trustworthy"
      exit 2
      ;;
  esac
}

bad=0

# NUL-separated, so non-ASCII names are not octal-quoted.
git ls-files -z > "$scratch/files"
name_hits=0
content_hits=()
while IFS= read -r -d '' f; do
  if matches <<< "$f"; then
    name_hits=$((name_hits + 1))
    continue
  fi
  if [ -L "$f" ] || [ ! -f "$f" ]; then
    continue
  fi
  if matches "$f"; then
    content_hits+=("$f")
  fi
done < "$scratch/files"
if [ "$name_hits" -gt 0 ]; then
  echo "::error::$name_hits tracked file name(s) match the deny-list"
  bad=1
fi
if [ "${#content_hits[@]}" -gt 0 ]; then
  echo "::error::file contents match the deny-list in:"
  printf '%s\n' "${content_hits[@]}"
  bad=1
fi

RANGE_FROM="${RANGE_FROM:-}"
RANGE_TO="${RANGE_TO:?RANGE_TO is the pushed commit}"
if [ -z "$RANGE_FROM" ] || [[ "$RANGE_FROM" =~ ^0+$ ]] || ! git cat-file -e "${RANGE_FROM}^{commit}" 2> /dev/null; then
  range="$RANGE_TO"
else
  range="$RANGE_FROM..$RANGE_TO"
fi
git rev-list "$range" > "$scratch/commits"
message_hits=()
identity_hits=()
while read -r c; do
  git log -1 --format=%B "$c" > "$scratch/msg"
  if matches "$scratch/msg"; then
    message_hits+=("$c")
  fi
  git log -1 --format='%an%n%ae%n%cn%n%ce' "$c" > "$scratch/who"
  if matches "$scratch/who"; then
    identity_hits+=("$c")
  fi
done < "$scratch/commits"
if [ "${#message_hits[@]}" -gt 0 ]; then
  echo "::error::commit messages match the deny-list in:"
  printf '%s\n' "${message_hits[@]}"
  bad=1
fi
if [ "${#identity_hits[@]}" -gt 0 ]; then
  echo "::error::author or committer names or emails match the deny-list in:"
  printf '%s\n' "${identity_hits[@]}"
  bad=1
fi

# Every annotated tag the checkout has (a pushed tag among them): its name,
# its tagger and its message. The tag object's ID is printed, not its name.
git for-each-ref --format='%(objecttype) %(objectname)' refs/tags > "$scratch/tags"
tag_hits=()
while read -r kind obj; do
  [ "$kind" = tag ] || continue
  git cat-file tag "$obj" | sed -n '/^tag /,$p' > "$scratch/tag"
  if matches "$scratch/tag"; then
    tag_hits+=("$obj")
  fi
done < "$scratch/tags"
if [ "${#tag_hits[@]}" -gt 0 ]; then
  echo "::error::annotated tags (name, tagger or message) match the deny-list; tag objects:"
  printf '%s\n' "${tag_hits[@]}"
  bad=1
fi

exit "$bad"
