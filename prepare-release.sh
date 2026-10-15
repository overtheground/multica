#!/usr/bin/env bash
# Prepare a fork branch that merges one upstream release tag.
#
#   prepare-release.sh start  <issue-id>   merge the tag onto the fork base (detached HEAD)
#   (resolve conflicts, git commit)
#   prepare-release.sh finish <issue-id>   verify, light build, push sync/<tag> + pre-sync/<tag>
#   prepare-release.sh fail   <issue-id> <reason>
#
# Run inside a checkout of the multica repo with remotes `origin`
# (multica-ai/multica) and `fork` (overtheground/multica). Never force-pushes,
# never touches the base branch. Add --dry-run after the issue id to skip
# pushes and metadata writes.
set -euo pipefail

REGISTRY_ISSUE="${UPSTREAM_SYNC_REGISTRY:-01a13c85-9454-73bd-ada3-6c78a2fd104a}"
UPSTREAM_REPO="multica-ai/multica"
cmd="${1:-}"; issue="${2:-}"
[[ -n "$cmd" && -n "$issue" ]] || { sed -n '2,12p' "$0"; exit 64; }
DRY_RUN=0
[[ "${3:-}" == "--dry-run" ]] && DRY_RUN=1

die() { echo "prepare-release: $*" >&2; exit 1; }
meta() { multica issue metadata list "$1" --output json | jq -r --arg k "$2" '.[$k] // empty'; }
set_meta() {
  [[ $DRY_RUN == 1 ]] && { echo "dry-run: meta $2=$3"; return 0; }
  multica issue metadata set "$1" --key "$2" --value "$3" --type string >/dev/null
}

if [[ "$cmd" == fail ]]; then
  set_meta "$issue" upstream_sync_state failed
  set_meta "$issue" upstream_sync_error "${3:-unspecified}"
  exit 0
fi

tag="${UPSTREAM_SYNC_TAG:-$(meta "$issue" upstream_sync_tag)}"  # override: manual tests only
[[ -n "$tag" ]] || die "issue $issue has no upstream_sync_tag metadata"
base_branch=$(meta "$REGISTRY_ISSUE" upstream_sync_base_branch)
[[ -n "$base_branch" ]] || die "registry has no upstream_sync_base_branch"
# What production runs now. The base must contain it, so a stale base branch
# cannot drop deployed fixes or applied migrations.
deployed_sha=$(meta "$REGISTRY_ISSUE" upstream_sync_deployed_sha)
[[ -n "$deployed_sha" ]] || die "registry has no upstream_sync_deployed_sha"
sync_branch="sync/$tag"
rollback_tag="pre-sync/$tag"

git rev-parse --is-inside-work-tree >/dev/null || die "not in a git checkout"
git remote get-url fork >/dev/null 2>&1 || die "remote 'fork' missing"
git fetch --quiet origin "refs/tags/$tag:refs/tags/$tag" \
  || [[ $DRY_RUN == 1 ]] && git rev-parse --verify --quiet "refs/tags/$tag" >/dev/null \
  || die "cannot fetch upstream tag $tag"
git fetch --quiet fork
tag_sha=$(git rev-parse "$tag^{commit}")

case "$cmd" in
start)
  # The tag must still be a published stable release pointing at the same commit.
  if [[ $DRY_RUN == 0 ]]; then
    rel=$(gh api "repos/$UPSTREAM_REPO/releases/tags/$tag") || die "release $tag not found on GitHub"
    jq -e '.draft == false and .prerelease == false' <<<"$rel" >/dev/null || die "$tag is not a stable release"
    remote_tag_sha=$(gh api "repos/$UPSTREAM_REPO/commits/$tag" --jq .sha) || die "cannot resolve $tag on GitHub"
    [[ "$remote_tag_sha" == "$tag_sha" ]] || die "tag $tag moved upstream ($remote_tag_sha != $tag_sha)"
  fi

  if git rev-parse --verify --quiet "refs/remotes/fork/$sync_branch" >/dev/null; then
    echo "fork already has $sync_branch (earlier attempt); continuing from it"
    git switch --quiet --detach "fork/$sync_branch"
    echo "next: prepare-release.sh finish $issue"
    exit 0
  fi
  [[ -z "$(git status --porcelain)" ]] || die "working tree is not clean"

  base_sha=$(git rev-parse "fork/$base_branch")
  git merge-base --is-ancestor "$deployed_sha" "$base_sha" \
    || die "fork/$base_branch ($base_sha) does not contain deployed $deployed_sha; fix upstream_sync_base_branch in the registry"
  ahead=$(git log --oneline "$deployed_sha..$base_sha")
  [[ -n "$ahead" ]] && { echo "NOTE: $base_branch has commits not yet deployed (they go into the sync branch):"; echo "$ahead"; }
  if git merge-base --is-ancestor "$tag_sha" "$base_sha"; then
    die "$tag is already contained in $base_branch ($base_sha); nothing to merge"
  fi
  set_meta "$issue" upstream_sync_base_branch "$base_branch"
  set_meta "$issue" upstream_sync_base_sha "$base_sha"
  set_meta "$issue" upstream_sync_deployed_sha "$deployed_sha"
  set_meta "$issue" upstream_sync_state merging

  git switch --quiet --detach "$base_sha"
  if git merge --no-ff --no-edit -m "Merge upstream release $tag into $base_branch" "$tag_sha"; then
    echo "merged cleanly; next: prepare-release.sh finish $issue"
  else
    echo
    echo "CONFLICTS — resolve, keep both sides' intent (fork features + upstream), then:"
    echo "  git add -A && git commit --no-edit && prepare-release.sh finish $issue"
    git diff --name-only --diff-filter=U | sed 's/^/  conflict: /'
    exit 3
  fi
  ;;

finish)
  base_sha=$(meta "$issue" upstream_sync_base_sha)
  [[ -n "$base_sha" ]] || base_sha=$(git merge-base HEAD "fork/$base_branch")
  [[ -z "$(git diff --name-only --diff-filter=U)" ]] || die "unresolved conflicts remain"
  [[ -z "$(git status --porcelain --untracked-files=no)" ]] || die "uncommitted changes; commit the merge first"
  git merge-base --is-ancestor "$tag_sha" HEAD || die "HEAD does not contain $tag"
  git merge-base --is-ancestor "$base_sha" HEAD || die "HEAD does not contain base $base_sha"
  git merge-base --is-ancestor "$deployed_sha" HEAD || die "HEAD does not contain deployed $deployed_sha"

  # Migrations are append-only: every file production has applied stays
  # byte-identical under its name; upstream and fork only add files.
  touched=$(git diff --name-status "$deployed_sha" HEAD -- server/migrations/ | grep -v '^A' || true)
  [[ -z "$touched" ]] || die "deployed migrations were changed, renamed or removed:
$touched"

  # Upstream license files stay exactly upstream's; FORK.md must survive.
  git diff --quiet "$tag_sha" HEAD -- LICENSE NOTICE || die "LICENSE/NOTICE differ from upstream $tag"
  git cat-file -e HEAD:FORK.md 2>/dev/null || die "FORK.md is missing"

  if git diff "$base_sha" HEAD | grep -qE '^\+(<<<<<<<|>>>>>>>)( |$)'; then
    die "conflict markers were committed; check git diff $base_sha HEAD"
  fi

  # Upstream reuses some old numbers itself; report only collisions this merge adds.
  dups() { git ls-tree --name-only "$1" server/migrations/ \
           | sed -n 's#.*/\([0-9]*\)_\(.*\)\.\(up\|down\)\.sql#\1 \2#p' | sort -u | awk '{print $1}' | uniq -d; }
  dup=$(comm -13 <(dups "$base_sha") <(dups HEAD) | tr '\n' ' ')
  [[ -n "$dup" ]] && echo "WARNING: new migration number collisions: $dup(see FORK.md, step 2)"

  # Light build check only: the backend compiles.
  go_bin=$(command -v go || echo "$HOME/.local/sdk/go1.27.1/bin/go")
  [[ -x "$go_bin" ]] || die "go toolchain not found"
  (cd server && "$go_bin" build ./...) || die "go build ./... failed"
  echo "go build ./... ok"

  head_sha=$(git rev-parse HEAD)
  if [[ $DRY_RUN == 1 ]]; then
    echo "dry-run: would push $head_sha to $sync_branch and $base_sha to tag $rollback_tag"
    exit 0
  fi
  # Plain pushes: rejected rather than overwriting if the remote moved.
  git push --quiet fork "$head_sha:refs/heads/$sync_branch"
  if [[ -z "$(git ls-remote --tags fork "refs/tags/$rollback_tag")" ]]; then
    git push --quiet fork "$base_sha:refs/tags/$rollback_tag"
  fi
  set_meta "$issue" upstream_sync_branch "$sync_branch"
  set_meta "$issue" upstream_sync_prepared_sha "$head_sha"
  set_meta "$issue" upstream_sync_state prepared
  echo "prepared: $sync_branch = $head_sha (base $base_branch @ $base_sha, rollback tag $rollback_tag, upstream $tag @ $tag_sha)"
  ;;
*) die "unknown command $cmd" ;;
esac
