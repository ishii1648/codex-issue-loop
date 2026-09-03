# codex-issue-loop アーキテクチャ概要

## 1. 目的と設計原則

`codex-issue-loop`は、信頼できるGitHub Issueを決定的な順序で選び、1件ずつcoding workerへ渡し、個別Issueの結果にかかわらず次のIssueを処理し続けるsupervisorである。

設計上の優先順位は次のとおりとする。

1. repository全体の正本と実行authorityを壊さない
2. 個別Issueの障害をそのIssueへ閉じ込め、queueの進行を維持する
3. worker、回答、公開の重複と古い実行世代からの更新を防ぐ
4. 作業成果、質問、公開identity、判断根拠を失わない

安全側に停止する単位は、影響を受ける最小範囲とする。Issue固有の不整合や失敗はIssue単位で隔離し、canonical repository state全体を解釈できない場合だけsupervisor全体を停止する。

## 2. システム境界

```text
Issue producer
      │ Issue
      ▼
   GitHub ── observed facts ──► queue policy ──► supervisor
      ▲                              │                │
      │                              │ trusted        │ one active execution
      │                              ▼                ▼
 publisher ◄── durable effect ── lifecycle ───────► worker
      ▲                              ▲                │
      │                              │ result/facts   │
      └──────── reconciliation ──────┴────────────────┘
                                     │
                                     ▼
                              durable state
```

- GitHubはIssue、actor、label、Pull Request、check、mergeの外部事実を所有する。
- durable stateは処理履歴、現在の実行、質問、continuation、publication intentの正本である。
- workerは1件のIssueについて非決定的な開発作業を行うが、queue、永続状態、GitHub公開を所有しない。
- supervisorは決定的な選択、遷移、外部effectの調停を行う。
- eventとwebhookは起床のhintであり、実行authorityにはならない。
- Codex監視taskは操作画面であり、supervisorや状態の正本ではない。

Release deliveryは別bounded contextである。releaseの取得、検証、repository別assignment、drain、install、health check、rollbackはIssue lifecycleへ混在させない。

## 3. 責務

| 責務 | 入力 | 出力 | 所有しないもの |
| --- | --- | --- | --- |
| GitHub observer | Issue、actor permission、PR、check | 正規化した外部事実 | lifecycle判断 |
| author policy | actor identity、permission、allowlist | trusted / rejected / unverifiable | GitHub API呼び出し |
| queue policy | trustedな候補集合、順序設定、snapshot | 次に開始するIssueまたは待機理由 | worker実行 |
| lifecycle | 現在のIssue aggregate、intent、観測事実 | 次状態、effect intent、audit | 外部I/O |
| supervisor | snapshot、deadline、外部事実 | use caseの実行順序 | lifecycle規則の再実装 |
| worker | Issue文脈、worktree、回答 | 構造化された結果 | commit、push、PR、次Issue選択 |
| publisher | durable publication intent、Git事実 | commit、push、PRの冪等な結果 | 実装判断 |
| reconciler | snapshotと現在の外部事実 | lifecycleへ渡すreconcile intent | scenario固有のstate更新 |
| state store | 検証済みdecision | atomic snapshot、event | domain判断 |
| monitor | snapshot | status、attention | 状態遷移 |

外部adapterは事実を観測するだけで、その事実から直接statusを書き換えない。すべてのIssue状態変更はlifecycle decisionを経由する。

## 4. 永続aggregate

### 4.1 Repository aggregate

repositoryの正本は概念上、次の要素だけを持つ。

```text
RepositoryState
├── identity
├── mode: running | stopped | blocked
├── lifecycle_api_version
├── active_execution: none | (issue, run_id, generation)
├── issues: IssueEnvelope[]
├── pending_effects
└── state_revision
```

`active_execution`は並列resource leaseではない。process終了、再起動、遅延結果の前後で「どの実行だけが現在状態を変更できるか」を示す単一のfencing identityである。

repository rootで検証する不変条件は、identity、version、revision、transaction整合性、active executionが0件または1件であることに限定する。個別Issueの内容を理由にroot全体を読めなくする設計を避ける。

### 4.2 Issue envelope

各Issueは次のどちらかとして保存する。

```text
IssueEnvelope = Managed(IssueAggregate) | Quarantined(QuarantineRecord)
```

`Managed`は通常のlifecycle対象である。`Quarantined`は、そのIssueだけを安全に解釈または継続できない場合の証拠保存形式であり、実行枠を消費しない。

Issue aggregateは少なくとも次を一体として保持する。

- Issue identityとauthor verification evidence
- 公開lifecycle状態とそのAPI version
- run IDとgeneration
- workspace、branch、base commit
- continuation checkpointとsuspension
- pending requestとanswer
- publication identityと結果
- failure、reconciliation、監査evidence

scenario別のresume status、sync flag、resource park、recovery substateを追加しない。中断理由の違いはgenericな`Suspension`のreasonとevidenceで表現する。

## 5. 単一の状態遷移境界

Issueの変更は概念上、次の一つの契約を通す。

```text
Decide(current aggregate, intent, observations)
  -> next aggregate + durable effects + audit evidence
```

- `intent`は開始、worker結果、回答、取消、外部事実の照合など、利用者またはapplicationの意図を表す。
- `observations`はGitHub、process、worktree、clockからadapterが取得した事実である。
- lifecycleは副作用を実行せず、決定的なdecisionだけを返す。
- state storeは現在のrevision、run ID、generationを再検証し、next aggregate、effect intent、eventをatomicにcommitする。
- effect実行はcommit後に行い、同じeffect identityで再試行する。
- effect結果も同じlifecycle境界からaggregateへ反映する。

application、CLI、reconciler、publisherが独自にstatus、request、execution identityを組み合わせて更新することを禁止する。新しい障害シナリオは新しい復旧状態ではなく、新しいobservationまたはreasonとして扱う。

decisionがIssue不変条件を満たさない場合、無効なnext aggregateは保存しない。最後の有効なIssue aggregateと拒否理由から`Quarantined` envelopeを作り、対象Issueがactiveなら同じtransactionで実行枠を解放する。root invariantまたはtransaction chain自体が成立しない場合だけrepositoryを`blocked`にする。

## 6. Queueと単一実行

queue処理は次の順序に固定する。

1. GitHubから候補とactor事実を観測する
2. author policyで信頼できる候補だけを残す
3. snapshot上で処理可能な候補を決定的にsortする
4. repositoryの実行枠が空であることを確認する
5. 選択したIssueのactor事実を再取得して信頼性を再検証する
6. 新しいrun IDとgenerationを含むactive executionをatomicに保存する
7. workerを起動する
8. 結果をlifecycleへ渡し、実行枠を解放または次のworker実行へ更新する
9. queue評価へ戻る

同時workerはrepositoryごとに最大1つとする。Issue間resource、path claim、dependency metadata、worker pool、slot assignmentは現行設計に含めない。

`needs_input`、`retry_wait`、`awaiting_checks`、`awaiting_merge`、terminal、quarantineはworkerを実行していないため実行枠を消費しない。これらのIssueはdurableに追跡しつつ、supervisorは別Issueを選択する。publisherとGitHub mutationはrepository単位で直列化するが、外部checkやmergeの待機によってworker枠を占有しない。

同一repositoryを処理するsupervisorは1つ、hostも1つとする。worker並列化とmulti-hostは現行設計の拡張点ではなく対象外であり、必要になった時点で別要件とADRを作成する。[ADR-0005](adr/0005-single-execution-boundary.md)を正本とする。

## 7. Issue作成者の信頼境界

Issue本文はuntrusted inputである。ready labelだけではworker起動を許可しない。

author policyはGitHub observerが取得した次の事実だけを入力にする。

- actorのexact loginとaccount種別
- repository ownerか
- 現在のrepository permission
- repository設定の明示allowlistに一致するか

既定ではownerと`write`以上のcollaboratorを信頼し、botまたはGitHub Appはexact allowlistを必要とする。Issue本文、コメント、labelによる自己申告は信頼根拠にしない。

候補取得時の検証結果はqueueの効率化に利用できるが、worker開始直前に必ず再検証する。検証不能または不一致は対象Issueを非着手として記録し、後続候補の選択を続ける。author検証APIの一時障害はその候補の一時的な不適格であり、既に検証できた別Issueを止めない。

## 8. Continuationとreconciliation

中断したIssueは共通の`Suspension`として扱う。

```text
Suspension
├── reason
├── checkpoint
├── evidence
├── allowed_resolutions
└── pending_request (optional)
```

checkpointは同じ作業を継続するためのworkspace、branch、base、session、run、generation、publication identityを保持する。回答、retry、resume、cancelは保存済みcheckpointと現在の外部事実を再検証してからlifecycle intentとして適用する。

reconcilerは「workerが消えた」「PRが既に存在する」「merge済み」「label同期が不足」などの事実を正規化し、通常と同じ`Decide`へ渡す。事実ごとの専用status、専用command、専用state mutationを作らない。event logの文言や件数からauthorityを合成しない。

## 9. 障害境界

| 範囲 | 例 | 設計上の処理 |
| --- | --- | --- |
| Issue-local | worker失敗、provenance不足、PR不一致、個別aggregate違反、author不一致 | 対象Issueをretry、suspend、terminalまたはquarantineへ移し、実行枠を解放してqueueを継続 |
| Transient shared dependency | GitHub 5xx、rate limit、短時間のnetwork断 | durable deadlineまでbackoffし、repository stateを維持 |
| Repository-wide | root snapshotを解釈不能、transaction chain破損、global config不正、全Issueに共通するauthority消失 | 新規effectを止めてrepositoryを`blocked`にする |

Issue番号、run ID、generation、Issue lifecycle intentを持つerrorは、分類不能でもIssue-local境界で処理する。未分類errorを自動的にrepository-wideへ昇格させない。

GitHubへの状態同期が失敗しても、localのIssue終端化と実行枠解放を妨げない。同期はdurable effectとして再試行し、別Issueのworker起動を継続する。

## 10. Issue lifecycle APIと互換性

公開lifecycle contractは、状態ごとの意味、許可遷移、terminal判定、実行枠消費、自動継続可否、人間操作要否を一つのversioned sourceとして定義する。CLI JSON、event、GitHub表示、migration、validatorはこのcontractから同じ意味へprojectする。

内部storage schemaとIssue lifecycle APIを分離する。

- storage変更を決定的に移行でき、公開上の意味が変わらない場合はAPI majorを維持する。
- 同一majorの旧minor fixtureは新minorで読み、同じ意味で継続できなければならない。
- 公開状態の削除・改名・意味変更、terminal判定、実行枠消費、許可遷移の非互換変更ではAPI majorを更新する。
- major更新でも、根拠が十分なIssueは決定的に移行する。
- 移行不能なIssueは`Quarantined`へ変換し、他Issueを継続する。

migration decoderは旧形式を読む境界に限定する。旧scenario別runtime経路を互換性のために残さず、変換後は現行aggregateと共通lifecycleだけを使用する。

## 11. Package境界

```text
internal/
├── domain/
│   ├── issue/       # aggregate、lifecycle contract、decision、invariant
│   └── queue/       # author policy、eligibility、deterministic ordering
├── application/
│   ├── supervisor/  # queue loop、単一実行、Issue-local failure boundary
│   ├── reconcile/   # 外部事実の収集とreconcile intent
│   └── publication/ # durable effectの直列実行
├── adapter/
│   ├── state/
│   ├── github/
│   ├── worker/
│   └── publish/
└── platform/        # config、filesystem、process、launchd
```

依存方向は`domain`を内側とし、domainはfilesystem、network、process、clock、永続storeを直接参照しない。applicationはdomain decisionを呼び出してeffectを調停し、adapterは外部事実とeffect結果を変換する。

`domain/admission`を現行runtimeの判断経路に置かない。既存resource admission実装を将来再利用する前提も設けない。並列化要件が新たに承認された場合にだけ、現在の単一実行モデルとの置換または独立bounded contextとして再設計する。

## 12. 設計上の不変条件

- repositoryのactive executionは常に0件または1件である。
- active execution以外のrunまたはgenerationは状態と外部公開を変更できない。
- Issue状態は共通lifecycle decision以外から変更しない。
- Issue-local障害はrepository modeを`blocked`にしない。
- workerを実行していないIssueは実行枠を消費しない。
- authorを検証できないIssueをworkerへ渡さない。
- untrusted Issueが後続のtrusted Issueを妨げない。
- 未回答request、workspace、publication identity、quarantine evidenceを暗黙に削除しない。
- eventを正本または実行authorityにしない。
- workerにqueue選択、commit、push、PR作成を許可しない。
- 同一publication intentの再試行で別branchまたは別PRを作らない。
- 同一API majorのminor updateでIssue状態の意味を変えない。

## 13. 実装との関係

本書は目標設計を表し、現行コードがすべて適合済みであることを意味しない。適合状況と既知の差分は[実装状況](implementation.md)に記録する。要件は[要件定義](requirements.md)、外部観測可能な振る舞いは[仕様書](specification.md)を正本とする。
