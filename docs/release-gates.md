# Release gates

Stable Releaseはsuffixのないannotated `vMAJOR.MINOR.PATCH` tagだけを起点にし、alpha/beta/RC suffixをstableへ昇格しない。tag pushだけでは公開されない。`build-candidate`が作成したbinary、SBOM、manifest、checksumsを唯一の配布正本とし、比較用buildを配布へ使わない。

workflowは次の順で失敗停止する。

1. `build-candidate`
2. `verify-reproducibility`
3. `verify-attestation-and-manifest`
4. `replay-production-fixtures`
5. `lifecycle-conformance`
6. `cli-surface-contract`
7. `credentialless-isolated-canary`
8. `production-state-isolation`
9. `soak`
10. `promotion-evidence`
11. `promote-stable`
12. `post-release-health`

`cli-surface-contract`は実際にinstallした`gh`とpinned Codex CLIの`--version`、`--help`、`features list`だけを検査し、GitHub API呼び出しもCodex inferenceも行わない。`credentialless-isolated-canary`はlocal bare Git remote、状態付きfake `gh`、状態付きfake Codex、隔離したstate rootを使い、外部networkを閉じて実candidate binaryを起動する。claimからmerge/terminalまでと、`needs_input`から停止・answer・true resume・merge/terminalまでの2 lifecycle、supervisor起動2回、5 crash boundary、webhook fixture replay、重複副作用と残存resourceが0であることを記録する。両scriptは`CANARY_GITHUB_TOKEN`または`OPENAI_API_KEY`が設定されている場合も失敗する。

GitHub-hosted runnerはproduction stateへ到達できないため、hosted canaryだけでproduction非変更を主張しない。candidate prerelease作成後、production hostで次を実行する。

```sh
PRODUCTION_REPOSITORY_PATH='/absolute/production/repository' \
PRODUCTION_AGENT_LOOP_BINARY='/absolute/installed/agent-loop' \
CANDIDATE_BINARY='/absolute/candidate/agent-loop_Darwin_arm64' \
CONTRACT_ARTIFACT_DIR='/absolute/evidence-directory' \
RELEASE_TAG='v0.8.0' \
RELEASE_COMMIT='<40-character-merge-commit>' \
CANDIDATE_TAG='candidate-v0.8.0-<workflow-run-id>' \
scripts/production-state-isolation.sh
gh release upload 'candidate-v0.8.0-<workflow-run-id>' \
  '/absolute/evidence-directory/production-state-report.json'
```

scriptはproductionの`doctor --json`と`status --json`だけを使用し、credentialless contract前後でstate revision、Issue、lease owner/generation、pending request、worker数をbyte比較する。`worker_limit=1`、`active_workers<=1`、doctor成功も必須である。`production-state-isolation`はreportのrelease commitとcandidate binary SHA-256を照合してattestし、成功後に30分soakを開始する。soakは開始、15分、30分にcandidate prereleaseからbinaryを再取得し、canonical candidateとのbyte一致とattestationを検査する。

このrepositoryは単一maintainer運用のため、外部collaboratorや自己承認不能なrequired reviewerをrelease authorityにしない。`High-risk review gate`は変更headに結び付いたmachine-readable reviewについて全check成功・finding 0件を必須とする。`promotion-evidence`はCLI surface、offline lifecycle、production非変更、30分soakの全証跡とcandidate digestを再検証し、`production` Environment内で機械的な昇格許可を生成する。Environmentは`v*` refだけを許可し、production isolation前とstable promotion前にそれぞれ30分のwait timerを強制する。未解決conversationはmain rulesetで引き続きmergeを拒否する。

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
