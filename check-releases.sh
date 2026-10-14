#!/usr/bin/env bash
# Daily check for new stable releases of multica-ai/multica.
#
# Creates at most one issue per run: for the newest stable release published
# after the high-water mark (the baseline or the newest release that already
# has a sync issue). Older unseen releases are included by that merge.
# Prints one line and exits 0 when there is nothing to do.
#
# State lives in Multica issue metadata, see README.md:
#   registry issue (MUL-15): upstream_sync_* config and baseline
#   release issues:          upstream_sync_kind=release, upstream_sync_tag, ...
#
# Usage: check-releases.sh [--dry-run]
set -euo pipefail

REGISTRY_ISSUE="${UPSTREAM_SYNC_REGISTRY:-01a13c85-9454-73bd-ada3-6c78a2fd104a}"
UPSTREAM_REPO="multica-ai/multica"
FORK_REPO="overtheground/multica"
DRY_RUN=0
[[ "${1:-}" == "--dry-run" ]] && DRY_RUN=1

die() { echo "check-releases: $*" >&2; exit 1; }

# GitHub with retries; a failure leaves all state untouched, so the release
# is detected again by the next run.
gh_api() {
  local attempt out
  for attempt in 1 2 3 4; do
    if out=$(gh api "$@" 2>/dev/null); then printf '%s' "$out"; return 0; fi
    sleep $((attempt * 15))
  done
  return 1
}

meta() { # meta <issue-id> <key>  -> value or empty
  multica issue metadata list "$1" --output json | jq -r --arg k "$2" '.[$k] // empty'
}

record_check() { # best effort, never fails the run
  [[ $DRY_RUN == 1 ]] && return 0
  multica issue metadata set "$REGISTRY_ISSUE" --key upstream_sync_last_check_at \
    --value "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --type string >/dev/null 2>&1 || true
  multica issue metadata set "$REGISTRY_ISSUE" --key upstream_sync_last_check_result \
    --value "$1" --type string >/dev/null 2>&1 || true
}

trap 'rc=$?; [[ $rc != 0 ]] && record_check "error"; exit $rc' EXIT

baseline_tag=$(meta "$REGISTRY_ISSUE" upstream_sync_baseline_tag)
baseline_at=$(meta "$REGISTRY_ISSUE" upstream_sync_baseline_published_at)
agent_id=$(meta "$REGISTRY_ISSUE" upstream_sync_agent_id)
[[ -n "$baseline_tag" && -n "$baseline_at" && -n "$agent_id" ]] \
  || die "registry $REGISTRY_ISSUE has no baseline/agent metadata"

releases=$(gh_api "repos/$UPSTREAM_REPO/releases?per_page=50") \
  || die "GitHub releases API failed after retries"

# Stable = published, not a draft, not a prerelease, plain vX.Y.Z tag.
stable=$(jq -c '[.[] | select(.draft == false and .prerelease == false
                  and .published_at != null
                  and (.tag_name | test("^v[0-9]+\\.[0-9]+\\.[0-9]+$")))
                | {id, tag: .tag_name, published_at, url: .html_url}]
                | sort_by(.published_at)' <<<"$releases")

sync_issues=$(multica issue list --metadata upstream_sync_kind=release \
  --limit 100 --output json | jq -c '.issues')

# High-water mark: the baseline or the newest release that already has an issue.
hw_at=$(jq -r --arg b "$baseline_at" \
  '[$b] + [.[] | .metadata.upstream_sync_published_at // empty] | max' <<<"$sync_issues")

newest=$(jq -c --arg hw "$hw_at" '[.[] | select(.published_at > $hw)] | last // empty' <<<"$stable")
if [[ -z "$newest" ]]; then
  echo "no new stable release (latest known: $(jq -r 'last.tag // "none"' <<<"$stable"))"
  record_check "no-new-release"
  exit 0
fi

tag=$(jq -r .tag <<<"$newest")
release_id=$(jq -r .id <<<"$newest")
published_at=$(jq -r .published_at <<<"$newest")
url=$(jq -r .url <<<"$newest")
title="Обновить форк до Multica $tag"
skipped=$(jq -r --arg hw "$hw_at" --arg t "$tag" \
  '[.[] | select(.published_at > $hw and .tag != $t) | .tag] | join(", ")' <<<"$stable")

# One merge at a time: while a sync issue is still being worked, wait.
open=$(jq -r '[.[] | select(.status | IN("backlog","todo","in_progress","blocked"))
               | .identifier] | join(", ")' <<<"$sync_issues")
if [[ -n "$open" ]]; then
  echo "new release $tag waits: sync issue $open is still open"
  record_check "waiting:$tag"
  exit 0
fi

if [[ $DRY_RUN == 1 ]]; then
  echo "dry-run: would create '$title' (release $release_id, also covers: ${skipped:-none})"
  exit 0
fi

# Recover an issue created by an interrupted earlier run (it has the title
# but may lack metadata and still be in backlog).
issue_id=$(multica issue search "$title" --include-closed --output json \
  | jq -r --arg t "$title" '[.issues[] | select(.title == $t)][0].id // empty')

if [[ -z "$issue_id" ]]; then
  desc=$(mktemp ./upstream-sync-issue.XXXXXX.md)
  cat >"$desc" <<EOF
Новый стабильный релиз upstream: [$tag]($url) (release id $release_id, опубликован $published_at).${skipped:+
Также включает пропущенные релизы: $skipped.}

Задача: подготовить ветку форка \`$FORK_REPO\` с merge тега \`$tag\` по инструкции \`README.md\` из ветки \`upstream-sync\` форка (раздел «Подготовка релиза»). Без деплоя, без PR и issues в upstream, без force-push; базовую ветку форка не менять.

Итог: ветка \`sync/$tag\`, точка отката \`pre-sync/$tag\`, комментарий с SHA и замечаниями, статус in_review. При неустранимой проблеме — статус blocked с причиной.
EOF
  issue_id=$(multica issue create --title "$title" --description-file "$desc" \
    --assignee-id "$agent_id" --status backlog --output json | jq -r .id)
  rm -f "$desc"
  [[ -n "$issue_id" && "$issue_id" != null ]] || die "issue create returned no id"
fi

# Metadata first, then todo: the prepare run must never start without it.
multica issue metadata set "$issue_id" --key upstream_sync_kind --value release --type string >/dev/null
multica issue metadata set "$issue_id" --key upstream_sync_tag --value "$tag" --type string >/dev/null
multica issue metadata set "$issue_id" --key upstream_sync_release_id --value "$release_id" --type string >/dev/null
multica issue metadata set "$issue_id" --key upstream_sync_published_at --value "$published_at" --type string >/dev/null
multica issue metadata set "$issue_id" --key upstream_sync_state --value queued --type string >/dev/null

status=$(multica issue get "$issue_id" --output json | jq -r .status)
[[ "$status" == backlog ]] && multica issue status "$issue_id" todo >/dev/null

echo "created sync issue for $tag: $(multica issue get "$issue_id" --output json | jq -r .identifier)"
record_check "issue:$tag"
