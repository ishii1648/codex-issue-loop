# Resource claim・依存metadata・admission契約

この文書は、同一repository内で複数Issueを並列実行するschedulerが使用するresource claim、Issue依存関係、resource leaseの正本仕様である。schema v3ではdurable lease、resource definition、複数worker admission、publish前の実diff監査を扱う。definition未設定のqueueは`concurrency: 1`と`repo:*`へ安全側に縮退する。

単一host並列化と複数host冗長化の責務分離は[ADR-0002](adr/0002-concurrency-and-multi-host.md)を正本とする。本書のleaseは、特記しない限り1つのsupervisorがlocal stateに保持する単一host用の論理leaseを指す。

## 1. 正本と信頼境界

admissionに使う正本は次のとおりである。

| 情報 | 正本 | 用途 |
| --- | --- | --- |
| resource名とpath glob | 対象repositoryの`.agent-loop.yaml` | claim名の検証、変更pathの監査 |
| Issueのresource claim | GitHub Issue labelの`area:` prefix | Issueが排他的に使用するresource集合 |
| Issueの依存関係 | GitHub Issue本文内の構造化metadata block | 着手前に完了している必要があるIssue集合 |
| active lease、run、PR状態 | supervisorの永続local state | admission時の競合判定と再起動復旧 |
| Issue/PRの共有状態 | 1回のreconciliationで取得したGitHub snapshot | ready、closed、merged等の判定 |

Issue本文の自然言語、コメント、title、assignee、milestoneからresourceや依存関係を推測しない。Codex workerにも推測させない。path globはlabelの代替入力ではなく、producerがlabelを選ぶためのtaxonomyと、worker結果を公開する前の監査に使う。

## 2. Resource claim label

### 2.1 Prefixとresource名

resource claim labelの固定prefixは`area:`とする。resource名はconfigの`resources.definitions[].name`を参照する。たとえば`area:config`はresource `config`のexclusive claimである。

labelは次の順で正規化する。

1. label全体の前後にあるASCII spaceとtabを除去する。
2. `area:`との比較だけASCIIの大文字小文字を区別しない。
3. suffixをASCII lowercaseへ変換する。
4. suffixが正規表現`^[a-z0-9][a-z0-9-]{0,62}$`に一致することを確認する。
5. 正規化後の重複を除去し、resource名のbyte昇順で保存する。

`AREA:Config`は`config`へ正規化される。`area: config`、`area:config/cli`、`area:`、非ASCII suffixは不正なclaim labelである。`area:`以外のlabelはclaim判定に影響しない。

`repo:*`はscheduler内部だけで使う予約resourceである。GitHub label `area:repo`やconfig definitionでは表現せず、後述の安全側への縮退で生成する。`repo:*`はすべてのresourceと競合する。

### 2.2 複数claimと競合

Issueには複数のclaim labelを付けられる。たとえば`area:config`と`area:docs`を持つIssueは両resourceのleaseを同時に取得し、片方でも取得できなければadmitされない。通常resource同士は、正規化後の名前が一致する場合だけ競合する。

競合関数を次のように固定する。

```text
conflicts(A, B) =
  true  if repo:* is in A or repo:* is in B
  true  if intersection(A, B) is not empty
  false otherwise
```

## 3. Repository config

並列化を導入するschema v3では、`.agent-loop.yaml`に次の形式を追加する。配列順は表示用であり、admission優先度には使わない。

```yaml
version: 4

queue:
  concurrency: 3

resources:
  definitions:
    - name: config
      paths:
        - ".agent-loop.yaml"
        - ".agent-loop.example.yaml"
        - "internal/platform/config/**"
        - "docs/resource-admission.md"
    - name: docs
      paths:
        - "README.md"
        - "docs/**"
```

configのvalidation規則は次のとおりである。

- Issue本文metadataの対応versionは実装が`1`に固定する。
- `definitions`は1件以上必要である。
- `name`はlabel suffixと同じ正規化・文字規則を適用し、正規化後の重複を拒否する。予約名`repo`は使用できない。
- 各definitionの`paths`は1件以上必要で、空文字と正規化後の重複を拒否する。
- pathはrepository root相対のUTF-8文字列とし、separatorは`/`だけを使う。先頭`/`、末尾`/`、空segment、`.`、`..`、backslash、NULを拒否する。
- glob tokenはsegment内の`*`と`?`、segment全体の`**`だけを許可する。`*`と`?`は`/`を越えず、`**`は0個以上のsegmentに一致する。character class、brace展開、negation、symlink解決、filesystem依存のcase foldingは行わない。
- pathとglobはUnicode code pointを変更せず、case-sensitiveに比較する。実在fileだけでなく、新規fileにも同じ規則を適用する。
- 未知keyと未知versionはsupervisor開始前にconfig errorとして拒否する。

resource definitionを有効化したschema v3の`bootstrap-labels`は各definitionに対応する`area:<name>`も不足分だけ作成する。definition未導入のconcurrency 1 configではresource labelを作成しない。

1つの変更pathが複数definitionに一致する場合、そのpathは一致したresourceをすべて必要とする。どのdefinitionにも一致しないpathは`repo:*`を必要とする。publisherはworker結果の変更pathを正規化したclaim集合で覆えない場合、公開せず、固定reason code `resource_claim_mismatch`で`needs_input`へ遷移させる。この監査は過少claimを検出する最後の防壁であり、実行中のlease集合を後から拡張しない。

## 4. Issue本文の`depends_on` metadata

### 4.1 形式

Issue本文には、次のHTML comment blockを最大1個だけ置く。schedulerは本文のCRLFをLFへ変換した後、Markdownとして解釈せず行単位で走査する。開始marker `<!-- agent-loop:metadata`と終了marker `-->`は、indentや末尾spaceのない完全一致の単独行とする。開始marker直後から最初の終了marker直前までをYAML 1.2としてdecodeする。code fence内に同じmarkerを書いてもmetadataとして検出されるため、説明用の例はIssue本文に含めない。

```markdown
<!-- agent-loop:metadata
version: 1
depends_on:
  - 52
  - 60
-->
```

依存がないIssueも、並列実行を許可するにはblockと空配列を明示する。

```markdown
<!-- agent-loop:metadata
version: 1
depends_on: []
-->
```

blockのvalidation規則は次のとおりである。

- top-levelはmappingで、keyは`version`と`depends_on`だけを許可し、両方を必須とする。
- `version`はquoteしていないinteger `1`だけを許可する。
- `depends_on`は0件以上のinteger配列とする。string、mapping、null、YAML alias、tagは許可しない。
- Issue番号は`1`以上で、現在のIssue番号自身を含めず、重複してはならない。同一repository内のIssue番号だけを表し、`owner/repo#123`等のcross-repository表記は許可しない。
- blockが複数ある、終了markerがない、YAMLとしてdecodeできない、keyが重複する、未知keyがある場合はすべて不正metadataとする。
- 配列順は意味を持たない。検証後はIssue番号の昇順で保存する。

依存Issueは、admission snapshotで次のいずれかを満たす場合に完了済みとする。

- local stateが対応Issueを`completed`としており、対応PRのmerge確認を永続化済みである。
- GitHub snapshotでIssueがclosedであり、そのIssueについてlocal stateに既知のopenまたはunmerged PRがない。

参照先が存在しない、取得できない、open、処理中、または既知のPRが未mergeなら未完了である。未完了はmetadata不正ではなく、leaseを取得せずキューで待機する。valid metadata間の自己参照以外のcycleは、cycle内のIssueを`dependency_cycle`理由で待機させ、LLMを呼ばない。producerが依存を修正するまで自動的に辺を無視しない。

### 4.2 不正例

次はいずれも不正である。

`version`がなく、Issue番号がstringである。

```markdown
<!-- agent-loop:metadata
depends_on: ["52"]
-->
```

cross-repository参照と未知keyを含む。

```markdown
<!-- agent-loop:metadata
version: 1
depends_on: [other/repo#52]
after: release
-->
```

Issue #61自身への依存と重複を含む。

```markdown
<!-- agent-loop:metadata
version: 1
depends_on: [61, 52, 52]
-->
```

## 5. 安全側への縮退

並列実行へのopt-inは、validなmetadata blockと、1件以上の既知resource claim labelの両方が揃った場合だけ成立する。次のいずれかなら、effective claim全体を`[repo:*]`へ置き換える。

- metadata blockがない。
- metadata blockが不正または未対応versionである。
- `area:` labelが1件もない、または不正な`area:` labelを含む。
- 正規化したresource名がconfigに存在しない。
- claim labelの一部だけが既知で、ほかに未知resourceがある。

縮退時は部分的に解釈できたresource claimを利用しない。metadata blockがvalidなら、resource claimが縮退しても正規化済み`depends_on`は維持する。metadata blockがない、または不正な場合だけeffective dependency集合を空として扱う。reason codeと元のvalidation errorはsnapshot/eventへ保存する。これにより、従来Issueや壊れたIssueは停止せず処理できるが、必ずrepository内で単独実行になり、validな依存関係はresource入力の不備だけで失われない。warningの文言、labelの取得順、YAML decoderのmap順はadmission結果に影響させない。

縮退reason codeは次の優先順位で最初に一致した1件を保存する。同種のerrorが複数ある場合、正規化できるresource名、元labelのUTF-8 byte列、validation fieldの順でdetailをsortする。

1. `metadata_missing`
2. `metadata_invalid`
3. `resource_claim_missing`
4. `resource_claim_invalid`
5. `resource_claim_unknown`

| 入力 | effective claim | effective dependencies |
| --- | --- | --- |
| valid block + `area:config` | `[config]` | blockの正規化済み配列 |
| valid block + `area:unknown` | `[repo:*]` | blockの正規化済み配列 |
| blockなし + `area:config` | `[repo:*]` | `[]` |
| valid block + claim labelなし | `[repo:*]` | blockの正規化済み配列 |
| 壊れたblock + `area:config` | `[repo:*]` | `[]` |

metadata未指定Issueが2件あっても、片方の`repo:*` leaseが解放されるまで他方は開始しない。既知resourceを持つIssueも`repo:*`と同時実行しない。

## 6. Producerの責務とready境界

Issue producerはready labelを最後に付ける。更新時もいったんready labelを外し、次の順で共有状態を完成させる。

1. 背景、変更範囲、完了条件を本文へ記載する。
2. `depends_on` metadataを追加し、依存がなければ`[]`を明示する。
3. 変更可能性のある全pathをconfig taxonomyへ照合し、必要な`area:<name>` labelをすべて付ける。
4. 依存Issue番号、metadata version、resource名が現在のrepository snapshotで有効であることを検証する。
5. 最後にready labelを付ける。

GitHubは本文編集とlabel更新を一つのtransactionにはできないため、supervisorはreadyを観測した時点の完全なsnapshotだけを評価する。producerの更新途中を観測しても、欠落・不正入力は`repo:*`へ縮退する。

supervisorは候補取得、metadata parse/validation、dependency graph、resource競合、queue sort、claim、待機、PR/CI/merge監視をGoコードだけで行う。admissionの可否、resourceの補完、dependencyの推測、待機理由の生成を目的としてCodexやその他のLLMを呼ばない。Codex workerは、admissionが成功し、`claiming`とleaseを同じ永続transactionで確定した後にだけ起動できる。

active Issueの本文、claim label、ready labelが変更されても、そのrunのeffective claimとdependency集合は変更しない。変更は次回の新しいclaimでだけ有効になる。active leaseがある間は`resources` definitionのhot reloadを拒否し、再起動前のconfig hashと永続leaseを照合する。

## 7. 決定論的admission

### 7.1 入力snapshot

1回のadmission cycleは、次の値を固定したimmutable snapshotを入力にする。

- config version、config content digest、queue order、concurrency、正規化済みresource definitions
- pagination完了後の全候補Issueについて、number、created time、state、本文のbyte列、label名の集合
- 依存先Issueのstate
- local state revision、active Issue/run、effective claim、PR URLとmerge/close状態

wall clock、GitHub APIの返却順、filesystem走査順、Go map iteration順、worker完了予測は入力にしない。同一snapshotとは、上記の意味値がすべて同じことを指す。

### 7.2 Algorithm

supervisorは1つだけとし、次の処理を直列に行う。

1. ready、exclude、open等の既存eligible条件で候補集合を作る。
2. 各Issueのmetadataとlabelを正規化し、effective dependenciesとeffective claimを確定する。
3. valid metadataからdependency graphを作り、未完了依存とcycleを持つIssueを待機候補へ移す。
4. [Queue ordering](queue-ordering.md)のstrategyと最終tie-breakであるIssue番号により候補を全順序へsortする。
5. sort順に候補を走査する。active leaseおよび同cycleですでに選択したIssueと競合せず、空slotがあれば選択する。競合するIssueはskipし、後続の非競合Issueを検討する。
6. 選択順に、local state transaction内で全resource lease取得と`claiming`遷移を一緒に永続化する。revisionがsnapshotから変わっていればcycle全体を読み直す。
7. transaction成功後にだけGitHub running label、worktree、worker起動へ進む。

正規化済み集合・依存配列は昇順、候補はqueueの全順序、選択結果は選択順で永続化する。同じsnapshotとscheduler versionからは、選択Issue番号、effective claim、待機reason codeが同じになる。

## 8. Resource lease lifecycle

単一host leaseにはwall-clock TTLを設けない。supervisor停止、process crash、Mac再起動、worker slot解放だけでは期限切れにならない。再起動時は新規admissionより先に永続leaseとGitHub/PRをreconcileする。

lease ownerは`(Issue number, run_id, generation)`で識別する。`generation`はIssueごとに1から単調増加し、release後もIssue stateの`lease_generation`へ保持する。同じIssueの新runが予約すると次generationになり、release/expandはIssue numberを含むowner tupleの完全一致を必須とする。したがって、旧run ID、旧generation、別Issue向けownerのいずれも現在のleaseを変更できない。

| Issue状態・event | Lease | 規則 |
| --- | --- | --- |
| ready/queued、dependency待ち | なし | admission前はresourceを予約しない |
| `claiming` | 取得 | `claiming`遷移と全claimを1 transactionで永続化する |
| `claimed`、`running` | 保持 | worker終了やslot数とは独立して保持する |
| `retry_wait`、`resume_pending` | 保持 | backoff中、continuation間も次Issueへ譲らない |
| 通常workerの`needs_input` | park | request/run/owner provenanceとcontinuationを保持し、回答までactive admissionから外す |
| publication監査・PR conflictの`needs_input` | 保持 | manual/security/conflict経路には自動parkを適用しない |
| 回答済み`answer_claim_waiting` | park | 回答を保存し、空slotとresource解放後に新generationで再取得する |
| typed worker environment `blocked` | park | PID/PGID不在を確認し、active leaseを`resource_park.original_lease`へ移してadmission対象から外す |
| park済み`resume-blocked` | 新generationで再取得 | 保存claimと全active lease、worker slotを同じstate transactionで再検証する |
| publish中、draft PR、`awaiting_checks` | 保持 | commit/push/CI待ちをlease内に含める |
| open PR、`awaiting_merge` | 保持 | 人手merge待ちでも同resourceの次Issueを開始しない |
| PR merge確認 | 解放準備 | merge結果と`completed`を永続化するtransactionで解放する |
| retry上限到達、`failed`、`blocked` | 条件付き解放 | worker/publisher停止済みでopen PRがなければterminal遷移と同時に解放できる |
| supervisor stop/crash | 保持 | local stateから自動削除・時刻expiryしない |

open PRがある限り、Issue labelがdone/failedへ変わったりreadyが外れたりしてもleaseを解放しない。PRがmergeされずcloseされた場合は、worker/publisherが停止し、publication intentがなく、Issueを明示的にabandoned/terminalとして永続化したことをreconcileで確認してから解放する。PR closeをlease解放要求として単独では扱わない。

claim途中のGitHub API失敗ではleaseを保持して同じrun IDでreconcileする。local transaction自体がcommitされていなければleaseは存在せず、workerも起動してはならない。過少claimをpublish前監査で検出した場合は、leaseをその場で追加せず、現在のleaseを保持してattention状態へ遷移する。

workerがtypedなenvironment block（`blocked_cause.origin=worker`、`kind=environment`、`resumable=true`）を返した場合、blockのdurable transactionはactive leaseをparkする。`resource_park`はpark ID、元owner generation、slot、declared/resolved/actual resources、original base SHA、reservation時刻を保持するが、admissionのactive lease集合には含めない。run、worktree、branch、dirty changes、session、answers、attempt/continuation、blocked causeは変更せず、GitHubは`blocked`のままとする。導入前から同期済みの同じtyped blockは、起動時reconciliationがsupervisor-owned `blocked` label、open PR不在、PID/PGID不在を確認できた場合だけ同じ形式へparkする。

通常workerの`needs_input`は、worker resultでPID/PGIDを消去した後、`input_requested` transaction内でrequest ID、run ID、park ID、元ownerを相互に保存してactive leaseをparkする。GitHubはneeds-inputのままとし、ready/runningを付与しない。`answer`はこのprovenanceと未回答状態を同じtransactionで再検証し、空slotと全active leaseに競合がなければgenerationを1回だけ増やして`resume_pending`にする。競合時は回答とanswer recordを保存して`answer_claim_waiting`にし、他Issueのleaseを変更しない。schedulerは競合解消後だけclaimを再取得し、spawn直前のworkspace provenance検証を通過するまでcontinuationを起動しない。

`resume-blocked --confirm-prerequisite-resolved`はpark IDと元claimを指定Issueだけに限定し、同一run/worktree/branch/session、GitHub Issue/PR/label、pending request不在、保存base SHAのHEAD祖先性、dirty/unpushed状態、空worker slot、全active leaseとの非競合を検証する。競合するIssueのleaseは奪わず、stateとGitHubを変更せずIssue番号付きで拒否する。成功時は`lease_generation`を1回だけ増やした新ownerを`resource_park.resume_owner`、resume event、active leaseへ同時保存する。GitHub同期中の`environment_resume_pending`もslot予約済みとして扱い、別claimとの二重slotを拒否する。再実行は同じresume IDとownerへ収束する。

manual/security block、PR conflict、failed、closed Issue、active worker、複数pending request、未知または改変されたpark provenanceにはneeds-input park/resumeを適用しない。parkやresumeの途中停止はstate transactionと冪等GitHub markerから回復し、state fileやsupervisor-owned labelの手編集を復旧手段にしない。後続Issueでbaseが進んでも元base SHAとdirty worktreeを保持し、publish時の通常base/conflict auditへ渡す。

## 9. 単一hostと複数hostの境界

本Issue群で実装対象とするのは、1 host、1 repository、1 supervisor内でworker slotだけを並列化するmodeである。local state transactionがresource leaseの排他を保証し、publisherとGitHub副作用はrepository単位で直列化する。

複数host排他は本Issue群の対象外である。local lease、GitHub label、Issue metadataは別hostとの相互排他を保証しない。複数host対応ではADR-0002に従い、GitHub外の線形化可能なcoordinator、epoch/fencing、期限付きdistributed lease、durable publication intent、fenced publication gatewayを別schema・別migrationで導入する。`queue.concurrency > 1`や本書のmetadataを有効にしてもdistributed modeにはならない。

## 10. Schema versionとmigration

この契約の初期versionを次のように固定する。

| 対象 | 現行 | 導入時 | 方針 |
| --- | --- | --- | --- |
| `.agent-loop.yaml` | v2 | v3 | `resources`と`queue.concurrency > 1`を追加。v2を暗黙変換しない |
| local state/event | v2 | v3 | Issueごとのeffective claim、dependency、config digest、leaseを追加 |
| Issue metadata block | なし | v1 | 実装が固定するmetadata version `1`と一致させる |

migrationは全loop停止、checksum付きbackup、read-only preview、明示`--apply`、doctor、1 repositoryずつの再開を必須とする。既存v2 config/stateはconcurrency 1のまま動作し、v3への自動migration、並列化の自動有効化、既存Issue本文/labelの自動書換えを行わない。

v2からv3へ移行する際、既存のactive Issueは宣言resourceを推測せず、slot 0と`repo:*`のexclusive leaseへ安全側に移行する。既存の未処理Issueは本文やlabelを変更せず受理し、metadataが揃うまでは`repo:*`へ縮退する。active Issueが複数あるなどconcurrency 1の前提と矛盾するv2 stateは自動修復せずmigrationを拒否する。

未知のconfig/state/metadata versionは推測で読み替えない。config/stateの未知versionは起動を拒否し、Issue metadataだけが未知versionの場合はそのIssueを`repo:*`へ縮退させる。現行v4からv3へのrollbackも全loop停止、active leaseなし、対応backupの確認を必須とし、active leaseを古いstateへ捨てて戻してはならない。

## 11. Self-hosting初期taxonomy

`ishii1648/codex-issue-loop`で並列化をself-hostする際の初期resource名を次のようにする。これは導入時の`.agent-loop.yaml`へ明示して正本化し、表だけをruntime入力にはしない。

| Resource | 主なpath glob | 意図 |
| --- | --- | --- |
| `config` | `.agent-loop*.yaml`、`internal/platform/config/**`、`docs/resource-admission.md`、`docs/specification.md` | config schemaとadmission契約 |
| `scheduler` | `internal/domain/**`、`internal/application/supervisor/**`、`internal/adapter/state/**`、`internal/adapter/webhook/**`、`internal/platform/schema/**`、`internal/platform/failure/**`、`internal/platform/fsutil/**` | ドメインdecision、選択、lease、永続状態、transaction補助 |
| `github` | `internal/adapter/github/**`、`internal/adapter/publish/**` | GitHub取得とpublication |
| `worker` | `internal/adapter/worker/**`、`schemas/**` | worker processとresult contract |
| `host` | `cmd/**`、`internal/application/app/**`、`internal/platform/launchd/**`、`internal/platform/registry/**`、`internal/platform/layout/**`、`internal/application/lifecycle/**`、`internal/adapter/worktree/**` | CLI、LaunchAgent、host-local lifecycle |
| `operations` | `internal/application/observe/**`、`internal/platform/retention/**`、`internal/platform/redact/**`、`docs/*runbook.md` | 監視、保持、運用 |
| `release` | `.github/**`、`scripts/**`、`Makefile`、`go.mod`、`go.sum`、`assets.go`、`internal/platform/compat/**`、`internal/application/migration/**`、`docs/release.md`、`docs/compatibility.md`、`docs/migration.md` | build、配布、互換性、migration |
| `docs` | `.gitignore`、`README.md`、`AGENTS.md`、`CLAUDE.md`、`docs/**` | 上記に含まれない横断文書 |

globは重なり得るため、たとえば`docs/resource-admission.md`を変更するIssueは`area:config`と`area:docs`の両方をclaimする。複数領域を横断するrefactorは必要な全resourceをclaimし、taxonomy外の大規模変更や範囲が確定できないIssueはmetadataを省略して意図的に`repo:*`へ縮退させる。
