# ADR-0002: 単一ホスト並列化と複数ホスト冗長化を分離する

- Status: Superseded by [ADR-0005](0005-single-execution-boundary.md)
- Date: 2026-08-16
- Decision owners: codex-issue-loop maintainers

2026-09-04に、現行product boundaryをconcurrency 1へ固定し、resource admissionとmulti-hostを将来拡張として保持しない判断へ変更した。本書は旧判断の履歴として保持し、現行設計の根拠には使用しない。

## Context

現行v2は、repositoryごとに1つのsupervisor、1つのworker、1つのpublisherを動かす。ローカルの`flock`は同一Mac上の二重supervisorを防ぐが、別hostから同じrepositoryを処理する場合には効かない。GitHubのlabelとcommentには、claim全体を原子的にcompare-and-swapする一般的な機能も、外部のfencing tokenを検証する機能もない。

将来の拡張には、目的とfailure modelが異なる二つの課題がある。

1. **単一host内の並列化**: 1つの信頼境界とlocal stateのまま、複数Issueのworker実行時間を重ねる。
2. **複数hostの冗長化**: host障害時に別hostへ引き継ぎ、network partition中の二重claim・二重公開を防ぐ。

この二つを同じ`concurrency`設定や単純な分散lockで同時に解決しない。

## Decision

### 1. 現行互換mode

v2の既定動作を`coordination.mode: local`相当として維持する。repositoryごとにlocal `flock`を持つsupervisorは1つ、`queue.concurrency`は1だけを許可する。現在の設定・stateを読み替えて並列化やmulti-hostを暗黙に有効化しない。

### 2. 単一host並列化

単一host並列化では、supervisorを増やさずworker slotだけを増やす。

- 1つのsupervisorが候補選択、globalなclaim順序、local state transactionを所有する。
- Issueごとに独立したrun ID、branch、worktree、worker processを割り当てる。
- 選択とclaimは決定論的な順番で直列化する。workerの完了順は保証しない。
- publisherによるcommit、push、Pull Request、label、comment操作はrepository単位で直列化する。
- GitHub API rate limitとbackoffはsupervisorがglobalに調停する。
- 同じIssueへ複数slotを割り当てない。通常workerの`needs_input`はslotを占有し続けず、worktreeとpark済みresource claimを保持する。

これはlocal state schemaをIssue map中心へ変更する将来機能であり、multi-host coordinatorを必要としない。

worker slot間のresource claim、Issue本文の依存metadata、local resource leaseの取得・保持・解放は[Resource admission契約](../resource-admission.md)に従う。

### 3. 複数host冗長化

最初のmulti-host modeはactive/passiveとする。別hostはstandbyとして待機し、同一repositoryのworkerを複数hostへ同時配分しない。実装にはGitHub外の**線形化可能なcoordinator**を必須とし、次のcontractを要求する。

- repository leaseをcompare-and-swapで取得・更新・解放できる。
- lease取得ごとに単調増加する`epoch`を払い出し、古いepochの更新を拒否できる。
- repository、Issue、host、run、epoch、expiryを所有権として永続化できる。
- heartbeat、条件付き更新、durable publication intent/outboxを同じ一貫性境界で扱える。
- status/watchが全hostの所有権とattentionを集約して読める。

GitHub labelは利用者向け表示であり、ownershipの正本にはしない。各hostのlocal snapshot、event、worktreeは実行cacheと復旧材料であり、distributed ownershipの正本にはしない。

### 4. 二重公開を防ぐ境界

lease保持者がGitHub操作の直前にepochを確認するだけでは不十分である。確認直後にpartitionした古いhostは、GitHubがepochを検証できないため、そのまま副作用を出せる。

multi-host modeではworker hostからGitHubへの直接公開を禁止する。worker結果はdigest付きのimmutableなpublication intentとしてcoordinator outboxへ登録し、**単一の論理publisher**だけが次を行う。

1. intentを条件付き更新で`publishing`へ遷移し、publisher epochを確定する。
2. 同じintent IDとrun IDをGitHub comment、branch、Pull Requestの冪等markerに使う。
3. remote branchはforce pushせず、既存refが期待OIDと一致する場合だけ再利用する。
4. 既存Pull Requestを検索してから作成し、結果を同じintentへ保存する。
5. `published`になったintentに対する別成果の公開を拒否する。

publisher instance自体を冗長化する場合も、GitHub副作用を実行できるnetwork pathはfenced publication gatewayへ集約する。gatewayを用意できない構成ではmulti-host modeを有効にしない。availabilityよりsafetyを優先し、coordinatorまたはgatewayへ到達できないhostは新しいclaim、worker開始、公開をfail closedする。

GitHub API呼び出しの途中で応答を失う可能性は残る。その場合は同じintent ID、branch、markerでremote状態を照合してから再実行する。別intentや別branchを作って回避してはならない。

### 5. Leaseとpartitionのfailure model

- heartbeat間隔はlease TTLの3分の1以下とし、jitterを加える。
- renewに失敗したhostは直ちに新規claimと公開を止め、expiry前のsafety marginまでにworkerを停止する。
- lease expiry後の新leaderは、より大きいepochで取得し、coordinatorに残るIssue ownershipとpublication intentをreconcileする。
- 古いepochからのheartbeat、state更新、intent登録・遷移は拒否する。
- partition中にcoordinatorへ到達できる側だけが進行できる。両側を稼働させる可用性は目標にしない。
- 公開途中のintentは消さず、新publisherがGitHubのbranch、comment、Pull Requestを照合して同じintentを再開する。
- clockはexpiry表示に使えるが、相互排他の根拠にはしない。coordinatorのrevision、transaction、epochを根拠にする。

### 6. Coordinator候補

backend名ではなく、上記contractへの適合性を採用条件にする。

| Candidate | Evaluation |
| --- | --- |
| PostgreSQL | 条件付き`UPDATE`、transaction、durable outboxを一つのserviceで構成しやすく、初期候補とする。[`UPDATE ... RETURNING`](https://www.postgresql.org/docs/current/sql-update.html)で所有権更新結果も取得できる。epoch sequenceとrow conditionを併用する。 |
| etcd | leaseとtransaction/CASに適する。公式文書も、lease ownershipだけでは相互排他にならずrevision/CASが必要と説明している。[API leases](https://etcd.io/docs/v3.5/learning/api/)、[Why etcd](https://etcd.io/docs/v3.6/learning/why/)、[concurrency API](https://etcd.io/docs/v3.6/dev-guide/api_concurrency_reference_v3/)を参照する。durable publication outboxは別途設計が必要。 |
| DynamoDB | conditional writeでownership rowを更新できる。[conditional writes](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/WorkingWithItems.html)を利用できるが、transaction、epoch、outbox、監視集約を含むadapter検証が必要。 |
| Redis TTL lock | 単独では不採用。公式の[distributed locks](https://redis.io/docs/latest/develop/clients/patterns/distributed-locks/)もfencing tokenの実装を推奨し、非同期failoverやclockの注意点を示している。durable outboxとdownstream fencingを別途満たせない。 |
| GitHub label/comment | 不採用。atomic claimとfencingを提供せず、共有表示にだけ使う。 |

実装前にbackend adapterのconformance suiteを作り、linearizable CAS、epoch単調性、expiry、partition、再起動、publication takeoverを検証する。外部coordinatorの運用、backup、認証、障害環境はこのrepositoryだけでは完結しない。

### 7. Queue、rate limit、watch

- 候補のsort規則は全hostで同じにするが、実際のclaim順はcoordinatorのglobal sequenceを正本とする。
- GitHub APIのrate budgetとretry deadlineは論理publisher/schedulerに集約し、hostごとに独立して消費しない。
- worktree pathはhost-localでよい。ownershipにはhost IDとrun IDを持たせ、別hostが同じpathを仮定しない。
- branchはIssue番号だけでなくrun/intent対応を検証する。異なる成果が同じbranchを上書きすることを許可しない。
- `status`と`watch`はcoordinator snapshotを正本として集約し、eventは低遅延化のヒント、reconciliation pollingは取りこぼし修復に使う。

## Migration and compatibility

multi-hostと単一host並列化は将来のschema updateで個別に導入する。

- 現行v2の設定・stateは`local`、concurrency 1として無変更で動く。
- 将来の設定には`coordination.mode: local|distributed`、backend、lease TTL、heartbeat、host IDを追加し、未知keyをv2 binaryが拒否する性質を維持する。
- `queue.concurrency > 1`はsingle-host parallel schemaを導入するまで拒否する。distributed modeの有効化条件とはしない。
- migrationは全loop停止、backup、preview、明示applyを必須とする。自動でdistributed modeへ移行しない。
- distributed modeへ切り替える前にcoordinator/gatewayのdoctorとconformance testを成功させる。
- localへrollbackする場合は全leaseを停止し、publication intentをdrainし、active ownershipがないことを確認する。active intentをlocal stateだけへ変換しない。

概念的な将来設定は次のとおりであり、現行binaryではまだ受理しない。

```yaml
queue:
  concurrency: 1

coordination:
  mode: distributed
  backend: postgres
  endpoint_env: AGENT_LOOP_COORDINATOR_URL
  host_id: mac-mini-a
  lease_ttl: 60s
  heartbeat_interval: 20s
```

## Consequences

### Positive

- 単一hostのthroughput改善を、distributed systemの複雑さなしに実装できる。
- network partition時は停止しても、同一Issueの二重公開を許さない。
- worker、ownership、publicationの責務と正本が明確になる。
- v2利用者のconcurrency 1動作を維持できる。

### Negative

- multi-hostには外部coordinatorとpublication gatewayの運用が必要になる。
- partition中は安全側に停止し、active/activeの可用性を得られない。
- worker成果の転送、global watch、rate limit調停、移行・rollbackの実装量が増える。

## Rejected shortcuts

- GitHub labelだけをlockとして使う。
- TTL lockを取得したhostがGitHubへ直接pushする。
- epochをlocal logに記録するだけでfencing済みとみなす。
- multi-hostを`queue.concurrency`の値だけで有効化する。
- partition時に両hostを進行させ、後で重複PRを整理する。

これらはclaimまたは公開の二重実行を安全に防げないため採用しない。
