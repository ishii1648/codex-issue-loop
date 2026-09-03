# codex-issue-loop アーキテクチャ概要

## 1. この仕組みが解決すること

`codex-issue-loop` は、常時稼働する Mac mini 上で GitHub Issue を順番に取得し、Issue が存在する限り Codex CLI のワーカーを繰り返し実行する仕組みである。Mac miniは実行ホストであり、Issueの作成主体ではない。

claim、lease、attention、publicationなど、本書とコードで使う中核用語は[用語集](glossary.md)に集約する。詳細な振る舞いと不変条件は、用語集から参照する各正本文書に従う。

ユーザーはスマートフォンから監視taskを主な操作入口として利用できる。

- **監視task**: ループの起動・停止・状態確認・質問への回答

仕事の投入元はCodexのIssue作成task、GitHub UI、`gh`やGitHub API、GitHub Actions等のautomationのいずれでもよい。作成元にかかわらず、着手可能条件を満たすGitHub Issueが共通のキュー境界になる。Codex taskは操作画面または任意のproducerであり、ループの実行主体ではない。監視taskが終了しても、`launchd` 配下のsupervisorは処理を継続する。

## 2. 全体像

![任意のIssue producer、GitHub、Mac mini上のsupervisorとCodex workerの関係](images/architecture-overview-v2.png)

図中の `DURABLE STATE` がループ状態の正本である。fsnotify/kqueueによるstate directory eventは即時性のために使い、60秒間隔のreconciliationで通知の取りこぼしやbackend停止を修復する。[ADR-0003](adr/0003-event-notification.md)を正本とする。

スマートフォンから監視taskへの紺色の矢印はCodex Remoteによる操作経路、`WATCH`から監視taskを経てスマートフォンへ戻るオレンジ色の破線はCodexの通知経路を表す。通知は正本ではなく、永続snapshotへ戻るための補助経路である。supervisorから外部serviceへの直接配送経路は持たない。

## 3. コンポーネントと責務

| コンポーネント | 実行主体 | 主な責務 |
| --- | --- | --- |
| 監視task | Codex app | CLI操作、状態の要約、ユーザーへの質問、回答登録 |
| Issue producer | Codex app、GitHub UI、CLI/API、automation等 | 要望整理、Issue作成、着手可能ラベル付与 |
| agent-loop Skill | Codexが読む手順 | 自然言語を安全なCLI操作へ対応づける |
| agent-loop CLI | Goの短命プロセス | start、stop、status、watch、answer |
| launchd | macOS | supervisorの起動と異常終了時の再起動 |
| delivery controller | Macごとに1つの短命LaunchAgent | production Releaseのpull、検証、host-wide drain、update、health check、rollback |
| supervisor | Goの常駐プロセス | Issue選択、claim、worker起動、状態遷移、復旧 |
| Codex worker | `codex exec` / `codex exec resume` | 1件のIssueの調査、worktree内の実装・検証、構造化結果の返却 |
| publisher | supervisor内の決定論的処理 | 差分検査、commit、push、draft PR作成・既存PR再利用 |
| PR lifecycle controller | supervisor内の決定論的処理 | CI監視、Ready化、任意のbranch更新・squash merge、merge確認 |

`supervisor.go`はscheduler入口と共通orchestrationだけを保持し、claim/worker、publication、checks、conflict、GitHub同期は同packageのvertical lifecycleへ分離する。各verticalはremote/worktreeのobserve、typed domain decision、`Store.Update`によるaggregate validation付きatomic persistence、GitHub/worker/publisher effectを同じ責務境界で完結させる。`app.go`もcommand dispatchだけを保持し、install、control、attention/answer、operator recoveryを別use caseへ分離する。file/function上限と例外理由はarchitecture testで固定し、package dependency方向とobservable lifecycle testを分割前後で共用する。
| GitHub | 外部共有状態 | Issueキュー、ラベル、コメント、Pull Request |
| 永続状態 | ローカルファイル | snapshot、event log、未回答request、世代番号 |

責務の中心は次のように分かれる。

```text
Issue producer = 作成場所を問わない仕事の投入
Codex app      = 人との対話、任意のproducer、監視
agent-loop     = 決定論的な制御と継続実行
Codex worker   = 1 Issue内の非決定的な開発作業
GitHub         = producerとループが共有する仕事のキュー
```

Release deliveryではGitHub Actionsの責務をbuild、test、SBOM/checksum、provenance attestation、Release公開までに限定する。production Macへ接続するworkflow、inbound listener、self-hosted runnerは置かない。同じログインユーザーの`com.codex-issue-loop.delivery`が既存の`gh`認証を使ってproduction Releaseをpullする。設定の正本はMac単位の`$HOME/.agent-loop-delivery.yaml`、transaction/cache/logは`$HOME/Library/Application Support/codex-issue-loop/delivery/`であり、repositoryの`.agent-loop.yaml`とは別のtrust boundaryである。

delivery controllerがmaintenance fenceを作ると、全repository schedulerは新規claim、retry/resume、conflict worker、PR maintenanceをdispatchしない。実行中workerへsignalを送らず、PID/PGIDが消えてsnapshotがflushされ、supervisorが`maintenance`へ到達するまで待つ。適用後のdoctorとbounded soakが完了するまでfenceを解除せず、失敗時はprevious installへrollbackする。LLM、Issue worker、GitHub Actionsはいずれもこのtransactionを所有しない。

現行production/self-hostingのownership境界は1 host・1 supervisor・同時worker 1件である。複数workerの実装とresource taxonomyは保持するが、安定化期間中の設定上限は`queue.concurrency: 1`とする。将来の再有効化は同じsupervisor内のworker slotとしてisolated canaryを通し、複数host冗長化は外部の線形化可能なcoordinatorとfenced publication gatewayを必須とする。GitHub labelを分散lockとして使わない。詳細は[ADR-0002](adr/0002-concurrency-and-multi-host.md)を正本とする。

単一host並列化でschedulerが評価するresource claim、Issue依存関係、local leaseの契約は[Resource admission契約](resource-admission.md)を正本とする。admissionと待機中の監視はGoコードで完結し、worker起動前の判断にLLMを使わない。

### 3.1 コードのドメイン境界と読解順

`internal`直下は責務ではなく依存方向を表す4層に限定する。`cmd`は具象実装を組み立てるcomposition rootであり、この4層の外側に置く。

```text
internal/
├── domain/       # 副作用のない語彙、不変条件、policy、decision
├── application/  # use caseと外部effectのorchestration
├── adapter/      # state、GitHub、workerなど外部境界の具象実装
└── platform/     # config、filesystem、launchdなどhost共通機能
```

許可するpackage依存は次の向きだけである。applicationが現在のcomposition/use case境界を持つためadapterの具象型を利用するが、adapterからapplicationへ逆流させない。将来portを抽出する場合も、この規則を狭める方向で行う。

| import元 | import可能な内部layer |
| --- | --- |
| `domain` | `domain` |
| `platform` | `platform`, `domain` |
| `adapter` | `adapter`, `platform`, `domain` |
| `application` | `application`, `adapter`, `platform`, `domain` |

production、内部test、外部testのimport graphをアーキテクチャテストで検査し、`internal`直下へのlayer外package追加と依存方向の逆流を拒否する。

人が実装を読むときは、外部I/Oを起点にせず、次の順序を基本とする。

1. `internal/domain/**`で状態名、不変条件、状態遷移の入力と出力を確認する。
2. `internal/domain/admission/**`と`internal/domain/capability/**`で、Issue選択と実行能力の決定論的な判定を確認する。
3. `internal/application/supervisor/**`で、ドメインdecisionを永続transactionと外部effectへ対応づけるapplication orchestrationを確認する。
4. `internal/adapter/state/**`、`internal/adapter/github/**`、`internal/adapter/worker/**`、`internal/adapter/publish/**`で永続化・外部I/Oの実装詳細を確認する。

`internal/domain/**`はfilesystem、process、clock、network、永続storeを直接参照せず、観測済みの値を入力として副作用のないdecisionを返す。Issue lifecycleの主状態である`Status`、GitHub反映待ちを表す第二状態軸`GitHubSync`、resource parkや各recoveryのsub-stateを同packageで型付き語彙として定義する。reconciliationではsupervisorがGitHub、worktree、processの観測をDTOへ正規化し、遷移先、retry時刻、GitHub同期、lease解放可否をdomain decisionが決める。retry/continuation/conflict/publicationの予算判定も同じ純粋層に置く。

`app`または`supervisor`はdecision作成後、`internal/adapter/state/issue_transition.go`のcommit境界を通して、永続snapshotのstatusがdecisionの観測したstatusから変化していないことを検証してcommitする。このfenceはstatus一致だけを保証し、Run IDやlease generationなどの所有権fenceは従来どおり各transaction closureが検証する。claim予約だけは、Issue recordの作成とlease競合検査を同じtransactionで線形化するため、`state.ReserveLease`がtransaction内のcurrent statusを`domain.StartClaim`へ渡す。

新しいIssue lifecycle遷移を`Store.Update`のclosureへ直接追加してはならない。まず`internal/domain/**`へ名前付きdecisionとtable-driven testを追加し、`state.ApplyIssueTransition`から適用する。汎用的にstatusを書き換えるcompatibility APIは設けない。

CLI recoveryとclaim予約を含むproduction codeの`Issue.Status`直接更新は、`internal/adapter/state/issue_transition.go`の`ApplyIssueTransition`へ集約している。アーキテクチャテストは`go list ./...`と`go/types`で全production packageと内部・外部test packageを走査し、`state.Issue.Status`への代入をこの1箇所だけの個数付きallowlistとして固定する。また、`Status`、`GitHubSync`、型付きsub-stateに対する文字列リテラルの代入、比較、`switch` caseと、判断箇所での`Status.String()`による型剥がしも拒否する。これにより、変数名、添字式、composite literal、package境界、test codeの違いに依存せず、生代入や未型付けのlifecycle語彙を追加できない。

未知のlifecycle語彙はstate validationでfail-closedに拒否する。このため、新しいstatusやsub-stateを永続化した版からその語彙を知らない旧版へロールバックすると、旧バイナリはstate file全体を読み込めない。語彙追加時はschema/semantic migrationだけでなく、rollback可能範囲と復旧手順もRelease変更として定義する。

## 4. 通常の実行フロー

1. 任意のproducerが着手可能ラベル付きのGitHub Issueを作成する。
2. supervisorがGitHubから着手可能なIssueを取得する。
3. 決定論的な順序で1件を選び、ラベルとローカル状態でclaimする。
4. Issue専用のbranchとworktreeを用意する。
5. `codex exec`ワーカーがpreflightを行い、そのまま実装を開始する。
6. ワーカーはworktree内で実装とテストを行い、構造化結果を返す。Git metadata、remote、GitHubは変更しない。
7. supervisorのpublisherが差分を検査し、署名なしで非対話commit、push、draft PR作成を冪等に行う。
8. supervisorがCIを監視し、成功したdraft PRをReady for reviewへ移す。失敗時は同じworktreeでworkerを再試行する。
9. `auto_merge: false`では人手のmergeを監視しながら次のIssueへ進む。`auto_merge: true`では必要ならbase branchへ追随してCIを再確認し、squash mergeまで同じIssueを所有する。
10. mergeを確認した後でIssueを完了扱いにし、設定に応じてcloseする。キューが空なら低負荷で待機する。

IssueごとのCodex workerは外側のループを所有しない。次のIssueを選ぶのは常にsupervisorである。

## 5. preflightと実行プロファイル

preflightは別のユーザー確認工程ではなく、各Issueの最初に必ず行う論理フェーズである。初回のCodex workerは次を確認したうえで、そのまま作業を開始する。

- 受け入れ条件と変更範囲
- 依存関係と検証方法
- 破壊的操作、権限追加、外部公開、課金の有無
- 複数段階の調査や反復が必要か
- 1回のworker実行で完了できる見込み

preflightは実行方針を次のどちらかに分類する。

| profile | 用途 | supervisorの扱い |
| --- | --- | --- |
| `standard` | 範囲が明確で、1回のworker実行で完了が見込める | 通常の上限とtimeoutで実行 |
| `extended` | 調査、移行、広範な変更、複数回の検証が必要 | continuation budgetを確保し、必要なら`codex exec resume`で継続 |

分類が難しい場合にユーザーへ質問してはならない。可逆性と安全性を維持できる限り、より強い `extended` を選ぶ。

preflightは必ずしも2つのCodex workerを起動することを意味しない。初回workerの中でpreflightから実装へ連続して進み、追加実行が必要な場合だけsupervisorが継続workerを起動する。

## 6. Codex Goalの位置づけ

Codex Goalは、一つの具体的な目的と検証可能な完了条件を追う長時間作業に適している。一方、GitHub Issueを選び続ける無期限のキュー処理とは責務が異なる。

このため、次の境界を設ける。

- Goalを外側のIssueループ、プロセス監視、永続状態の正本にはしない。
- headless workerは `standard` / `extended` profileと`codex exec`を既定経路にする。
- `extended` の継続はsupervisorが管理し、`codex exec resume`を使う。
- 監視taskで「この障害を復旧する」など単一目的を追う場合は、ユーザーがGoalを利用してよい。

したがってGoalは対話的な単一目的の内側に限定し、headless workerの製品機能にはしない。App Server方式は中核lifecycleが安定し、`codex exec resume`では満たせない要件と継続的なreplay testを定義できた段階で別Issueとして再評価する。

## 7. 質問で止まる場合

ワーカーは次の場合に限って `needs_input` を返す。

- 外部仕様やUI動作を変えるプロダクト判断
- データ削除、公開、課金、credential、権限拡大
- リポジトリ内の情報だけでは安全に決められない事項
- 相反する受け入れ条件

命名、局所的な実装、既存規約から推測できる事項、容易に戻せる内部構造については質問せず進める。また、`standard` / `extended` の分類をユーザーへ質問しない。

`needs_input` は一過性の通知ではない。request IDと質問内容を永続状態へ保存し、通常workerではactive leaseをpark済みclaimへ移して、ユーザーが回答するまでattention状態とcontinuationを保持する。監視taskが切断されても質問は失われない。

## 8. 監視を取りこぼさない仕組み

### 8.1 基本方針

監視では次の優先順位を守る。

1. **永続状態が正本**
2. **event通知は低遅延化のヒント**
3. **低頻度pollingは取りこぼし修復**

fsnotify/kqueueでstate directoryを監視するが、そのeventだけに依存しない。eventを受信した場合も、そのpayloadを正本とはせず永続状態を読み直す。watcherを作成・登録できない場合はpolling-onlyへ降格する。

### 8.2 watchアルゴリズム

`agent-loop watch --until-attention` は次の順序で動作する。

1. snapshotを読み、既にattention状態なら直ちに返す。
2. event通知を購読する。
3. 購読開始とのraceを防ぐためsnapshotをもう一度読む。
4. eventまたはreconciliation期限までOSレベルで待機する。
5. 起床後にsnapshotを読み直す。
6. `needs_input`、`blocked`、`stopped`等なら構造化結果を返す。
7. 通常状態なら再び待機する。

snapshotには単調増加する `state_revision` を持たせる。watchは最後に確認したrevisionを記録し、再接続後も状態変化を比較できる。

reconciliation間隔は実装が安全な内部値を所有し、複数watchがある場合はjitterを加える。Issueキューの取得間隔とは別の機構だが、どちらもrepository設定で調整する対象にはしない。

### 8.3 トークン消費の境界

reconciliation pollingはGoプロセス内で行い、待機中のheartbeatや途中結果をCodexへ返さない。この待機処理はモデルを呼び出さないため、polling自体はLLMトークンを消費しない。

Codexに「一定時間ごとにstatusを確認する」と推論させる設計は禁止する。Codexはwatchがattention結果を返した後の要約、ユーザーへの質問、回答登録にだけ使う。

ただし、Codex task内で保留中のtool callについて、OpenAIが課金を含む厳密なゼロトークン保証を公開しているとは限らない。そのため「Go側の待機ではモデル呼び出しを行わない」という実装上の保証と、Codex製品全体の課金仕様を区別する。

## 9. 障害時の振る舞い

| 障害 | 継続するもの | 復旧方法 |
| --- | --- | --- |
| 監視taskが終了 | supervisor、Issue処理、永続質問 | 同じまたは新しい監視taskからstatus/watch |
| event通知を取りこぼす | 永続状態、attention状態 | 60秒以内のreconciliation |
| watchプロセスが終了 | supervisor、永続状態 | 監視taskからwatchへ再接続 |
| Codex workerが異常終了 | worktree、run状態 | backoff後に再試行またはextended continuation |
| supervisorが異常終了 | snapshot、event log、GitHub状態 | launchd再起動後にreconciliation |
| Macがスリープ | 実行は停止し得る | macOSの「ディスプレイoff時もスリープさせない」を有効化 |

外部supervisorから任意のDesktop taskを直接wakeし、モバイルUIを`Needs input`へ変える公開契約には依存しない。通常はrepositoryごとにpinしたDesktop監視taskがblocking watchから戻った後に質問し、question notificationとActivityの回答待ちへ残す。監視taskが接続されていない期間は永続snapshotにattentionを保持し、再接続時にstatus-firstで回収する。詳細は[Codex Desktop監視task運用](codex-desktop-monitoring.md)と[公式仕様確認](codex-capability-review.md)を参照する。

## 10. 設計上の不変条件

- ループの正本状態をCodex taskの会話履歴だけに置かない。
- 未回答requestは回答または明示取消まで消さない。
- eventを状態そのものとして扱わない。
- Codexによる定期pollingを行わない。
- 1 Issueにつき同時に1つのwriterだけを許可する。
- GitHubへ公開できるpublisherをrepository単位で1つの論理writerに限定する。
- workerに次Issueの選択や無限ループを任せない。
- preflightの実行経路選択でユーザーを止めない。
- Goalを永続supervisorの代替にしない。

## 11. 日常運用のイメージ

通常、ユーザーが直接操作する必要があるのは監視taskだけである。

1. GitHub UI、CLI/API、automation、または任意の `[INTAKE] <repo> — new issue` taskから新しい仕事を登録する。
2. `[LOOP] <repo> — monitor` で状態確認と必要な回答を行う。

ループに着手可能なIssueがあればsupervisorが自動的に処理する。質問が必要になれば監視taskのwatchが戻り、Codexがユーザーへ質問する。回答後は同じworktreeと保存済みコンテキストを使って処理を再開する。
