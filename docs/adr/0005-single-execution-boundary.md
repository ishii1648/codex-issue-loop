# ADR-0005: repositoryごとに単一実行境界を採用する

- Status: Accepted
- Date: 2026-09-04
- Supersedes: [ADR-0002](0002-concurrency-and-multi-host.md)
- Decision owners: codex-issue-loop maintainers

## Context

このsystemの目的は、GitHub Issueを1件ずつcoding workerへ渡し、進行中Issueの順序を保ち、個別の停止・隔離・回答待ちでは後続Issueの処理を継続することである。productionのworker concurrencyは1であり、同時に複数Issueを実行する要求はない。

従来設計は、将来の単一host並列化とmulti-hostを見越してresource definition、path claim、dependency metadata、worker slot、resource lease、parkを現行lifecycleへ導入した。その結果、入力待ちや復旧時にも複数の所有状態を同期する必要が生じ、個別Issueの不整合がrepository全体を停止させる経路を増やした。

一方、concurrency 1でもprocess終了後の遅延結果、supervisor再起動、回答とworker完了のraceは存在する。この問題には汎用resource leaseではなく、現在の実行を識別する最小限のfencingが必要である。

## Decision

### 1. 現行product boundary

- 同一repositoryを処理するsupervisorは1つとする。
- 同時に実行するworkerは最大1つとする。
- 同一repositoryを複数hostから処理しない。
- `queue.concurrency`は`1`だけを受理する。
- worker並列化とmulti-hostは現行設計の拡張予定として保持しない。

並列化が必要になった場合は、具体的な利用要件、failure model、移行とrollback、Issue lifecycle API互換性を定義した新しいADRを先に承認する。

### 2. 最小限の実行fencing

repositoryはactive executionを0件または1件だけ持つ。active executionは次のidentityで表す。

```text
(issue number, run ID, generation)
```

worker結果、回答、retry、continuation、GitHub公開は、このidentityが現在値と一致する場合だけ状態を変更できる。古いidentityからの入力は副作用なく拒否して監査する。

これはresource ownershipを表さない。resource集合、path競合、dependency graph、slot、TTL、distributed epochを持たない。

### 3. 実行枠の解放

worker processが存在しない状態はactive executionを保持しない。

- `needs_input`
- `retry_wait`
- `awaiting_checks`
- `awaiting_merge`
- `completed`
- `failed`
- `blocked`
- quarantined Issue

これらは作業成果とprovenanceをIssue aggregateへ保持するが、repositoryのworker実行枠を解放する。新規受付は実行枠とは別に制限する。実装・公開・CI・レビュー・手動マージ待ち、予算内の自動retry・競合解消中は後続の未受付Issueをreadyのまま待機させる。`needs_input`、`failed`、`blocked`、quarantineでは対象Issueだけを保留し、後続の受付を継続する。

正常に進行するPRはマージを観測して内部完了を確定するまで順序を維持する。個別停止・隔離・回答待ちでは未マージPRが残っていても後続を止めない。PR不要の正当な完了と正式キャンセルでは解除する。既存Issue自身の観測・回答・復旧・worker再開・マージ処理は維持する。導入前から保存されている停止・隔離・回答待ちにも同じ原則を適用する。回答・復旧後の再開は既存schedulerを通し、別Issueのworkerを強制停止しない。

順序待ちはdomainの進行状態から導出し、quarantineは新規受付の停止条件にしない。新しい永続状態やlabelによる所有権は導入せず、active executionのrun/generation fencingを維持する。次の新規worktreeはfetchした最新のbase branchから作成する。

### 4. Issue-local failure boundary

Issue番号、run ID、generationまたはIssue lifecycle intentに関連する失敗は、分類不能でもIssue-localとして扱う。対象Issueをretry、suspend、terminalまたはquarantineへ移し、active executionを解放する。

repository全体を停止できるのは、root snapshot、transaction chain、global config、全Issueに共通する認証・実行・公開authorityを安全に扱えない場合だけとする。

### 5. 公開の直列化

workerはGitHubへ公開しない。publisherはrepositoryごとに一つの論理writerとし、durable publication intentを同じidentityで冪等に処理する。公開の直列化はworker並列化のためではなく、commit、push、PR、label、commentの重複を防ぐ境界として維持する。

### 6. 旧並列stateの扱い

旧resource lease、park、slot、resource metadataはmigration decoderの入力としてだけ解釈する。現行aggregateへ変換した後のruntime判断には使用しない。

変換根拠が十分なIssueはrun、generation、workspace、request、publication identityを保持して移行する。変換できないIssueは個別にquarantineし、新規受付と他Issueの処理を継続する。

## Consequences

### Positive

- workerの中心不変条件は「active executionは最大1件」を維持する。
- 未マージの先行変更を取り込んでから後続の実装を開始できる。
- recoveryはscenario別state変更ではなく、共通lifecycleとreconciliationへ集約できる。
- 個別Issueの不整合とrepository全体の破損を構造的に分離できる。
- 古いworker結果を拒否する安全性はrun IDとgenerationで維持できる。

### Negative

- 正常なCI・レビュー待ちの間は新規受付が止まり、throughputより実装順序の安定性を優先する。個別保留後の再開では、進んだbaseとの競合解消が必要になる場合がある。
- 既存のresource admission実装とstateを移行・撤去する必要がある。
- 将来並列化する場合は、現行設計の設定値を変えるだけでは導入できない。

## Rejected alternatives

### resource admissionを無効状態で保持する

runtimeに型と分岐が残り、recoveryとvalidatorが引き続き複数所有状態を扱うため不採用とする。

### concurrencyだけ1にしてleaseを維持する

待機中Issue、park、resumeでlease lifecycleが残り、単一実行という要件より強い状態同期を要求するため不採用とする。

### generationも削除する

supervisor再起動後の古いworker結果や遅延回答を現在の実行から区別できないため不採用とする。

### GitHub labelだけを現在実行の正本にする

local transactionと原子的に更新できず、遅延結果をfenceできないため不採用とする。
