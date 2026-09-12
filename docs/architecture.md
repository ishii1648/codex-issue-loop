# codex-issue-loop アーキテクチャ概要

## 1. 目的と設計原則

`codex-issue-loop`は、信頼できるGitHub Issueを決定的な順序で選び、1件ずつcoding workerへ渡し、進行中Issueの順序を保ち、個別の停止・隔離・回答待ちでは後続Issueの処理を継続するsupervisorである。

設計上の優先順位は次のとおりとする。

1. repository全体の正本と実行authorityを壊さない
2. 個別Issueの障害をそのIssueへ閉じ込め、queueの進行と正式復旧を維持する
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

GitHub外形監視も別bounded contextである。独立processがGitHubのIssue、label、event時刻だけからqueue可用性を判定し、supervisorのstate、metrics、logsを入力にしない。詳細は[`monitor/docs/architecture.md`](../monitor/docs/architecture.md)を正本とする。

## 3. 責務

### 3.1 Durable Issue lifecycle境界

現行実装では、状態語彙、遷移decision、不変条件を`internal/domain/issue`が所有し、`internal/adapter/state/issue_transition.go`がdurable snapshotへのstatus commit境界を担う。applicationはdomain decisionの呼び出しとeffectの調停に限定し、この規律のrepository作業ルールは[ルートの`AGENTS.md`](../AGENTS.md#durable-issue-lifecycle)、公開状態の互換性は[§10](#10-issue-lifecycle-apiと互換性)を参照する。

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
├── version
├── active_execution: none | (issue, run_id, generation)
├── issues: IssueEnvelope[]
├── pending_effects
└── state_revision
```

`active_execution`は並列resource leaseではない。process終了、再起動、遅延結果の前後で「どの実行だけが現在状態を変更できるか」を示す単一のfencing identityである。worker起動時はgeneration付きの`launching`がこれを所有し、started callbackが同じgenerationとPID/PGIDを一つのtransactionで保存したときだけ`running`へ遷移する。

repository rootで検証する不変条件は、identity、version、revision、transaction整合性、active executionが0件または1件であることに限定する。個別Issueの内容を理由にroot全体を読めなくする設計を避ける。

### 4.2 Issue envelope

各Issueは次のどちらかとして保存する。

```text
IssueEnvelope = Managed(IssueAggregate) | Quarantined(QuarantineRecord)
```

`Managed`は通常のlifecycle対象である。`Quarantined`は、そのIssueだけを安全に解釈または継続できない場合の証拠保存形式であり、実行枠を消費しない。

Issue aggregateは少なくとも次を一体として保持する。

- Issue identityとauthor verification evidence
- 公開lifecycle状態（互換性はsnapshot全体のversionで識別）
- run IDとgeneration
- workspace、branch、base commit
- continuation checkpointとsuspension
- pending requestとanswer
- publication identityと結果
- failure、reconciliation、監査evidence

scenario別のresume status、sync flag、resource park、recovery substateを追加しない。中断理由の違いはgenericな`Suspension`のreasonとevidenceで表現する。

GitHub コメントは mailbox の transport として扱い、状態の正本にはしない。GitHub adapter が versioned 質問/ack marker を同期し、supervisor が authoritative comment 一覧と actor の現在権限を取得する。CLI と GitHub は共通の `state.RecordAnswer` で回答を保存し、GitHub の observation は任意の `Request.answer_provenance` と同じ transaction に残す。旧 request に provenance は必須としない。回答受領では Issue status・実行枠・checkpoint を変更せず、既存 `PrepareAnsweredRequests` と worker 起動境界が再開を判断する。ack の再同期やコメント編集・削除は保存済み回答を取り消さない。

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
4. repositoryの実行枠が空で、順序待ちを必要とする進行中Issueがないことを確認する
5. 選択したIssueのactor事実を再取得して信頼性を再検証する
6. 新しいrun IDとgenerationを含むactive executionをatomicに保存する
7. workerを起動する
8. 結果をlifecycleへ渡し、実行枠を解放または次のworker実行へ更新する
9. queue評価へ戻る

同時workerはrepositoryごとに最大1つとする。Issue間resource、path claim、dependency metadata、worker pool、slot assignmentは現行設計に含めない。

`needs_input`、`retry_wait`、`awaiting_checks`、`awaiting_merge`、terminal、quarantineはworkerを実行していないため実行枠を消費しない。worker枠の解放と新規受付は別であり、`claiming`、`claimed`、`launching`、`running`、`resume_pending`、`retry_wait`、`awaiting_checks`、`awaiting_merge`、`resolving_conflict`では後続の未受付Issueはreadyのまま待機する。`needs_input`、`failed`、`blocked`、quarantineは対象Issueだけを保留し、後続の新規受付を妨げない。publisherとGitHub mutationはrepository単位で直列化するが、外部checkやmergeの待機によってworker枠を占有しない。

PRがある場合、worker終了・CI成功・auto-merge設定ではなく、マージの観測と内部完了の確定で順序待ちを解除する。PR不要の正当な完了経路も維持する。次の新規worktreeは既存のfetch処理により最新のbase branchから作成する。待機理由はstatusのIssue状態・エラーとsupervisor messageで確認できる。

過去の停止・隔離・回答待ちIssueにも同じ受付判定を適用し、一括キャンセルを要求しない。保留Issueの回答・復旧後も別Issueの実行権を奪わず、既存schedulerで再開する。既存workerを強制停止せず、最大1 workerのfencingを維持して収束させる。受付待ちの判断は毎cycleのdurable snapshotから導出し、labelや追加の永続状態に依存しない。

同一repositoryを処理するsupervisorは1つ、hostも1つとする。worker並列化とmulti-hostは現行設計の拡張点ではなく対象外であり、必要になった時点で別要件とADRを作成する。[ADR-0005](adr/0005-single-execution-boundary.md)を正本とする。

## 7. Issue作成者の信頼境界

Issue本文はuntrusted inputである。ready labelだけではworker起動を許可しない。

author policyはGitHub observerが取得した次の事実だけを入力にする。

- actorのexact loginとaccount種別
- repository ownerか
- 現在のrepository permission
- repository設定の明示allowlistに一致するか

既定ではownerと`write`以上のcollaboratorを信頼し、botまたはGitHub Appはexact allowlistを必要とする。Issue本文、コメント、labelによる自己申告は信頼根拠にしない。

候補取得時の検証結果はqueueの効率化に利用できるが、worker開始直前に必ず再検証する。検証不能または不一致は対象Issueを非着手として記録する。受付済みIssueの停止はそのIssueだけで正式復旧またはキャンセルを待ち、後続候補の選択を妨げない。author検証APIの一時障害はその候補の一時的な不適格であり、既に検証できた別Issueを止めない。

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

管理対象Issueのlifecycle authorityはcanonical snapshotに置く。worker結果、supervisorが検証したPR・process・worktreeの事実、正式なoperator commandがdomain decisionを経て内部状態を変更する。GitHubの管理labelとIssueの開閉状態はその投影であり、手動変更から内部status、実行権、retryを決めない。未管理Issueのready/exclusionによる受付と、正式なrequestへの回答は入力として扱う。

reconciliationの責務は二つに分ける。実行復旧はprocess・worktree・PR identityを検証してdomain decisionを適用する。表示同期は現在の内部statusから期待する管理label・開閉状態を導出し、GitHubの差分だけを書き戻す。`completed`、`canceled` Issueは定期巡回の対象外とし、状態変更時・未完了effectの再試行・対象Webhookで表示同期する。quarantined Issueは定期的な表示同期の対象とする。quarantineからworkerを再開せず、GitHubにはblocked相当を表示する。

```text
worker / supervisor / operator command
                 ↓ 検証・transaction
             内部状態（正本）
                 ↓ 起動時・状態変更後・定期同期
          GitHub の管理label・開閉状態
```

`pending_effects`はcommentなどの未完了副作用を再試行するための記録であり、表示同期の必要性を決める唯一の根拠にはしない。成功にはGitHubの再取得による期待状態との一致を要求する。marker commentだけで未完了のcloseを完了扱いにしない。同期中に内部状態が変わった場合は古い結果を確定せず、最新状態で再同期する。ネットワーク待機中はstate transaction lockを保持しない。

worker起動前には、GitHubへの投影を同期・再取得して確認した後、内部のIssue/run/generationと`active_execution`を再検証する。同期失敗では起動せず、内部状態をGitHubへ合わせてcancel・completeにはしない。schedulerの既存lifecycle gateで同一process内の同期と状態変更を直列化し、実行中workerの表示同期は新しい実行jobや実行枠を作らない。手動close・除外labelによる停止はサポートせず、正式なoperator commandを使う。PR mergeはラベルと異なる外部事実なので、保存済みpublication identityを検証してから完了とし、予期しないmergeを自動revertしない。

## 9. 障害境界

| 範囲 | 例 | 設計上の処理 |
| --- | --- | --- |
| Issue-local | worker失敗、provenance不足、PR不一致、個別aggregate違反、author不一致 | 対象Issueをretry、suspend、terminalまたはquarantineへ移し、再admissionを拒否して実行枠を解放し、後続候補からqueueを継続 |
| Transient shared dependency | GitHub 5xx、rate limit、短時間のnetwork断 | durable deadlineまでbackoffし、repository stateを維持 |
| Repository-wide | root snapshotを解釈不能、transaction chain破損、global config不正、全Issueに共通するauthority消失 | 新規effectを止めてrepositoryを`blocked`にする |

Issue番号、run ID、generation、Issue lifecycle intentを持つerrorは、分類不能でもIssue-local境界で処理する。未分類errorを自動的にrepository-wideへ昇格させない。

GitHubへの状態同期が失敗しても、localのIssue終端化と実行枠解放を妨げない。未完了effectと定期的な表示同期で再試行する。個別の停止・隔離・回答待ちは新規受付を妨げない。予算内の自動retryは順序を維持し、上限到達後の停止は対象Issueだけに閉じ込める。

## 10. Issue lifecycle APIと互換性

公開lifecycle contractは、状態ごとの意味、許可遷移、terminal判定、実行枠消費、自動継続可否、人間操作要否を一つのversioned sourceとして定義する。CLI JSON、event、GitHub表示、migration、validatorはこのcontractから同じ意味へprojectする。

snapshot の永続化契約は単一整数 `version=6` で識別する。構造・JSON名・必須条件・相互不変条件は `internal/domain/snapshot`、フィールドの意味と実行要件は同層と `internal/domain/statecontract`、許可遷移は既存の `internal/domain/issue` が正本である。`state_revision` は transaction の更新番号であり、互換性識別には使わない。snapshot は直接編集用の公開 API ではなく、更新は正式操作と Store transaction を通す。

契約変更の検出対象は `internal/domain/snapshot/**`、`internal/domain/statecontract/**`、`internal/domain/issue/**`、埋め込む型の依存先である `internal/domain/publication/**`、`internal/domain/queue/**`。型だけでなく validator の受理条件、JSON decode、正規化、遷移 decision も対象とする。#536 はこのパス集合を基準に CI を実装する。

同じ version を維持できるのは、新旧 reader が既存の正常 snapshot を同じ意味で受理できる変更だけである。必須条件・状態の意味・許可遷移・validator の受理集合の変更、旧 reader が拒否する optional field の追加でも version を更新する。非互換変更は version 更新と明示的 migration を対にする。CLI 出力自身の schema、config/registry、monitor、delivery protocol の version はこの識別子へ統合しない。release metadata の旧名 `state_schema_current` / `semantic_contract_current` は同じ単一 version を表示し、別々に更新しない。lifecycle API metadata は旧契約の情報であり snapshot reader の互換判定には使用しない。

Store の load・保存直前の transaction・診断は domain の version/aggregate 判定を通す。`issue_transition.go` は domain decision の commit fence を確認して status を変更する既存境界を維持する。ファイル I/O、lock、atomic 保存、redaction、外部の process/Git/GitHub 観測は adapter/application に残す。観測結果は値として domain に渡し、domain から外部へ依存しない。

旧入力 `(version, semantic_contract_version, issue_lifecycle_api_version)=(5,4,2.0/2.1)` は v6 とは別の移行元である。v6 reader は旧版、未知版、v6 に旧2項目が残る入力を拒否する。旧 reader は version=6 を拒否するため、旧 binary で直接開かない。既存 v4→v5 migration 専用の `ValidateLegacy` は domain の共通不変条件を検証するが、runtime load/commit の許可には使わない。v6 への起動前 migration は #537 で統合し、移行不能状態があれば原本を変更せず移行全体を中止する。配布保留・解除条件は [release gates](release-gates.md)、停止下の backup/binary を対にした rollback は [migration runbook](migration.md) を参照する。

## 11. Package境界

```text
internal/
├── domain/
│   ├── snapshot/    # 永続構造、version、validator、transaction契約
│   ├── statecontract/ # フィールド意味・実行要件
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
- `launching`はworker processを持たない予約状態として一時的に実行枠を所有できるが、失敗またはreconciliationで実行枠を解放する。
- `running`は同じactive execution generationに属するPID/PGIDを必ず持つ。
- authorを検証できないIssueをworkerへ渡さない。
- untrusted Issueが後続のtrusted Issueを妨げない。
- 未回答request、workspace、publication identity、quarantine evidenceを暗黙に削除しない。
- eventを正本または実行authorityにしない。
- workerにqueue選択、commit、push、PR作成を許可しない。
- 同一publication intentの再試行で別branchまたは別PRを作らない。
- PR待機中にbase branchが進んでもfast-forwardなら同一publication intentを継続し、履歴分岐だけを拒否する。
- 同一API majorのminor updateでIssue状態の意味を変えない。

## 13. 実装との関係

本書は目標設計を表し、現行コードがすべて適合済みであることを意味しない。適合状況と既知の差分は[実装状況](implementation.md)に記録する。要件は[要件定義](requirements.md)、外部観測可能な振る舞いは[仕様書](specification.md)を正本とする。
