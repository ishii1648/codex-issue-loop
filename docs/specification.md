# codex-issue-loop システム仕様

## 1. 位置づけ

`codex-issue-loop` は配布リポジトリ名、`agent-loop` はユーザーが操作するCLI名とする。

ループの制御は決定論的なGoプログラムが担当し、実装上の判断とコード変更のみをCodexワーカーへ委譲する。

## 2. 全体構成

![codex-issue-loopの全体構成](images/architecture-overview-v2.png)

```text
Issue producers (outside Mac mini)
  ├─ Codex intake task on any host
  ├─ GitHub UI
  ├─ CLI / GitHub API
  └─ automation ─────────────────────────► GitHub Issues
                                                  │ ready queue
                                                  ▼
Mac mini — execution host
  ├─ ChatGPT mobile app ── Codex Remote ──► [LOOP] monitor task
  │          ◄──────────── Codex notification ────────┘
  │                                             └─ Skill ──► agent-loop CLI
  │                                                            ├─ start ──► launchctl ──► launchd
  │                                                            ├─ status / answer ───────────┤
  │                                                            └─ watch ── event + 60s ──────┤
  │                                                                         reconcile        ▼
  └─ supervisor ◄──────── GitHub Issues                            durable state/events
         └─ pick/claim ──► worktree + codex exec ──► branch / draft PR
```

Codex notificationは、`watch`がattention状態を返した監視taskからChatGPTモバイルアプリへ届く。supervisorや永続状態からスマートフォンへ直接pushするadapterは、この経路に含めない。

### 2.1 責務境界

| コンポーネント | 責務 | 保持しないもの |
| --- | --- | --- |
| Issue producer | GitHub Issue作成、要望・完了条件の記載、着手可能ラベル付与 | ループ制御、Mac mini上の状態 |
| Codex監視task | ユーザーとの会話、CLI操作、質問表示 | ループの正本状態 |
| agent-loop Skill | 自然言語からCLIへの安全な操作手順 | 常駐プロセス、Issue選択ロジック |
| agent-loop CLI | 設定、プロセス制御、監視、回答登録 | 実装判断、監視状態の正本 |
| launchd | supervisorの起動と再起動 | Issue状態機械 |
| supervisor | Issue選択、状態遷移、Codex起動、復旧 | プロダクト判断 |
| Codex worker | 1 Issueの調査、実装、検証、結果報告 | 次Issueの選択、無限ループ |
| GitHub | Issue/PRの共有状態 | ローカル実行詳細 |
| 永続snapshot/event log | supervisor状態、attention request、revision | ユーザーとの会話 |

Codex Skill の実行主体はCodex taskである。Skillは実行可能プロセスではなく、Codexが読む操作手順である。長時間動き続ける実行主体は `agent-loop run` supervisor である。Issue producerはこの実行経路から独立しており、Mac mini上で動作する必要はない。

## 3. 技術選定

### 3.1 言語

Goを採用する。

理由:

- 単一バイナリで配布しやすい
- 常駐プロセス、signal、subprocess、ファイルロックを標準機能中心で実装できる
- macOS/arm64向けのビルドとテストが容易
- 対象リポジトリの言語やruntimeに依存しない

初期実装ではCGOを必須にしない。永続状態は原子的JSON snapshotとappend-only JSONL event logを用いる。要件が増えた場合のみ組み込みDBへの移行を検討する。

### 3.2 外部コマンド

- `git`: worktree、branch、commit状態の操作
- `gh`: GitHub Issue、label、comment、Pull Requestの操作
- `codex`: 非対話ワーカー
- `launchctl`: LaunchAgentの登録と制御

各コマンドは絶対パスを登録時に解決し、LaunchAgentの限定されたPATHに依存しない。

登録時には`git`、`gh`、`codex`、`launchctl`の絶対パスと実行時PATHをregistryへ保存する。LaunchAgentはこのPATHを引き継ぐが、credential値は環境へ追加しない。

## 4. ディレクトリ構成

### 4.1 配布リポジトリ

```text
codex-issue-loop/
├─ cmd/agent-loop/
├─ internal/
│  ├─ config/
│  ├─ supervisor/
│  ├─ state/
│  ├─ github/
│  ├─ codex/
│  ├─ gitworktree/
│  ├─ launchd/
│  └─ observe/
├─ schemas/
│  └─ worker-result.schema.json
├─ skill/
│  └─ agent-loop/SKILL.md
├─ launchd/
│  └─ agent-loop.plist.tmpl
├─ scripts/
├─ docs/
├─ go.mod
└─ README.md
```

### 4.2 対象リポジトリ

```text
target-repository/
├─ .agent-loop.yaml
├─ AGENTS.md                 # 任意だが推奨
└─ ...
```

### 4.3 ユーザー領域

```text
~/Library/Application Support/codex-issue-loop/
├─ bin/agent-loop
├─ install.json
├─ backups/<timestamp>-<version>/
├─ migration.json
├─ migrations/<timestamp>-v1-to-v2/
├─ registry.json
├─ worktrees/<repo-id>/issue-<number>/
└─ repos/<repo-id>/
   ├─ state.json
   ├─ events.jsonl
   ├─ events.jsonl.<timestamp>.gz
   ├─ supervisor.log
   ├─ launchd.stdout.log
   ├─ launchd.stderr.log
   ├─ lock
   ├─ prompts/
   └─ runs/<run-id>/

~/Library/LaunchAgents/
└─ com.codex-issue-loop.<repo-id>.plist

~/.codex/skills/agent-loop/
├─ SKILL.md
└─ VERSION
```

`repo-id` は GitHub の `owner/repo` とcanonicalなローカルパスから生成した、人間が識別可能なprefix付きstable hashとする。リポジトリ移動時は再登録を必要とする。

`install.json`はrelease version、Git commit、binaryとSkillのSHA-256、そのbinaryが要求する永続schema versionを保持する。`update`はinstall一式を`backups`へ保存してから、稼働中だったLaunchAgentだけを停止し、binary・Skill・plistを置換して再開する。途中失敗時は自動rollbackする。state、event、registry、worktreeはinstall/update/uninstallの対象外とする。

## 5. 設定仕様

設定ファイル名は対象リポジトリ直下の `.agent-loop.yaml` とする。

```yaml
version: 2

github:
  repo: ishii1648/example
  ready_labels: [codex-loop:ready]
  exclude_labels: [blocked, do-not-automate]
  running_label: codex-loop:running
  needs_input_label: codex-loop:needs-input
  failed_label: codex-loop:failed
  done_label: codex-loop:done

queue:
  poll_interval: 60s
  concurrency: 1
  order: issue_number_asc
  priority_labels: []
  max_attempts: 3
  continue_after_needs_input: true

worker:
  command: codex
  model: null
  sandbox: workspace-write
  session_mode: resumable
  timeout: 2h
  timeout_grace: 30s
  ambiguous_profile: extended
  profiles:
    standard:
      max_continuations: 0
    extended:
      max_continuations: 3

watch:
  reconcile_interval: 60s
  reconcile_jitter: 10%

git:
  branch_prefix: codex/issue-
  worktree_root: null
  base_branch: main

completion:
  create_draft_pr: true
  auto_merge: false
  close_issue: true

worktrees:
  completed_max_age: 168h
  failed_max_age: 720h
  blocked_max_age: 0s
  needs_input_max_age: 0s

logs:
  rotate_bytes: 16777216
  rotate_interval: 24h
  generations: 7
  worker_run_max_age: 720h
  worker_run_max_count: 100

security:
  redact_env: []
```

### 5.1 設定規則

- `version` は必須。現行は`2`とし、v1は明示migrationの対象、未知versionはエラーとする。
- `github.repo` は `owner/name` 形式で必須。
- `queue.concurrency` はMVPでは `1` のみ許可する。将来の単一host並列化と複数host冗長化は別のmigrationであり、この値だけでdistributed modeを有効化しない。
- `queue.order`は`issue_number_asc`、`created_at_asc`、`priority_then_created_at`を許可する。既定値は後方互換な`issue_number_asc`とする。
- `priority_then_created_at`では`queue.priority_labels`を高い順に1件以上指定する。labelなしは最低順位、複数該当は最上位一致とする。
- `worker.model: null` はユーザーのCodex既定値を使う。
- `worker.sandbox` の既定値は `workspace-write` とする。
- `worker.session_mode` は初回run後に `extended` と判定された場合に継続できるよう `resumable` とする。terminal状態へ到達したIssueのsession IDはactive stateから外す。
- `worker.ambiguous_profile` は `extended` 固定とし、MVPではユーザー確認へ切り替える設定を設けない。
- `queue.poll_interval` はGitHub Issueキューの再取得間隔、`watch.reconcile_interval` はattention監視の取りこぼし修復間隔であり、別の設定として扱う。
- `watch.reconcile_jitter` は複数watchの同時起床を避けるため、各待機期限へ加える。
- worktree root未指定時はユーザー状態領域配下を使う。
- durationはGo duration形式とする。
- 未知キーは設定ミスを検出するため既定でエラーとする。
- secretsを設定ファイルに記述しない。
- `security.redact_env`には追加でマスクする値そのものではなく、値を保持する環境変数名だけを記述する。
- `git.worktree_root`を指定する場合は絶対pathとし、branch prefix、base branch、GitHub repository名はargv/refとして安全な形式だけを許可する。
- `completion.create_draft_pr`の既定は`true`とする。`completion.auto_merge`の既定は`false`であり、`true`はdraft PR作成を前提とする。
- `completion.auto_merge: false`ではCI成功後にPRをReady for reviewへ移し、人手のmergeを待つ。`true`ではbase branchへの追随とCI再確認後にsquash mergeする。
- `completion.close_issue`はPRのmerge確認後にだけ適用し、既定は`true`とする。
- worktree保持期間の`0s`は無期限を表す。既定はcompleted 7日、failed 30日、blockedとneeds-inputは無期限とする。`resume_pending`はneeds-inputのポリシーへ含め、その他の非terminal状態は期間にかかわらず保持する。
- event、supervisor、launchd、worker logは16 MiBまたは24時間でrotationし、gzip世代を7件保持する。worker run directoryは30日かつterminal run 100件を上限とし、active、retry、`needs_input`は削除しない。

### 5.2 log rotationと容量保護

- `events.jsonl`はstate lock内で、現行logをgzip archiveへcopyした後、現在の`state_revision`を持つ`event_log_checkpoint`へ原子的に置換する。以後のsequenceはcheckpointから連続させる。
- archive作成中に停止した場合、active logは置換前の完全な履歴または置換後のcheckpointのどちらかであり、次回Loadで検証できる。余分なarchiveは世代整理で除去する。
- supervisorとworkerのstream logは書込前に閾値を検査し、close、gzip、active file再作成の順でrotationする。
- launchdが直接開くstdout/stderrは起動時にrotationする。常時の運用logはprocess管理の`supervisor.log`へ出力し、`logs`はgzip archiveからactive fileまで時系列で表示する。
- terminal worker runを削除した場合は`worker_logs_pruned`監査eventを残す。未回答requestとactive/retry中のrunは保持する。
- 利用可能容量がrotation閾値の2倍未満なら、新しいworkerを起動する前にsupervisorを`blocked`へ移し、`doctor`と容量復旧手順で扱う。

## 6. CLI仕様

### 6.1 共通規則

```text
agent-loop <command> [options]
```

- 対象は `--repo <path>`、現在ディレクトリ、registryの順で解決する。
- 対象が一意に決まらなければ終了コード `2` と候補を返す。
- `--json` 指定時はstdoutにJSONだけを出し、診断ログはstderrへ出す。
- 破壊的・復旧用操作は通常コマンドから分離する。

### 6.2 コマンド一覧

| コマンド | 目的 |
| --- | --- |
| `init [--agents codex,claude] [--apply]` | Codex / Claude Codeのuser-scope Issue作成ルールをpreviewまたは明示適用する |
| `install` | バイナリに対応するSkillと共通ディレクトリをセットアップする |
| `uninstall` | 実行中プロセスを確認してインストール物を削除する |
| `register --repo PATH` | 対象リポジトリを検証し、registryとplistを生成する |
| `unregister --repo PATH` | 停止確認後に登録を解除する |
| `start` | LaunchAgentをbootstrap/kickstartする |
| `stop` | LaunchAgentを停止する。Issue状態は保持する |
| `restart` | 停止後に再起動する |
| `status` | snapshot、launchd状態、GitHub状態の要約を返す |
| `watch` | イベントを追跡する |
| `answer` | 未回答requestへ回答を登録する |
| `logs` | supervisorまたはIssue別ログを表示する |
| `cleanup --repo PATH [--apply]` | worktreeの保持・安全性planを表示し、停止中かつ安全な期限切れ対象だけを削除する |
| `purge --repo PATH --issue N --confirm TOKEN` | 停止中の単一worktreeを完全一致token付きで強制削除する |
| `doctor` | 依存関係、認証、設定、電源条件、状態整合性を検査する |
| `bootstrap-labels --repo PATH [--apply]` | 必須GitHubラベルの変更計画を表示し、明示時だけ不足分を作成する |
| `run` | launchd専用の内部supervisorエントリーポイント |

`bootstrap-labels`は`--apply`なしではread-onlyのplanを返す。`--apply`時も不足ラベルの`gh label create`だけを実行し、既存ラベルに`--force`を指定せず、更新・削除を行わない。部分成功は成功分を保持してlabel別のfailureを返し、再実行で不足分だけを処理する。`doctor`が不足ラベルを検出した場合はこの修復コマンドを表示する。

`init`は`--apply`なしではuser設定もagent-loop内部ディレクトリも変更しない。release binaryに埋め込んだrule versionと本文を、Codexの有効なuser-level `AGENTS.md`系file内の管理block、およびfrontmatterなしのClaude Code専用ruleへ反映する。競合は上書きせず、symlinkの解決先、予定action、適用結果、backup pathをJSONへ出力する。詳細と復旧手順は[user-scope Issue作成ルール](user-rules.md)を参照する。install manifestのSkill整合性とは分離し、`install`、`update`、`doctor`、`uninstall`はこの設定を暗黙に変更しない。

`doctor --repo PATH`はhostと対象repository、`doctor`はhostとregistry内の全repositoryを診断する。JSONは`schema_version: 1`、全体`ok`、`generated_at`、`diagnostics`から成る。各diagnosticは安定した`code`、`ok`、`scope`、任意の`repo_id`、人間向けsummary/detail、0件以上のremediationを持つ。remediationはkind、summary、command/settings、automatic、destructiveを持つ。doctor自身は修復を実行せず、state破損時にもfileを変更しない。

### 6.3 start

```text
agent-loop start --repo /path/to/repo [--json]
```

処理:

1. 登録と設定を検証する
2. 既に実行中なら成功として現在状態を返す
3. plistを `launchctl bootstrap gui/<uid>` で登録する
4. 必要に応じて `launchctl kickstart` する
5. 起動確認を行う

`start` 自身は常駐しない。

### 6.4 watch

```text
agent-loop watch --repo /path/to/repo --until-attention [--json]
```

`--until-attention` の終了条件:

- 未回答の `needs_input`
- supervisorが `blocked`
- 明示的な停止
- `--until-idle` も指定された場合のキュー空

未回答質問が既に存在する場合、待機せず即時返却する。watchはsnapshotとevent logを読み取るだけで、supervisorの親プロセスにはならない。

watchは永続snapshotを正本とし、fsnotify/kqueueによるstate directory eventを低遅延化のヒントとして扱う。event payloadだけで終了条件を判定せず、起床のたびにsnapshotを読み直す。個別fileではなくdirectoryを監視するため、`state.json`のatomic rename後も新しいfileを検出できる。watcher作成・登録失敗またはchannel終了時はpolling-onlyへ降格する。[ADR-0003](adr/0003-event-notification.md)を正本とする。

raceを避けるため、watchは次の順序で待機する。

1. snapshotを読み、attention状態なら返す
2. event通知を購読する
3. snapshotを再読し、購読開始前後の状態変化を確認する
4. eventまたはreconciliation期限までOSレベルで待機する
5. 起床後にsnapshotを再読する
6. 終了条件を満たさなければ待機へ戻る

reconciliationの既定間隔は60秒とする。内部polling中はheartbeatや途中結果をstdoutへ出さず、Codexへ制御を戻さない。したがって待機中にモデルを定期実行しない。

### 6.5 answer

自由記述:

```text
agent-loop answer --request-id req_... --message "回答"
```

stdin:

```text
agent-loop answer --request-id req_... --message-file -
```

処理:

1. request IDと未回答状態を検証する
2. 回答を原子的に保存する
3. `answer_recorded` イベントを追記する
4. supervisorをwakeする
5. 既に同一回答が登録済みなら成功、異なる回答ならconflictとする

### 6.6 終了コード

| code | 意味 |
| --- | --- |
| `0` | 成功、または期待されたwatch終了 |
| `1` | 実行時エラー |
| `2` | 引数・設定エラー |
| `3` | 対象が未登録または曖昧 |
| `4` | 競合、古いrequest、二重claim |
| `5` | 認証・権限不足 |

## 7. launchd仕様

登録単位は1対象リポジトリにつき1 LaunchAgentとする。

主要plist設定:

- `Label`: `com.codex-issue-loop.<repo-id>`
- `ProgramArguments`: 絶対パスの `agent-loop run --repo <absolute-path>`
- `RunAtLoad`: true
- `KeepAlive`: 異常終了時に再起動する条件
- `ThrottleInterval`: 再起動stormを防ぐ値
- `WorkingDirectory`: 対象リポジトリ
- `StandardOutPath` / `StandardErrorPath`: repo別状態ディレクトリ
- `EnvironmentVariables`: 必要最小限のPATHとHOME。tokenは含めない

LaunchAgentなので、ユーザーがログアウトしている間は動作保証しない。system-wideなLaunchDaemon、自動ログイン、daemon/helper分割は、ユーザーcredential、HOME、Keychain、Codex認証、worktree ownershipの境界を変え、現在の可用性要件に対して過剰なため採用しない。正式な比較と再検討条件は[ADR-0001](adr/0001-macos-execution-model.md)を正本とする。

## 8. supervisor状態機械

### 8.1 リポジトリ状態

```text
stopped
   │ start
   ▼
starting ──► polling ◄──────────────┐
                 │ issue found      │
                 ▼                  │
              claiming              │
                 │                  │
                 ▼                  │
              running               │
        ┌────────┼─────────┐        │
        ▼        ▼         ▼        │
 awaiting_checks needs_input retry_wait
        │        │         │         │
  CI成功│        │ answer  └─────────┘
        ▼        ▼
 awaiting_merge running
        │ merge確認
        ▼
   completed ───────────────────────► polling

running ──fatal/nonrecoverable──► blocked
```

`needs_input` はIssue単位の状態であり、`continue_after_needs_input: true` の場合、supervisor全体は別Issueのpollingを続けてよい。ただし同一worktreeは回答まで変更しない。

### 8.2 Issue状態

- `claimed`
- `running`
- `needs_input`
- `retry_wait`
- `awaiting_checks`
- `awaiting_merge`
- `completed`
- `failed`
- `blocked`

すべての遷移はsnapshot更新前にevent logへ記録し、再起動時にsnapshotとGitHubを照合する。

## 9. Issue選択とclaim

### 9.1 着手可能条件

Issueは以下をすべて満たす場合に着手可能とする。

- stateがopen
- `ready_labels` をすべて持つ
- `exclude_labels` を一つも持たない
- running、needs-input、done、failedラベルを持たない
- Pull Requestではない
- ローカル状態で処理中ではない

Issueのauthor、作成場所、作成に使ったclientは着手可能条件に含めない。producerは着手可能ラベルを付与できるGitHub権限、または同ラベルを付与するautomationを持つ必要がある。

### 9.2 並び順

既定はIssue番号昇順とする。`created_at_asc`は作成日時、Issue番号の順、`priority_then_created_at`はconfigured priority、作成日時、Issue番号の順で比較する。configured priority labelがないIssueはpriority付きIssueの後へ置き、複数付いている場合は設定上の最高順位を採用する。

GitHub CLIのpaginationが完了した候補集合をlocalでsortし、APIの返却順やpage境界には依存しない。設定変更時もactiveなIssueはそのまま継続し、未claim候補の次回選択から新しい順序を使う。詳細とtarget repositoryのlabel準備は[Queue ordering](queue-ordering.md)を参照する。

### 9.3 claim手順

1. 候補一覧を取得してローカルでsortする
2. 先頭Issueの最新状態を再取得する
3. ローカル状態へwrite-aheadの`claiming`を保存する
4. runningラベルを追加し、readyラベルを外す
5. run IDを含む開始コメントを冪等キー付きで作成する
6. ローカル状態を `claimed` にする

`claiming`で停止した場合は、再起動後に最新Issueを再取得し、同じrun IDと冪等markerでclaimを再実行する。

GitHub APIには汎用的なcompare-and-swapがないため、MVPは「同一リポジトリを処理するsupervisorは1つ」という運用制約を置く。local `flock`は同一hostだけを保護する。複数host対応では単なる分散lockだけでなく、線形化可能なcoordinator、epoch、durable publication intent、GitHub副作用を集約するfenced publication gatewayを必要とする。[ADR-0002](adr/0002-concurrency-and-multi-host.md)を正本とする。

## 10. worktreeとGit仕様

- branch: `codex/issue-<number>-<slug>`
- worktree: `<worktree-root>/<repo-id>/issue-<number>`
- base branchは処理開始時にfetchした設定値
- 既存branchまたはPRがある場合は対応関係を検証して再利用する
- ユーザーの通常working treeは変更しない
- 未コミット変更があるworktreeを自動削除しない
- force pushを行わない
- 完了後もPRがopenの間はworktreeを保持できる設定を用意する
- cleanupは既定read-onlyとし、`--apply`でもdirty、未push commit、open PR、未回答requestを削除しない
- cleanup/purge適用時はLaunchAgent停止を必須とし、`git worktree remove`後に`git worktree prune`を実行する
- cleanupはlocal branchを残し、削除前後を`worktree_cleanup_started` / `worktree_cleaned`へ監査記録する
- purgeはIssue単位の完全一致確認tokenを必須とし、`worktree_purge_started` / `worktree_purged`へ安全性と復元元を記録する

## 11. Codexワーカー仕様

### 11.1 起動

概念上、次の形式で実行する。

```text
codex exec \
  --cd <issue-worktree> \
  --sandbox workspace-write \
  --json \
  --output-schema <worker-result.schema.json> \
  <generated-prompt>
```

初回runはpreflight後に `extended` と判定された場合にresumeできるよう、resumable sessionとして起動する。supervisorは出力からsession IDを取得してIssue状態へ保存する。`standard` が完了した場合はsession IDをactive stateから外し、以降のloop状態をCodex sessionへ依存させない。

実際の引数は起動前に `codex exec --help` とversion capabilityを検査する。構造化された初回実行を安全に行えないversionでは推測で継続せず、supervisorの開始を拒否する。session resumeだけが利用できない場合は、同じIssue worktree、run ID、永続化された回答履歴を使って新規sessionを起動する。session IDはJSONL内の`thread_id`、`session_id`、および既知のnested形式を受け付ける。

### 11.2 preflightとexecution profile

preflightは別プロセスではなく、初回worker promptに含める論理フェーズとする。workerは最初に受け入れ条件、変更範囲、依存関係、検証方法、リスク、反復回数の見込みを整理し、`standard` または `extended` を選択した後、そのまま実装へ進む。

分類規則:

| profile | 選択条件 | 実行方針 |
| --- | --- | --- |
| `standard` | 変更範囲と完了条件が明確で、単一run内で実装と検証を完了できる見込み | 初回runで完了を目指し、continuationを既定では許可しない |
| `extended` | 広範な調査、migration、複数コンポーネント、長時間テスト、段階的検証が必要 | supervisorがcontinuation budgetと同一sessionのresumeを管理する |

分類が難しい場合は `extended` を選ぶ。profile選択は内部の実行戦略であり、ユーザーへ質問しない。ユーザー質問は11.4の質問ポリシーに該当する実質的な判断だけに限定する。

初回workerはpreflight結果を実行ログへ構造化eventとして出力するが、preflightだけで終了しない。したがって `extended` の判定だけを理由に2つのworkerが必ず動くわけではない。初回runで完了しなかった場合に限り、supervisorが保存されたsession ID、worktree、検証結果を使って `codex exec resume` を起動する。ユーザー回答後の再開は自動continuation budgetとは別に扱う。

MVPではnative Goalをheadless workerの実行機構にしない。Goalは監視task内の単一目的の復旧作業等に利用できるが、Issueキュー、process lifetime、永続状態を所有しない。App Serverの`thread/goal/set|get|clear`、`thread/resume`、`turn/start`は公式提供済みであり、`extended` profileのoptional adapterをIssue #53で検証する。導入までは`codex exec resume`を維持する。[Codex公式仕様確認](codex-capability-review.md)を正本とする。

### 11.3 ワーカープロンプト

次を含む。

- repository、base branch、worktree
- Issue番号、title、body、関連コメント
- 現在の試行番号
- 過去の質問とユーザー回答
- 実行可能な範囲と禁止事項
- 対象リポジトリのAGENTS.mdに従う指示
- 実装とテストに関する完了条件
- `git add`、commit、push、PR作成、`agent-loop`の再帰起動を行わず、公開処理をsupervisorへ返す指示
- 構造化結果の意味
- 質問すべき条件と、推測して進めてよい条件
- preflightの分類規則と、曖昧な場合は質問せず`extended`を選ぶ指示

### 11.3.1 決定論的な公開境界

Codex workerにはIssue worktreeだけを`workspace-write`で渡す。linked worktreeのGit metadataは元repositoryの`.git/worktrees`配下にありsandbox外なので、workerへ書き込み権限を広げない。workerが`completed`を返した後、supervisor内のpublisherが次を順に実行する。

1. `git status --porcelain`で差分を確認し、差分があれば`git add --all`と`git diff --cached --check`を行う。
2. `git -c commit.gpgsign=false ... commit`で対話的な署名promptを発生させずcommitする。
3. 対象branchをpushする。
4. 同じhead branchのopen PRを検索し、1件なら再利用、0件ならdraft PRを作成、複数なら安全側で停止する。
5. `gh`が警告文を併記しても出力内の有効なPR URLを抽出し、異なるURLが複数なら拒否する。
6. `statusCheckRollup`を定期的に確認し、未完了ならモデルを呼ばず待機、失敗なら同じworktreeと失敗理由をworkerへ返す。
7. CI成功後にdraftをReady for reviewへ移す。`auto_merge: false`では人手のmergeを監視しながら次のIssueへ進む。
8. `auto_merge: true`ではbase branchより遅れているPRを更新してCIを再確認し、conflictがなければsquash mergeする。merge確認後だけIssueを完了扱いにする。

この境界により、モデルのsandboxをremote操作のために広げず、公開処理の引数、順序、冪等性をGo側で固定する。

### 11.4 質問ポリシー

質問する:

- 要件の選択肢が複数あり、外部仕様やUI動作が大きく変わる
- データ削除、公開、課金、credential、権限拡大が必要
- リポジトリ内の情報だけでは安全に決められない
- 相反する受け入れ条件がある

質問せず合理的な仮定で進める:

- 命名や局所的実装詳細
- 既存規約から一意に推測できる事項
- 容易に戻せる内部構造
- テスト追加やformatなど通常の実装作業
- `standard` / `extended` のprofile選択

質問時は、調査済み事実、判断が必要な理由、推奨案、2〜3個までの選択肢を返す。単なる進捗報告を質問にしない。

### 11.5 構造化結果

```json
{
  "version": 1,
  "status": "completed",
  "execution_profile": "standard",
  "summary": "Implemented ...",
  "question": null,
  "tests": [
    {"command": "go test ./...", "result": "passed"}
  ],
  "git": {
    "branch": "codex/issue-123-example",
    "commit": "abc1234",
    "pull_request_url": "https://github.com/owner/repo/pull/456"
  },
  "retry": null
}
```

`needs_input` の例:

```json
{
  "version": 1,
  "status": "needs_input",
  "execution_profile": "extended",
  "summary": "Both APIs are compatible with the current code.",
  "question": {
    "text": "Which compatibility policy should be used?",
    "reason": "The choice changes the public API.",
    "recommended_option": "preserve-v1",
    "options": [
      {"id": "preserve-v1", "label": "Preserve v1"},
      {"id": "breaking-v2", "label": "Adopt v2"}
    ],
    "allow_free_text": true
  },
  "tests": [],
  "git": null,
  "retry": null
}
```

ワーカーのstdout JSONLは実行ログとして保存し、最終メッセージのみをschema検証済み結果として採用する。schema不一致は `retryable_failure` とする。

## 12. 永続状態とイベント

### 12.1 state.json

最低限次を保持する。

```json
{
  "version": 2,
  "repo_id": "example-a1b2c3d4",
  "state_revision": 42,
  "supervisor": {
    "state": "polling",
    "pid": 12345,
    "started_at": "2026-08-15T08:00:00Z",
    "updated_at": "2026-08-15T08:10:00Z"
  },
  "issues": {},
  "pending_requests": {}
}
```

一時ファイルへのwrite、fsync、renameにより原子的に更新する。ファイルpermissionはユーザーのみ読み書き可能とする。

`state_revision` は有効な状態更新ごとに単調増加させる。event通知を取りこぼしたwatchも、最後に確認したrevisionとの差分から状態変化を検出できる。

Issue状態にはbranch、worktree、session ID、PR URLに加えてmerge確認済みフラグを保持する。これにより起動時は未確認の旧`completed` PRだけを一度照合し、merge確認済みの履歴へ毎回GitHub APIを呼ばない。

### 12.2 events.jsonl

各行を独立したJSONイベントとする。

共通フィールド:

- `version`
- `event_id`
- `sequence`
- `timestamp`
- `repo_id`
- `issue_number`（該当時）
- `run_id`（該当時）
- `type`
- `payload`

主要イベント:

- `supervisor_started`
- `claim_started`
- `issue_claimed`
- `worker_started`
- `worker_preflight_completed`
- `worker_continuation_started`
- `worker_completed`
- `pull_request_checks_pending`
- `pull_request_poll_scheduled`
- `pull_request_ready`
- `input_requested`
- `answer_recorded`
- `retry_scheduled`
- `issue_completed`
- `issue_failed`
- `github_state_synced`
- `supervisor_blocked`
- `supervisor_stopped`

### 12.3 transactionと破損復旧

各状態更新は、更新後snapshotと対応eventを `state.txn.json` へ先に原子的に保存してから、event logへのappend、snapshotの置換、transaction削除の順でcommitする。各段階でprocessが停止しても、次回のreadまたはsupervisor起動時にtransactionから不足分を補完する。

起動時とread時には次を検証する。

- `state_revision` と最後のevent `sequence` が一致する
- event `sequence` が1から単調に連続する
- snapshot、event、transactionのversionと `repo_id` が一致する
- prepared transaction内のsnapshot revisionとevent sequenceが一致する

改行前で停止したevent log末尾は、最後の完全なeventとsnapshot revisionが一致する場合に限り切り詰め、`event_log_tail_truncated` を記録する。prepared transactionが残っている場合は、そのtransactionを正本としてeventとsnapshotのcommitを完了する。

transactionなしでsnapshotとevent logが食い違う場合、途中に壊れたeventがある場合、またはsnapshotを復元できない場合は自動推測しない。既存の `state.json`、`events.jsonl`、`state.txn.json` をrepository state directory配下の `recovery/` へ隔離し、元の理由とbackup pathを含む `recovery_blocked` 状態を新しいsnapshotへ保存する。blocked状態では通常の状態更新とIssue処理を拒否し、backupを保持したまま手動復旧を待つ。

### 12.4 永続schema migration

config、registry、state、active event log、prepared transactionの現行schemaはv2とする。v1を検出したbinaryはsupervisor開始、status、通常update後の自動再開を拒否し、`migrate --json`と`doctor`で`SCHEMA_MIGRATION_REQUIRED`を返す。v3以上やversion欠落は自動変換しない。

v1からv2へのforward migrationは次の順序で行う。

1. 全登録LaunchAgentが停止中であることを確認する
2. config、registry、state、active event、transactionをchecksum付きmigration backupへcopyする
3. `migration.json`へ`prepared` journalを原子的に保存する
4. 各fileを個別に原子的置換する
5. 全対象がv2へ収束したことを再検査し、journalを`completed`にする

process停止でv1/v2が混在しても、再実行は同じjournalとbackupを使い、v1のfileだけを変換する。active event logはfile全体を一度に置換し、同一log内のversion混在を許可しない。rotation済みgzip archiveはruntime復旧入力ではないためimmutableな監査履歴として保持する。

rollbackは管理対象migration backupのmanifest、restore先、SHA-256を検証してから全fileを復元する。schema v1対応binaryへ戻す場合は、先にschema backupをrestoreし、その後に対応するinstall backupをrestoreする。schemaとbinaryの対応versionが異なるrollbackはCLIが拒否する。

## 13. 監視とCodex task連携

### 13.1 Skillの標準フロー

監視開始:

1. `agent-loop doctor --json`
2. `agent-loop start --json`
3. `agent-loop status --json`
4. 未回答質問がなければ `agent-loop watch --until-attention --json`

watch呼び出し後、Codexは独自のtimerや定期status確認を開始しない。event受信とreconciliation pollingはGoのwatchプロセスが担当し、attention条件を満たすまでCodexへ途中結果を返さない。

入力待ち:

1. watchの結果を人間向けに要約する
2. questionをほぼそのままユーザーへ提示する
3. 回答後に `agent-loop answer` を実行する
4. statusを確認する
5. watchへ戻る

停止:

1. 現在Issueと未コミット状態を表示する
2. ユーザーの停止意図を確認する
3. `agent-loop stop` を実行する
4. 最終statusを表示する

### 13.2 Codex app上の制約

外部supervisorからCodexアプリ内taskへ直接メッセージを挿入したり、task状態を変更したりする非公開機能には依存しない。

`Needs input` のCodex内表示は、監視task内で実行中のwatchが戻り、Codex自身がユーザーへ質問することで成立する。監視taskが接続されていない間も質問は永続状態に残り、opt-inの外部push adapterは永続outboxからスマートフォンへ直接通知できる。再接続時には通知の成否に関係なく未回答質問をsnapshotから即時表示する。

App Server所有threadのprogrammatic continuationと、任意のDesktop taskを外部processからwakeしてmobile表示を変更する機能は別契約として扱う。後者の公式APIが提供された場合はoptional adapterとして追加できる。

### 13.2.1 外部push adapter

初期providerはntfyとする。attentionの永続更新と同じtransactionで通知outboxへenqueueし、`pending`、`sent`、`failed`、`canceled`、試行回数、次回試行時刻をsnapshotへ保持する。notification IDはrequestまたはblocked原因から決定し、再起動やreconciliationで同じattentionを重複enqueueしない。

送信はsupervisor cycleの前後で行う。失敗は上限付きexponential backoffで再試行し、event/logへ記録するが、Issue選択・worker・publisherを停止しない。回答済みrequestに対する未送信通知は送信前に取消す。既定本文はrepository、Issue番号、request ID、状態だけとし、質問文と失敗理由は`include_details`が明示された場合だけ含める。

credentialはrepository別管理directoryのmode `0600` fileへ専用CLIで保存し、設定file、LaunchAgent plist、repository、command引数へ置かない。endpointはHTTPS、topicは推測困難かつaccess control済みとする。通知tapはGitHub Issueを開き、安定した公式deep linkが提供されるまではCodex taskを直接起動しない。詳細は[スマートフォン直接push通知](notifications.md)を正本とする。

Codex App Serverは`thread/tokenUsage/updated`とGoalの`tokensUsed`を提供し、`codex exec --json`もturn完了時のusageを返す。一方、Claude Codeのmonitor toolと同じ汎用的なtoken-free契約や、保留中tool call・long commandの厳密なzero-token/zero-costは公式に保証されていない。本システムが保証するのは、Goのwatchプロセス内のevent待機とreconciliationがLLMを呼び出さないことである。[Codex公式仕様確認](codex-capability-review.md)を参照する。

### 13.3 eventとreconciliationの役割

| 機構 | 役割 | 信頼性上の扱い |
| --- | --- | --- |
| event通知 | 状態変化を低遅延でwatchへ知らせる | 取りこぼし得るhint |
| 永続snapshot | 現在状態、未回答request、revisionを保持する | 唯一の正本 |
| 60秒reconciliation | event欠落、watch再接続、raceを修復する | 最終的な検出経路 |

`needs_input`、`blocked`、`stopped`等のattention状態はstickyとし、event送信後に自動解除しない。requestへの回答、明示的な復旧操作、または状態機械で定義された遷移だけが解除できる。

## 14. エラー処理と再試行

### 14.1 分類

| 種類 | 例 | 動作 |
| --- | --- | --- |
| 一時障害 | network timeout、GitHub 5xx、Codex一時失敗 | backoff後に再試行 |
| Issue固有の恒久障害 | test不能、競合要件、許可されない操作 | Issueをblocked/failedにして次へ |
| supervisor全体の障害 | auth失効、config不正、binary消失 | supervisorをblockedにして通知対象 |
| ユーザー入力待ち | product判断、権限承認 | requestを保存し、設定に応じて次へ |

実装上はerrorを `transient`、`issue`、`supervisor` のtyped classificationで伝播させる。未分類errorは安全側に倒して `supervisor` とみなし、文字列の部分一致で遷移を決めない。workerが返す `needs_input` はerrorではなく状態遷移として扱う。

### 14.2 backoff

- 初回: 5秒
- 倍率: 2
- 上限: 5分
- jitter: ±20%

- Issueワーカーの既定上限: 3回
- polling失敗はsupervisorを終了させず、連続失敗閾値でblockedにする

worker retry、GitHub queue polling、GitHub同期retryの待機時間に独立した乱数を適用する。exponential backoffはjitter適用後も5分を超えない。乱数源はprocessごとに初期化されたsystem sourceを使い、複数repositoryの再試行集中を避ける。clockとrandom sourceはテスト時に差し替え可能とする。

Issue retryのsnapshotには `failure_kind`、`last_error`、`retry_after` を保存し、`retry_scheduled` eventにも分類、理由、予定時刻、delayを記録する。supervisorの一時障害も同じ情報と連続失敗数をsnapshotおよび `supervisor_retry_scheduled` eventへ保存する。連続失敗数と予定時刻はlaunchdによるprocess再起動後も引き継ぐ。連続失敗数は、GitHub取得から永続状態更新までを含む1回のsupervisor cycleが成功した場合にのみresetし、`supervisor_recovered` eventを記録する。event通知やretry状態自身の書き込みによる早期起床ではbackoffを解除せず、counterもresetしない。

`supervisor` 分類は継続による状態破壊を避けるため直ちにblockedへ移す。`transient` 分類は最大5回まで再試行し、5回連続で失敗した場合にblockedへ移す。`issue` 分類は対象Issueをblockedまたはfailedへ移し、GitHub同期後に別のIssueを処理できる状態を維持する。

### 14.3 worker timeoutとprocess終了

`worker.timeout`に達した場合、workerのPIDをprocess group IDとしてgroup全体へ`SIGTERM`を送る。親processが先に終了しても子processが残っている間は終了完了とみなさない。`worker.timeout_grace`の間にprocess group全体が終了しなければ、同じgroupへ`SIGKILL`を送り、親processを必ずwaitして回収する。既定値はtimeout 2時間、grace period 30秒とし、grace periodは正数かつtimeout以下でなければならない。

timeout理由には、設定timeout、grace period内に終了したか、強制終了まで進んだかを含める。supervisorはworker PIDを0へ戻し、`retry_wait`と一時障害理由を永続化する。Issue worktree、dirty file、commit、session IDは削除しない。次回試行またはLaunchAgent再起動時は通常のstartup reconciliationを実行し、既存branch、worktree、push済みbranch、open/merged/closed PRを調べてから継続する。

## 15. 再起動時のreconciliation

supervisor起動時に次を行う。

1. lockを獲得し、二重supervisorを拒否する
2. state snapshotとevent logの整合性を検証する
3. GitHub Issue、branch、PRの現況を取得する
4. 保存されたworker PIDが生存しているか確認する
5. worktreeとGit状態を検証する
6. merge済みPRがあれば完了へ収束させ、旧versionでcompletedにされたopen PRはCI/merge監視へ戻す
7. 実行途中でworkerが消えていればretryへ移す
8. 未回答requestはneeds_inputのまま保持する
9. reconciliationイベントを記録してpollingを開始する

reconciliationでは、永続状態を処理履歴の正本、GitHubとGit worktreeを外部事実の正本として扱う。次の不一致は、重複実行や人手変更の上書きを生じない範囲で自動収束させる。

| 検出した状態 | 起動時の処理 |
|---|---|
| 保存済みworker PIDが存在するがprocessは消失 | PIDを破棄し、active Issueを即時retryへ移す |
| write-ahead claim後に停止し、GitHubはrunningへ遷移済み | claim済みとして即時retryへ移す |
| 保存前にpush・PR作成まで完了 | branchに紐づく単一のopen PRを保存して処理を継続する |
| 旧versionでcompletedだがPRがopen | draftなら`awaiting_checks`、Readyなら`awaiting_merge`へ戻し、done labelを除いて監視を再開する |
| PRがmerge済み | Issueをcompletedへ移し、未反映のdone label/commentを再同期する |
| needs-input、done、failedのlabel/comment同期が途中 | marker付きcommentを照合し、不足しているGitHub更新だけを再実行する |
| running labelだけが欠落し、worktreeが整合 | running labelを修復する |

次の不一致は自動で上書きせず、Issueを `blocked` にして理由を保存する。

- 保存済みworker PIDが現在も生存している
- readyとrunning/needs-inputが同時に付与されている
- exclusion labelが人手で付与された
- PRがmergeされずcloseされた、または同じbranchに複数のopen PRがある
- Issueがdoneを伴わずcloseされた
- 保存済みworktree、local branch、open PRのremote branchが消失した
- worktreeのbranchが保存値から変更された
- claim中のready/running labelが両方とも除去された

完了・失敗・除外を示すGitHub labelとmerge済みPRは、古いactive snapshotより優先する。一方、branch名、worktree、open PR、Issue stateの競合は、どちらかを推測して削除・再作成しない。各照合結果は `startup_reconciled` eventへ、変更前後の状態、理由、worktree inspection、検出したPRとともに記録する。

## 16. セキュリティ仕様

- subprocess引数はshell文字列として連結せず、argv配列で起動する
- Issue本文をshellとして評価しない
- Issueのタイトル・本文・コメントは件数とbyte数を制限し、制御文字を除去してuntrusted JSON dataとしてprompt命令から分離する
- worktree pathはcanonical化し、許可root配下であることを確認し、root逸脱とworktree symbolic linkを拒否する
- 管理directoryは0700、plist、registry、状態、event、transaction、worker/supervisor logは0600で作成する
- credentialをpromptへ明示的に埋め込まない
- 既知credential形式と`security.redact_env`の値をstdout/stderr、worker result、state、event、GitHub通知の境界でmaskする
- 外部push tokenは専用のprivate管理fileへmode `0600`で保存し、設定、plist、repository、log、state、eventへ値を残さない
- Codex sandboxは既定で `workspace-write`とし、worker起動時に`approval_policy="never"`を上書きする
- dangerous bypassは設定schemaでもMVPでは許可しない
- GitHub Issueは信頼済み入力とはみなさず、prompt injectionの可能性をworkerへ明示する
- stopはプロセスを終了するが、worktreeや未コミット変更を削除しない
- reset/purgeを実装する場合は別コマンドとし、明示確認を必須にする
- CIで到達可能なGo依存脆弱性を`govulncheck`により検出する
- 信頼境界、残余risk、最小権限、backup、credential棚卸しは`docs/threat-model.md`と`docs/security-runbook.md`を正本とする

## 17. テスト仕様

### 17.1 ユニットテスト

- configの正常・異常系
- Issue filterと決定論的sort
- 全状態遷移
- answerの冪等性とconflict
- structured result validation
- preflight profile分類と曖昧時の`extended` fallback
- retry/backoff
- secret masking
- repo-id生成

### 17.2 統合テスト

- fake GitHub adapter + fake Codex process
- worktree作成、再利用、異常終了
- supervisor二重起動防止
- snapshot途中書き込みからの復旧
- worker kill後のreconciliation
- watchの接続、切断、複数接続
- event通知を破棄した場合の60秒reconciliation
- watcher生成・購読失敗とevent channel終了時のpolling-only fallback
- 実fsnotifyを使う複数watchと終了後の再接続
- read-subscribe-read間に状態が変わるrace
- attention状態と`state_revision`の永続化
- standard workerが追加runなしで完了すること
- extended workerだけが設定上限内でresumeされること
- 外部pushのoutbox永続化、重複抑止、再送、rate limit、回答後取消、adapter障害からのsupervisor分離
- 外部push credentialの0600保存、無出力、redaction、通知本文の詳細opt-in
- 将来のsingle-host worker slotで同一Issueを二重割当しないこと、およびclaim/publishの直列化
- coordinator adapterのCAS、epoch、lease expiry、partition、古いhost拒否、publication takeover conformance

### 17.3 macOS E2E

- install/register/start/stop/uninstall
- `launchctl`による自動再起動
- Macの画面off中の継続
- Codex Remoteからの監視開始
- `needs_input`のスマートフォン通知、回答、再開
- 監視task未接続時の外部push到達、再起動後の非重複、provider障害中のloop継続
- ChatGPT desktop taskを閉じた後のsupervisor継続
- Codexによる定期status確認なしでwatchがattentionまで待機すること
- multi-host障害環境でpartition中に片側だけが進行し、二重branch・Pull Requestを作らないこと

## 18. 実装時に確定する項目

以下は要件を変えずに実装検証で確定する。

- Goの最低version
- Codex CLIの最低対応version
- `gh` JSON fieldとlabel更新の具体コマンド
- worker timeout時のgrace period
- desktop app更新によるRemote/通知表示差異のE2E手順
- Codex CLI session resumeのversion別capabilityとfallback
- distributed coordinatorとpublication gatewayのbackend、認証、backup、障害環境
