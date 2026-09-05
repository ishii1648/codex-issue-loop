# Release gates

Stable Releaseはsuffixのないannotated `vMAJOR.MINOR.PATCH` tagだけを起点にし、alpha/beta/RC suffixをstableへ昇格しない。stableが唯一の正式releaseであり、GAを別の段階として定義しない。tag pushだけでは公開されない。`build-candidate`が作成したbinary、SBOM、manifest、checksumsを唯一の配布正本とし、比較用buildを配布へ使わない。

Release公開とproduction assignmentは別のtransactionである。Release workflowはstable公開と同一artifact readbackで完了し、repositoryを更新しない。production rolloutは独立した`Repository rollout health` workflowで、先行repository、typed rollback drill、同じartifactの再適用、対象外repository不変を検証する。rollout失敗はstableを未公開・未GAへ戻さず、対象assignmentの失敗として扱う。時間経過だけを目的とするEnvironment waitやartifact再取得waitはgateとして扱わない。

workflowは次の順で失敗停止する。

1. `build-candidate`
2. `verify-reproducibility`
3. `verify-attestation-and-manifest`
4. `replay-production-fixtures`
5. `lifecycle-conformance`
6. `cli-surface-contract`
7. `credentialless-isolated-canary`
8. `production-state-isolation`
9. `candidate-integrity`
10. `promotion-evidence`
11. `promote-stable`
12. `verify-stable-release`

`cli-surface-contract`は実際にinstallした`gh`とpinned Codex CLIの`--version`、`--help`、`features list`だけを検査し、GitHub API呼び出しもCodex inferenceも行わない。`credentialless-isolated-canary`はlocal bare Git remote、状態付きfake `gh`、状態付きfake Codex、隔離したstate rootを使い、外部networkを閉じて実candidate binaryを起動する。claimからmerge/terminalまでと、`needs_input`から停止・answer・true resume・merge/terminalまでの2 lifecycle、supervisor起動2回、5 crash boundary、webhook fixture replay、重複副作用と残存resourceが0であることを記録する。両scriptは`CANARY_GITHUB_TOKEN`または`OPENAI_API_KEY`が設定されている場合も失敗する。

GitHub-hosted runnerはproduction stateへ到達できないため、hosted canaryだけでproduction非変更を主張しない。candidate prerelease作成後、production hostで次を実行する。

```sh
PRODUCTION_REPOSITORY_PATH='/absolute/production/repository' \
PRODUCTION_AGENT_LOOP_BINARY='/absolute/assigned-runtime/agent-loop' \
CANDIDATE_BINARY='/absolute/candidate/agent-loop_Darwin_arm64' \
CONTRACT_ARTIFACT_DIR='/absolute/evidence-directory' \
RELEASE_TAG='v0.8.0' \
RELEASE_COMMIT='<40-character-merge-commit>' \
CANDIDATE_TAG='candidate-v0.8.0-<workflow-run-id>' \
scripts/production-state-isolation.sh
gh release upload 'candidate-v0.8.0-<workflow-run-id>' \
  '/absolute/evidence-directory/production-state-report.json'
```

scriptはproduction assignment runtimeの`doctor --assignment-health --json`と`status --json`だけを使用し、global operator installとは分離して、credentialless contract前後でstate revision、Issue、root active execution、pending request、worker数をbyte比較する。`worker_limit=1`、`active_workers<=1`を必須とする。supervisor稼働中はdoctor成功を要求し、保守のため意図的に停止中なら`SUPERVISOR_STOPPED`だけを許容して`active_workers=0`を要求する。その他のdoctor failureは停止中でもfail closedである。

raw snapshotとoffline contractはmode `0600`の`production-state-private-evidence.json`へ保存し、公開Releaseへuploadしない。公開する`production-state-report.json`はcandidate SHA-256、変更数0、安全条件のboolean、private evidenceのSHA-256 commitmentだけを含むredacted summaryとする。repository ID、state revision、Issue数、Issue番号、active execution、run ID、pending requestの内容、filesystem pathを公開payloadへ含めない。`production-state-isolation`はこのsummaryのrelease commitとcandidate binary SHA-256を照合してattestする。`candidate-integrity`はcandidate prereleaseからbinaryを1回取得し、canonical candidateとのbyte一致とattestationを即時検査する。immutable artifactの再取得だけを目的とした時間待機は行わない。

このrepositoryは単一maintainer運用のため、外部collaboratorや自己承認不能なrequired reviewerをrelease authorityにしない。`High-risk review gate`は変更headに結び付いたmachine-readable reviewについて全check成功・finding 0件を必須とする。`promotion-evidence`はCLI surface、offline lifecycle、production非変更、candidate integrityとdigestを即時再検証する。`production` Environmentはstable公開jobだけに付与し、wait timerは`0`とする。通常releaseのstable tagに加え、修正版workflowを実行するdefault branchを許可し、後者では入力tagとpeeled commitの一致をworkflow内でfail closedに検証する。未解決conversationはmain rulesetで引き続きmergeを拒否する。

tag push後にworkflow自体のrelease blockerを修正した場合は、tagを移動せず、default branchの修正版workflowを`workflow_dispatch`で実行する。入力したtagがannotated stable tagであること、そのpeeled commitが入力commitと一致すること、そのcommitが`main`のancestorであることを検査し、同じtagged sourceからcandidateを新規作成して全gateを再実行する。

manifestのsemantic contract predicateを変更する場合は、tag作成前なら変更commitをrevertして旧predicateへ戻せる。tag作成後はtagやcandidateを移動・再利用せず、修正版workflowをdefault branchへmergeして上記`workflow_dispatch`から同じtagged sourceの全gateを再実行する。

失敗したcandidateをstableへ昇格しない。candidate prereleaseは監査証拠として残し、修正は新しいcommitと新しいcandidateで全gateを再実行する。production rollout failure時は対象repositoryをprevious versionへrollbackし、state、active execution、continuation、request、worktreeを手編集しない。rollout failureだけを理由にRelease workflowを失敗へ戻したり、新patchを作成したりしない。

stable公開後はrepository別assignmentによる段階展開とtyped rollback drillの後、production hostで5分間のhealth soakを行う。開始時、1分後、5分後に全repositoryのassignment、scoped doctor、statusを採取し、全sampleが成功してから同じstable Releaseへreportを追加する。定期LaunchAgentの実行やEnvironment timerを待つ必要はない。

```sh
PRODUCTION_AGENT_LOOP_BINARY='/absolute/verified/stable/agent-loop' \
PRODUCTION_REPOSITORIES_FILE='/absolute/private/repositories.json' \
ROLLBACK_DRILL_FILE='/absolute/private/rollback-drill.json' \
HEALTH_ARTIFACT_DIR='/absolute/evidence-directory' \
RELEASE_TAG='v0.9.0' \
RELEASE_COMMIT='<40-character-merge-commit>' \
STABLE_BINARY_SHA256='<stable-binary-sha256>' \
scripts/production-assignment-health.sh
gh release upload 'v0.9.0' '/absolute/evidence-directory/production-health-report.json'
gh workflow run rollout.yml --repo ishii1648/codex-issue-loop --ref main \
  -f release_tag='v0.9.0'
```

privateな`repositories.json`はrepository IDとlocal pathを入力するが、公開reportへpathを出力しない。`rollback-drill.json`は先行repositoryのstable→previous→同じstableというtyped操作と、state、Issue、execution identity、worktree、対象外repositoryのassignment/PID/binary/state revision保全を記録する。`Repository rollout health` workflowはreportに含まれる1件以上のrepositoryについてexact version/commit/digest、terminal transaction、fence不在、doctor成功、worker limit 1、active worker 1以下を照合してreportをattestする。reportが無い場合や不一致はrollout workflowだけを失敗させ、完了済みRelease workflowの結果を変更しない。
