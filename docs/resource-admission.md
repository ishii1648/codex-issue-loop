# Resource admission（廃止済み）

> Status: Superseded by [ADR-0005](adr/0005-single-execution-boundary.md)

旧resource claim、dependency metadata、slot、lease、parkによるadmissionは現行runtimeから削除した。これらをscheduler、publisher、operator command、status、release gateの判断へ使用してはならない。

現行の実行可否は次の条件だけで決める。

1. Issueがopenでready labelを持つ。
2. Issue作成者をGitHub APIで検証でき、repository ownerまたは設定済みtrusted ownerである。
3. Issueが別のactive lifecycleを持たない。
4. snapshot rootの`active_execution`が空である。

実行開始はIssueのgeneration更新、active statusへの遷移、root `active_execution`の取得を1 transactionで行う。待機・terminal・quarantine状態は実行枠を保持できない。中断後の再開材料はIssue-localな`continuation`、`suspension`、`continuation_evidence`に保存する。

旧fieldと旧statusはcurrent v5 runtime入力ではfail closedで拒否する。v4 snapshotからのupgrade時だけmigration decoderが意味を失わない範囲で現行構造へ変換する。変換不能な入力を推測で復旧しない。

同一repositoryで並列実行が将来必要になった場合は、旧admission実装を再有効化せず、Issue lifecycle APIのmajor version、state schema、障害分離要件を含む別設計として承認を得る。
