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
8. `soak`
9. `production-approval`
10. `promote-stable`
11. `post-release-health`

`isolated-canary`はproductionとは別のprivate repository、credential、state root、registry root、repository IDを使う。actual contractまたはcanaryに必要なcredentialが無い場合はskipせず失敗する。canaryは2 lifecycle、supervisor restart 2回、polling fallback 1回、transaction crash recovery 1回を記録し、30分soakは開始、15分、30分のhealthを別jobで保存する。

`production-approval`はGitHub Environmentの独立reviewerを必要とする。high-risk PRは`High-risk review gate`で自動reviewとauthor以外によるlatest commitへのapprovalを検査する。stale approval、自己approval、未解決conversationをmerge authorityにしない。

失敗したcandidateをstableへ昇格しない。candidate prereleaseは監査証拠として残し、修正は新しいcommitと新しいcandidateで全gateを再実行する。production health failure時はdelivery controllerのmaintenance transactionでprevious versionへrollbackし、state、lease、park、request、worktreeを手編集しない。
