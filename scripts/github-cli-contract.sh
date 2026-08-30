#!/bin/sh
set -eu

repository=${CANARY_REPOSITORY:?CANARY_REPOSITORY is required}
case "$repository" in
  */codex-issue-loop-canary) ;;
  *) printf '%s\n' "refusing non-canary repository: $repository" >&2; exit 1 ;;
esac

command -v gh >/dev/null
gh auth status >/dev/null
full_name=$(gh repo view "$repository" --json nameWithOwner --jq .nameWithOwner)
repo_id=$(gh api "repos/$repository" --jq .id)
[ "$full_name" = "$repository" ]
[ -n "$repo_id" ]

run_key=${GITHUB_RUN_ID:-local}-$(date +%s)
label="contract-$run_key"
branch="contract/$run_key"
issue_number=
pr_number=
temporary_root=
cleanup() {
  if [ -n "$pr_number" ]; then gh pr close "$pr_number" --repo "$repository" --delete-branch >/dev/null 2>&1 || true; fi
  if [ -n "$issue_number" ]; then gh issue close "$issue_number" --repo "$repository" >/dev/null 2>&1 || true; fi
  gh label delete "$label" --repo "$repository" --yes >/dev/null 2>&1 || true
  if [ -n "$temporary_root" ] && [ -d "$temporary_root" ]; then rm -rf -- "$temporary_root"; fi
}
trap cleanup EXIT HUP INT TERM

gh label create "$label" --repo "$repository" --color 5319E7 --description "contract test $run_key"
issue_url=$(gh issue create --repo "$repository" --title "contract $run_key" --body "contract-created" --label "$label")
issue_number=${issue_url##*/}
issue_listed=false
for attempt in $(seq 1 20); do
  if gh issue list --repo "$repository" --label "$label" --json number --jq '.[].number' | grep -qx "$issue_number"; then
    issue_listed=true
    break
  fi
  sleep 1
done
[ "$issue_listed" = true ]
gh issue view "$issue_number" --repo "$repository" --json body --jq .body | grep -qx contract-created
gh issue edit "$issue_number" --repo "$repository" --body contract-edited --remove-label "$label"
gh issue view "$issue_number" --repo "$repository" --json body,labels --jq '.body + " " + ([.labels[].name] | join(","))' | grep -q '^contract-edited '
gh issue edit "$issue_number" --repo "$repository" --add-label "$label"

temporary_root=$(mktemp -d "${TMPDIR:-/tmp}/agent-loop-gh-contract.XXXXXX")
gh repo clone "$repository" "$temporary_root/repo" -- --quiet
(
  cd "$temporary_root/repo"
  git switch -c "$branch"
  git -c user.name=codex-issue-loop-contract -c user.email=contract@example.invalid -c commit.gpgsign=false commit --allow-empty -m "contract $run_key"
  git push origin "$branch"
)
pr_url=$(gh pr create --repo "$repository" --head "$branch" --base main --title "contract $run_key" --body "Closes #$issue_number")
pr_number=${pr_url##*/}
gh pr view "$pr_number" --repo "$repository" --json number,statusCheckRollup --jq '.number, (.statusCheckRollup | length)' >/dev/null
pr_listed=false
for attempt in $(seq 1 20); do
  if gh pr list --repo "$repository" --head "$branch" --json number --jq '.[].number' | grep -qx "$pr_number"; then
    pr_listed=true
    break
  fi
  sleep 1
done
[ "$pr_listed" = true ]
gh release view --repo ishii1648/codex-issue-loop --json tagName,isDraft,isPrerelease --jq '.tagName' | grep -Eq '^v[0-9]'

artifact_dir=${CONTRACT_ARTIFACT_DIR:-$temporary_root}
mkdir -p "$artifact_dir"
printf '{"schema_version":1,"repository":"%s","repository_id_present":true,"issue":{"list":true,"get":true,"edit":true,"label_add_remove":true},"pull_request":{"create":true,"view":true,"list":true,"checks_status":true},"release_metadata":true}\n' "$full_name" >"$artifact_dir/github-cli-contract.json"
cmp "$artifact_dir/github-cli-contract.json" "$(git rev-parse --show-toplevel)/internal/contract/testdata/github-cli-contract.golden.json"
