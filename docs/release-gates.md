# Release gates

Stable Releaseはannotated `v*` tagを起点にするが、tag pushだけでは公開されない。`build-candidate`が作成したbinary、SBOM、manifest、checksumsを唯一の配布正本とし、比較用buildを配布へ使わない。

workflowは次の順で失敗停止する。

1. `build-candidate`
2. `verify-reproducibility`
3. `verify-attestation-and-manifest`
4. `replay-production-fixtures`
5. `lifecycle-conformance`
6. `github-cli-contract`
7. `isolated-canary`
8. `production-state-isolation`
9. `soak`
10. `production-approval`
11. `promote-stable`
12. `post-release-health`

`isolated-canary`はproductionとは別のprivate repository、credential、state root、registry root、repository IDを使う。actual contractまたはcanaryに必要なcredentialが無い場合はskipせず失敗する。canaryは2 lifecycle、supervisor restart 2回、polling fallback 1回、transaction crash recovery 1回を記録する。

GitHub-hosted runnerはproduction stateへ到達できないため、hosted canaryだけでproduction非変更を主張しない。candidate prerelease作成後、production hostで次を実行する。

```sh
PRODUCTION_REPOSITORY_PATH='/absolute/production/repository' \
PRODUCTION_AGENT_LOOP_BINARY='/absolute/installed/agent-loop' \
CANDIDATE_BINARY='/absolute/candidate/agent-loop_Darwin_arm64' \
CANARY_REPOSITORY='ishii1648/codex-issue-loop-canary' \
CANARY_ARTIFACT_DIR='/absolute/evidence-directory' \
RELEASE_TAG='v0.8.0' \
RELEASE_COMMIT='<40-character-merge-commit>' \
CANDIDATE_TAG='candidate-v0.8.0-<workflow-run-id>' \
scripts/production-state-canary.sh
gh release upload 'candidate-v0.8.0-<workflow-run-id>' \
  '/absolute/evidence-directory/production-state-report.json'
```

scriptはproductionの`doctor --json`と`status --json`だけを使用し、canary前後でstate revision、Issue、lease owner/generation、pending request、worker数をbyte比較する。`worker_limit=1`、`active_workers<=1`、doctor成功も必須である。`production-state-isolation`はreportのrelease commitとcandidate binary SHA-256を照合してattestし、成功後に30分soakを開始する。soakは開始、15分、30分のhealthを保存する。

`production-approval`はGitHub Environmentの独立reviewerを必要とする。high-risk PRは`High-risk review gate`で自動reviewとauthor以外によるlatest commitへのapprovalを検査する。stale approval、自己approval、未解決conversationをmerge authorityにしない。

失敗したcandidateをstableへ昇格しない。candidate prereleaseは監査証拠として残し、修正は新しいcommitと新しいcandidateで全gateを再実行する。production health failure時はdelivery controllerのmaintenance transactionでprevious versionへrollbackし、state、lease、park、request、worktreeを手編集しない。

stable公開後はproduction delivery controllerによる適用とsoakが完了してから、production hostで次を実行し、同じstable Releaseへreportを追加する。

```sh
PRODUCTION_REPOSITORY_PATH='/absolute/production/repository' \
PRODUCTION_AGENT_LOOP_BINARY='/absolute/installed/agent-loop' \
HEALTH_ARTIFACT_DIR='/absolute/evidence-directory' \
RELEASE_TAG='v0.8.0' \
RELEASE_COMMIT='<40-character-merge-commit>' \
STABLE_BINARY_SHA256='<stable-binary-sha256>' \
scripts/production-release-health.sh
gh release upload 'v0.8.0' '/absolute/evidence-directory/production-health-report.json'
```

`post-release-health`はinstalled version/commit、delivery `succeeded`、doctor成功、worker limit 1、active worker 1以下、rollback未発生を照合し、reportをattestする。reportが無い場合や不一致はskipせずrelease runを失敗させる。
