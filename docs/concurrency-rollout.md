# Concurrency運用契約

同一repositoryのproduction concurrencyは`1`に固定する。設定値が`1`以外なら起動前に拒否し、暗黙の縮退や旧resource admissionへのfallbackは行わない。異なるrepositoryは、それぞれ独立したLaunchAgent、state、worktree root、repository assignmentを持つため並行稼働できる。

## 検証条件

- 任意のsnapshotで`active_execution`はnullまたは1件である。
- `worker_pool.limit == 1`かつ`worker_pool.active <= 1`である。
- active statusのIssueだけがroot `active_execution`を所有でき、Issue番号・run ID・generationが一致する。
- waiting、terminal、quarantineのIssueは実行枠とPID/PGIDを保持しない。
- 1 Issueが`needs_input`、retry待ち、PR/check待ち、失敗、quarantineになっても、実行枠を解放して後続ready Issueを取得できる。
- stop/restart後もroot execution identityとIssue-local continuationを照合し、重複workerを起動しない。
- release gateとproduction healthは旧`execution_lease`ではなくroot `active_execution`を検査する。

検証は`make test`、`make fault-test`、`make test-race`、`scripts/check-release.sh`で行う。productionではrepositoryごとの`doctor --assignment-health --json`と`status --json`を記録し、worker上限、実行identity、後続Issueの進行を確認する。

## 将来の並列化

同一repositoryでconcurrencyを増やす変更は設定変更として扱わない。競合境界、publication直列化、複数実行の永続identity、停止・復旧、互換性を再定義するIssue lifecycle APIのmajor updateとして設計・検証する。旧lease、resource claim、slot、parkのコードを復活させてはならない。
