# ADR-0005: repositoryごとに単一実行境界を採用する

- Status: Accepted
- Date: 2026-09-04
- Supersedes: [ADR-0002](0002-concurrency-and-multi-host.md)
- Decision owners: codex-issue-loop maintainers

## Context

このsystemの目的は、GitHub Issueを1件ずつcoding workerへ渡し、個別Issueが失敗しても次のIssueを処理し続けることである。productionのworker concurrencyは1であり、同時に複数Issueを実行する要求はない。

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

これらは作業成果とprovenanceをIssue aggregateへ保持するが、repositoryのworker実行枠を解放する。supervisorは別Issueの選択を継続する。

### 4. Issue-local failure boundary

Issue番号、run ID、generationまたはIssue lifecycle intentに関連する失敗は、分類不能でもIssue-localとして扱う。対象Issueをretry、suspend、terminalまたはquarantineへ移し、active executionを解放する。

repository全体を停止できるのは、root snapshot、transaction chain、global config、全Issueに共通する認証・実行・公開authorityを安全に扱えない場合だけとする。

### 5. 公開の直列化

workerはGitHubへ公開しない。publisherはrepositoryごとに一つの論理writerとし、durable publication intentを同じidentityで冪等に処理する。公開の直列化はworker並列化のためではなく、commit、push、PR、label、commentの重複を防ぐ境界として維持する。

### 6. 旧並列stateの扱い

旧resource lease、park、slot、resource metadataはmigration decoderの入力としてだけ解釈する。現行aggregateへ変換した後のruntime判断には使用しない。

変換根拠が十分なIssueはrun、generation、workspace、request、publication identityを保持して移行する。変換できないIssueは個別にquarantineし、他Issueの処理を継続する。

## Consequences

### Positive

- queueの中心不変条件が「active executionは最大1件」に縮小する。
- 入力待ちや個別障害がresource leaseを通じて後続Issueを止めなくなる。
- recoveryはscenario別state変更ではなく、共通lifecycleとreconciliationへ集約できる。
- 個別Issueの不整合とrepository全体の破損を構造的に分離できる。
- 古いworker結果を拒否する安全性はrun IDとgenerationで維持できる。

### Negative

- 複数Issueのworker実行時間を重ねるthroughput改善は得られない。
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
