# Mac mini常駐運用runbook

最終確認日: 2026-08-16

repositoryごとのversion更新は[Repository別stable delivery](per-repository-delivery.md)を、`codex-issue-loop`自身が壊れて通常loopを利用できない場合は[break-glass repair](break-glass-repair.md)を正本とする。stable Release公開だけでは登録repositoryを自動更新しない。

このrunbookは、Apple Silicon Mac mini上で`agent-loop`を常駐させ、ChatGPTモバイルアプリのCodex Remoteから起動、監視、質問への回答、停止を行うための標準手順である。ループ本体はLaunchAgentとして動作し、Codex taskやCodex desktop appの生存には依存しない。一方、スマートフォンからMacを操作する経路には、ログイン中のmacOSユーザーセッション、起動中のCodex desktop app、同一アカウントのRemote接続が必要である。

コマンド例の`/absolute/path/to/repository`は、対象Git repositoryの絶対パスへ置き換える。複数repositoryを運用するときも、登録、操作、監視はrepository単位で行う。

## 1. 運用モデルと前提

Mac miniには、日常利用や管理者作業と分離した標準macOSユーザーを用意することを推奨する。そのユーザーで次を満たす。

- macOSユーザーがログイン中である。`agent-loop`はsystem daemonではなくユーザーのLaunchAgentなので、ログアウト中には動かない。
- Mac miniがAC電源と安定したnetworkへ接続され、「ディスプレイがオフのときに自動でスリープさせない」が有効である。
- Codex desktop appが起動し、Remote Controlが有効である。これはスマートフォンから操作する場合だけ必要であり、ループの実行主体ではない。
- Mac miniとChatGPTモバイルアプリが同一のOpenAIアカウントで認証されている。
- `git`、互換範囲内の`gh`、`codex`、ソースからbuildする場合はGoがインストール済みである。対応versionは[CLI互換性マトリクス](compatibility.md)を参照する。
- 対象repositoryのdefault branchにbranch protectionとrequired checksが設定され、workerの資格情報にbypass権限がない。

FileVaultを利用するMacでは、OS再起動後に利用者がdiskをunlockしてmacOSへログインするまでLaunchAgentとRemoteは復旧しない。自動ログイン、LaunchDaemon、root/system-wide credentialは採用しない。比較、security boundary、再検討条件は[ADR-0001](adr/0001-macos-execution-model.md)を正本とする。

## 2. 初回セットアップ

### 2.1 ソースの検証とインストール

配布releaseがない間は、確認済みのcommitをcloneしてbuildする。`main`の未確認な最新状態をそのまま本番へ入れない。

```sh
cd /absolute/path/to/codex-issue-loop
make ci
./bin/agent-loop --version
./bin/agent-loop install --json
```

インストール先は次の2か所である。

- `~/Library/Application Support/codex-issue-loop/bin/agent-loop`
- `~/.codex/skills/agent-loop/SKILL.md`

以降、インストール済みbinaryへPATHを通すか、絶対パスで実行する。LaunchAgentには登録時のbinary絶対パスが記録される。

`aqua`のproxy経由で`gh`や`go`を使う環境では、非対話LaunchAgentのPATHに`aqua`本体がないとproxyが失敗することがある。`command -v`の結果がproxy symlinkなら、実体のCLI絶対パスを登録するか、`aqua`本体を含むPATHで再登録し、`doctor`を実行する。

### 2.2 GitHubとCodexの認証

secret値を表示・記録せず、認証状態と対象repositoryへの権限を確認する。

```sh
gh auth status
codex login status
gh repo view owner/repository
```

未認証または期限切れなら、Mac mini上で次を実行する。

```sh
gh auth login
codex login
```

画面を利用できないCodex CLI認証では`codex login --device-auth`を使用できる。GitHub資格情報はMetadata read、Contents read/write、Issues read/write、Pull requests read/writeへ限定する。詳細は[セキュリティ運用runbook](security-runbook.md)を参照する。

### 2.3 対象repositoryの設定とラベル

対象repositoryのrootに`.agent-loop.yaml`を置き、[設定例](../.agent-loop.example.yaml)をもとにrepository名、入口ラベル、並列実行境界、base branch、公開方針を確認する。polling間隔やretry、保持期間などの内部運用値は記載しない。Go publisher整形を使うrepositoryでは`formatters.go.enabled: true`を明示し、`gofmt`を利用できるPATHでregisterする。任意formatter commandや追加引数は指定できない。GitHub Issue本文や設定へtokenを保存しない。

まず不足ラベルのplanだけを表示し、内容を確認してから適用する。

```sh
agent-loop bootstrap-labels --repo /absolute/path/to/repository --json
agent-loop bootstrap-labels --repo /absolute/path/to/repository --apply --json
agent-loop bootstrap-labels --repo /absolute/path/to/repository --json
```

2回目のpreviewで`create`が0件であることを確認する。既存ラベルの色や説明は上書きされず、ラベルは削除されない。必要なGitHub権限と部分失敗時の再実行方法は[GitHubラベルbootstrap runbook](github-labels.md)に従う。

### 2.4 macOSの電源設定

System SettingsのEnergyで「Prevent automatic sleeping when the display is off」を有効にする。CLIで現在値を確認する場合は次を使い、AC Powerの`sleep`が`0`であることを確認する。

```sh
pmset -g custom
```

管理者が明示的にCLIで設定する場合は、Mac miniのAC電源時のsystem sleepを無効化する。

```sh
sudo pmset -c sleep 0
```

この設定はmacOS側の永続設定でありCodex側の設定ではない。display sleepは有効のままでよい。system sleepを防止すると消費電力が増える。組織管理端末ではMDMのenergy policyを優先する。

### 2.5 登録、診断、開始

```sh
agent-loop register --repo /absolute/path/to/repository --json
agent-loop doctor --repo /absolute/path/to/repository --json
agent-loop start --repo /absolute/path/to/repository --json
agent-loop status --repo /absolute/path/to/repository --json
```

`doctor`はread-onlyである。`schema_version: 1`、`ok: true`、LaunchAgentのloaded状態、supervisorの状態を確認する。失敗codeに応じた復旧は[doctor診断・復旧runbook](doctor.md)に従い、表示されたremediationを無条件に自動実行しない。

formatterを有効にした場合は`FORMATTER_GO_AVAILABLE`も確認する。`FORMATTER_GO_NOT_REGISTERED`、`FORMATTER_GO_UNAVAILABLE`、`FORMATTER_GO_CAPABILITY_MISSING`ではloopを開始せず、Go toolchainと非対話LaunchAgentのPATHを直してregister、doctorを繰り返す。registerとdoctorは固定sourceをstdinへ渡すread-only probeで実際の整形capabilityも確認する。

### 2.6 Codex Remoteを接続する

Mac miniのCodex desktop appでSettingsのConnectionsを開き、「Control this Mac or PC」を有効にする。表示されたQR codeをChatGPTモバイルアプリで読み取り、同じOpenAIアカウントの接続先としてMac miniが表示されることを確認する。

Codex desktop appを終了、sign out、またはRemote Controlを無効化するとスマートフォン経路は切れる。再login後はRemote Controlを再度有効化する。Mac側のCodex設定で、Remote接続中にMacをawakeに保つoptionが利用できる場合も有効にする。ただし常駐運用ではmacOSのEnergy設定を正本とし、desktop appのoptionだけに依存しない。

## 3. Codex app上の監視taskと任意のIssue作成task

通常運用ではrepositoryごとに監視taskを用意する。Issue作成taskを使う場合は監視taskと分けると、会話履歴や役割が混ざらず、監視taskがblocking watch中でも新しい仕事を投入できる。

Desktopのquestion notifications、Activity、pin、複数repositoryのtask分離、Desktop/Mac再起動後の再接続を含む正式な運用契約は[Codex Desktop監視task運用](codex-desktop-monitoring.md)を参照する。

### 監視task

名前は`[LOOP] owner/repository — monitor`とする。最初に対象repositoryを明示し、次のように依頼する。

> `/absolute/path/to/repository`のagent-loopを監視して。doctorとstatusで状態を確認し、未回答質問がなければ`watch --until-attention --json`を1回実行して。needs_inputなら推奨案と選択肢を要約して私に質問し、回答後にrequest IDを維持してanswerへ渡してから監視を再開して。

監視中はCodex側で短い間隔のpollingを作らない。1回のblocking `watch`がOS eventと内部reconciliation pollingを組み合わせ、attentionが必要になったときだけCodexへ戻る。

### Issue作成task

Issue作成taskは任意である。名前の例は`[issues] owner/repository`とする。Issueの背景、完了条件、対象repository、着手可能かを会話で整理し、`codex-loop:ready`を付けてキューへ投入する。監視やloop processの所有はこのtaskに持たせない。

IssueはGitHub UI、`gh`、GitHub API、GitHub Actions等のautomation、または別ホストのCodexから作成してもよい。Mac miniは実行ホストであり、Issue作成元ではない。どの経路でも、対象repositoryにopen Issueを作成し、設定されたreadyラベルと任意のassignee・milestone条件を満たせば同じキューへ入る。readyラベルを付ける主体には対象repositoryの適切なGitHub権限が必要である。

## 4. スマートフォンからの日常操作

操作前にCodex Remoteで正しいMac miniを選び、対象repositoryの絶対パスまたは`owner/repository`を毎回明示する。`stop`、`restart`、`unregister`、`uninstall`では、Codexに対象と影響を復唱させてから実行する。

### 起動

監視taskへ「`owner/repository`のloopを開始して監視して」と依頼する。Codexは`doctor`、`status`、`start`、1回の`watch`の順で操作する。すでにrunningなら二重起動せず監視へ接続する。

### 状態確認

監視taskへ「現在のstatusを確認して」と依頼する。CLIを直接確認する場合は次を使う。

```sh
agent-loop status --repo /absolute/path/to/repository --json
```

### 監視へ再接続

Codex taskが終了または切断してもループ本体は継続する。同じ監視taskまたは新しいtaskからstatusを読み、未回答requestがなければ次を1回実行する。

```sh
agent-loop watch --repo /absolute/path/to/repository --until-attention --json
```

監視task未接続時のattentionは永続snapshotに保持される。再接続時は`status`から現在のrequestを読み直し、未回答requestをwatchより先に表示する。

### 質問へ回答

`needs_input`では、監視taskがquestion、recommendation、options、request IDを提示する。回答にはcredentialやsecretを含めない。Codexは回答を標準入力から渡す。

```sh
printf '%s\n' '選択した方針と必要な補足' | agent-loop answer \
  --repo /absolute/path/to/repository \
  --request-id req_... \
  --message-file - \
  --json
```

記録後、同じrequest IDがansweredになったことをstatusで確認する。別Issueがroot `active_execution`を保持していれば、回答済みIssueはcontinuationを保持して待機し、実行枠が空いた後にschedulerが再開する。ready/running label、state、execution identityを手動編集しない。古いrequestや異なる二重回答はconflictとして扱い、推測で別requestへ転用しない。

### 停止

対象repositoryを確認してから、監視taskへ「状態とworktreeを残してloopを停止して」と依頼する。

```sh
agent-loop stop --repo /absolute/path/to/repository --json
agent-loop status --repo /absolute/path/to/repository --json
```

通常停止はdurable fenceで新規dispatchを止め、active lifecycleがresult/session/publication/GitHub同期を完了するまでworkerへsignalを送らず待つ。`status --json`の`operator_control.phase`と`operator_maintenance_fence`で進捗を確認する。CLIやDesktopが中断した場合はtransaction/fenceを編集せず、同じ`stop`または`restart`を再実行して同じgenerationを再開する。host reboot後も同じ手順とする。

`--timeout`では期限切れ時にworkerをkillせず通常運転へ戻る。drain中にsupervisor PIDが変わりorphan lifecycleが残った場合だけはfenceを保持してforce recoveryを要求する。再試行できない緊急時は対象と影響を再確認し、`agent-loop stop --force ...`または`restart --force ...`を使う。forceは保存process groupへ`SIGTERM`を送り、grace後の残存groupだけを`SIGKILL`する。通常停止もforce停止もstate、event、Issue worktree、未commit変更を削除しない。

## 5. 停止・障害からの復旧

復旧の基本順序は、変更を増やさずに`status`、`doctor`、logを集め、原因を1つずつ直し、`restart`することである。stateやworktreeを最初に削除しない。

```sh
agent-loop status --repo /absolute/path/to/repository --json
agent-loop doctor --repo /absolute/path/to/repository --json
agent-loop logs --repo /absolute/path/to/repository
agent-loop logs --repo /absolute/path/to/repository --stderr
```

`logs`は保持中のgzip世代と現行`supervisor.log`を古い順に連結して表示する。`--stderr`はlaunchdが捕捉した起動失敗を同様に表示する。既定のrotation・保持値は`.agent-loop.yaml`の`logs`で変更できるが、容量reserveを小さくしすぎない。

Incident起票判定は次で確認する。`decision_log`の保持期間は固定7日で、`decisions`は起票、再利用、dry-run、見送り、失敗のreason codeを返す。読み取りエラー時はfileを手編集せずautomationを停止し、[Incident自動対応runbook](incident-automation.md)のfail-closed手順に従う。

```sh
agent-loop incident status --repo /absolute/path/to/repository --json
agent-loop incident decisions --repo /absolute/path/to/repository --json
```

### `needs_input`

これは障害ではなく、workerが安全に続行するための入力待ちである。質問、推奨案、選択肢、Issue番号、request IDを確認し、[質問へ回答](#質問へ回答)の手順で回答する。質問が不明瞭なら、秘密を渡さず追加説明を回答として記録する。

### `blocked`

`doctor`の`diagnostics[].code`、直近event、supervisor stderrの順で原因を特定する。認証、設定、CLI互換性、GitHub権限、worktree不整合を直し、次を実行する。

```sh
agent-loop restart --repo /absolute/path/to/repository --json
agent-loop status --repo /absolute/path/to/repository --json
```

worktreeのbranch変更、未commit変更、remote PRとの不一致がある場合は自動修復しない。対象Issueのworktreeを人が確認し、変更を保持する方針を決める。

`publication_audited`の`reason`が`formatter_failed`なら、`formatter.failure_code`（`executable_unavailable`、`path_unsafe`、`exit_failure`、`timeout`、`canceled`、`verification_failed`等）を確認する。worktreeを削除せず、path safety違反は対象file種別とsymlinkを調査し、availabilityは停止後の再registerで直す。再試行は同じworktree・branchへ冪等に収束し、整形済みなら空commitを作らない。

terminal `blocked` / `failed` Issueを復旧するときは、scenario別commandやevent列の一致判定を使わない。まずcanonical snapshotと現在のprocess、worktree、Git、GitHubをまとめてread-only評価する。

```sh
agent-loop issue plan --repo /absolute/path/to/repository --issue 123 --json
```

planの`suspension`、`continuation_checkpoint`、evidence、missing evidence、および`resume|retry-stage|adopt-pr|cancel`それぞれのeligible/refusal codeを確認する。active PID/PGID、root execution identity、pending request、worktree/branch/head、open/merged PR、labelのいずれかが変わればresolveは拒否される。

operatorがplan上eligibleなactionを選択した後だけ適用する。

```sh
agent-loop issue resolve --repo /absolute/path/to/repository --issue 123 --action retry-stage --json
```

- `resume`は保存session/workspaceからworker境界を継続する。
- `retry-stage`は保存済みpublish/checks/conflict stageへ戻る。completed resultがある場合はSHA-256を照合し、workerを再実行しない。
- `adopt-pr`は保存branch/head/baseと一致するsame-repositoryの一意なmerged PRだけを採用する。commit、push、PR、mergeは作成しない。
- `cancel`はworkerを起動せずIssueを`canceled`へ収束させる。

terminal Issueはroot `active_execution`を持たず、base SHA、workspace、session、result digestはIssue-local `continuation`へ保持する。成功したresolveだけがgenerationを進めて単一実行枠を取得する。ambiguousなIssueはそのIssueだけをquarantineし、`cancel`以外を許可しないため、後続queueのcapacityを消費しない。

GitHub同期またはtransaction途中で停止した場合も同じ`issue plan`から再確認し、同じactionを再実行して冪等に収束させる。state/event/label/worktreeを手編集せず、別Issueのexecutionを変更せず、欠けたauthorityをevent件数・error文言・現在のbaseから合成しない。旧scenario別recordは全loop停止中のtyped migrationだけがv4 raw入力からgeneric continuationへ変換し、v5に残る旧状態はfail closedとする。

GitHub上でIssueを`not planned`としてcloseした場合は、startup/periodic/webhook/safety sweepが`status --json`の対象Issueを`canceled`へ収束させる。`github_state_reason: NOT_PLANNED`、`cancellation.previous_status`、`execution_release_result`、PR identityと`issue_canceled` eventを確認する。対象Issueのactive PID/PGIDまたはactive process、未回答request、root `active_execution`のIssue/run/generation不一致、複数/不一致PR、空または未知のstate reasonがある場合は自動cancelされない。一致するretained `active_execution`だけはprocess不在の再検証後に同じtransactionで解放され、別Issueの`active_execution`は保持される。workerへのsignal、worktree削除、state/label手編集で通過させてはならない。

### Git transportまたはcommit署名で停止する

`git config --global --get-regexp '^url\..*\.insteadof$'`でHTTPS URLがSSHへ書き換えられていないか確認する。LaunchAgentでSSH agentに依存しない運用では、対象repositoryのremoteをcredential helperで扱えるHTTPS URLに直す。URLやtokenをlogへ出さない。

publisherは対話停止を避けるため当該自動commitだけ`commit.gpgsign=false`を指定する。通常のユーザーcommit署名設定は変更しない。署名必須policyのrepositoryでは自動キューへ入れず、別の非対話署名方式を設計してから有効化する。

### GitHubまたはCodex認証の期限切れ

```sh
gh auth status
codex login status
```

Mac mini上で`gh auth login`または`codex login`を実行し、`doctor`が成功してからloopをrestartする。tokenをIssue、Codex task、logへ貼らない。Codex desktop appからsign outした場合は、再login後にRemote Controlも再度有効化する。

### disk容量不足

```sh
df -h
agent-loop stop --repo /absolute/path/to/repository --json
```

state directoryやIssue worktreeを容量確保のために直接削除しない。まず対象外のcache、download、古いbuild artifactなど、復旧可能性に影響しない領域を管理者が整理する。空き容量確保後にstate directoryをbackupし、`doctor`、`start`、`status`の順で確認する。どのfileの書き込みまで成功したか不明ならstate破損として扱う。

### stateまたはevent logの破損

loopを停止し、`~/Library/Application Support/codex-issue-loop`全体を削除せずbackupする。`doctor`はread-onlyなので、`STATE_CORRUPT`、`EVENT_LOG_INVALID`、registry関連codeとpathを記録する。正常に見える別fileで上書きせず、backup、対象Issue、worktree、GitHub上のlabel/PRを突合してから復旧方針を決める。自動復旧できない場合は[障害報告template](#障害報告template)でescalationする。

### OS再起動・logout後

1. FileVaultをunlockし、運用用macOSユーザーへloginする。
2. Codex desktop appが起動し、Remote Controlが有効か確認する。
3. `agent-loop status`でLaunchAgentのloaded状態を確認する。
4. loadedでなければ`doctor`後に`agent-loop start`を実行する。
5. 未回答requestを先に処理し、その後watchへ再接続する。

logoutはLaunchAgentとRemoteの両方を停止させる。screen lockやdisplay sleepはsystem sleepと異なり、Macがawakeでuser sessionが維持されていれば運用を継続できる。

## 6. backupとrestore

### backup

整合したbackupを取るには、全登録repositoryのloopを停止してから次を保全する。

- `~/Library/Application Support/codex-issue-loop`全体
- `~/Library/LaunchAgents/com.codex-issue-loop.*.plist`
- `~/.codex/skills/agent-loop/SKILL.md`
- 各対象repositoryの`.agent-loop.yaml`
- agent-loopのユーザー状態領域にあるmanaged worktree directory
- commitまたはpushされていないIssue worktree

backupは暗号化し、macOSユーザーと同等以上にアクセス制御する。GitHub/Codex tokenを別fileへexportしてbackupしない。通常のgit履歴とremote branch/PRも復旧点なので、保存すべき変更は可能な範囲でcommit・pushする。

### restore

同じmacOSユーザーと同じrepository絶対パスへ戻すのが最も安全である。

1. `agent-loop`の同じversionをinstallする。
2. loopが停止している状態でbackupを元のpathとpermissionへ戻す。
3. 対象repositoryごとに`register`を再実行し、現在のbinary絶対パスでplistを再生成する。
4. `doctor`でregistry、state、event、worktree、GitHubとの整合を確認する。
5. 問題がなければ1 repositoryずつstartし、statusとlogを確認する。

path、ユーザー、CLI version、state schemaが異なるMacへ移す場合は、その差分を障害報告へ記録し、破損fileを削除して先へ進まない。

## 7. 更新とrollback

release artifactの検証と更新方針は[Release・install・update方針](release.md)を正本とする。通常のschema-compatible updateはMac側pull型controllerを使う。

```sh
agent-loop delivery configure --json
agent-loop delivery configure --apply --json
agent-loop delivery status --json
```

`$HOME/.agent-loop-delivery.yaml`はownerがLaunchAgent user、mode `0600`、symlinkでないことを確認する。repository別`.agent-loop.yaml`へdelivery設定を追加しない。`status --json`でphase、current/desired/previous、drain進捗、backup、last/next checkを確認する。`rollback_failed`ではmaintenance fenceを手動削除せず、表示されたbackupを保全してdoctorの失敗codeを調査する。

外部原因を解消し、`doctor --json`が成功し、retained transactionのexact backupを検証できた場合だけ、operator確認後に検証済みの現行candidateから次を実行する。

```sh
/absolute/verified/agent-loop_Darwin_arm64 delivery retry-rollback \
  --backup '/absolute/managed/delivery-backup' \
  --confirm-retained-fence \
  --json
```

commandはdelivery lock、`rollback_failed` transaction、maintenance generation、desired identity、exact backup manifest、installed current/previous identityを再検証する。installed binaryが既にpreviousと一致する場合はbackupを再適用せず、maintenance下で全repositoryの再開とdoctorだけを再検証する。成功時だけtyped transactionを`rolled_back`へ進めてfenceを解除する。不一致やhealth failureではfenceとbackupを保持する。

retry中の新validator readが、旧`completed + pull_request_merged` recordのnumber/head欠落だけを理由にmaintenance snapshotを`recovery_blocked`へ隔離した場合は、JSONやbackupを手で戻さない。検証済みcandidateからexact backupを指定してpreviewし、全対象の旧identityとGitHub上のrepository-local merged PRがURL・branch・baseまで一致することを確認する。

```sh
/absolute/verified/agent-loop_Darwin_arm64 recover-quarantined-snapshot \
  --repo /absolute/path/to/repository \
  --backup '/exact/managed/recovery-backup' \
  --dry-run --json
```

LaunchAgent非稼働、`eligible=true`、`github_verified=true`、mutation scope、全repairsをoperatorが確認した場合だけ、`--dry-run`を`--confirm-legacy-merged-identities`へ置き換える。成功後は`doctor`を実行し、同じdelivery backupで`retry-rollback`を再実行する。追加invariant違反、別repo/fork/open PR、URL/branch/number不一致、exact backup不一致では使用しない。

semantic contract更新前後のbinaryでreadした結果、version mismatchだけを理由とするrevision 1 recovery markerが既に作られている場合は、markerが記録するbackupを1段ずつ指定する。別backupやstate fileをcopyしない。

```sh
/absolute/verified/agent-loop_Darwin_arm64 recover-semantic-quarantine \
  --repo /absolute/path/to/repository \
  --backup '/exact/managed/recovery-backup' \
  --dry-run --json
```

unloaded、worker/active execution/pending request 0、exact mismatch reason、digest、revision、Issue件数、`next_backup`を確認してから`--dry-run`を`--confirm-exact-backup`へ置き換える。元snapshotへ戻ったら新binaryの`status`や`doctor`を先に実行せず、全repositoryを停止して`migrate --json` / `migrate --apply --json`を実行する。

Issue lifecycle API不一致だけを理由とするmarkerなら、同じ停止条件で専用commandを使う。

```sh
/absolute/verified/agent-loop_Darwin_arm64 recover-lifecycle-quarantine \
  --repo /absolute/path/to/repository \
  --backup '/exact/managed/recovery-backup' \
  --dry-run --json
```

source/target lifecycle version、state/event/transaction digest、revision、Issue/worker/execution/request件数を確認し、`--confirm-exact-backup`で1回だけ復元する。prepared transactionは検証後にbyte-exactで復元され、次の正規readで完了する。marker記載外のbackup、nested marker、active worker、loaded runtimeでは実行しない。

初回導入とrelease前の実Mac E2Eでは、test repositoryと検証済みstable releaseを使い、次を記録する。

1. login後にdelivery LaunchAgentがstable releaseを検出する。
2. worker実行中に`delivery apply`し、workerへSIGTERM/SIGKILLが送られずcheckpoint後に進む。
3. update成功後、二度目のdoctor/soakまで新規Issueがclaimされない。
4. doctor failure fixtureでprevious installへrollbackしてから通常処理が再開する。
5. `downloaded`、`draining`、`applying`、`validating`各phaseでdelivery processを停止し、login後のreconcileが旧版継続、安全な再開、rollbackのいずれか一つへ収束する。

実施日時、tester、Mac model、macOS、current/desired commit、transaction resultを記録し、未実施項目を成功と扱わない。

controllerを使えない復旧時だけ、次の手動手順を使う。

1. 全loopを停止し、[backup](#backup)を取る。
2. `gh release download`、checksum、attestation、`version --json`でartifactを検証する。
3. 新しいbinaryから`update --json`を実行し、返されたbackup pathを記録する。
4. `update`が元々稼働していたLaunchAgentを再開し、全登録repositoryのplistを再生成したことを出力で確認する。
5. `doctor`と各repositoryの`status`を確認する。

`update`が`schema_migration_required: true`を返すreleaseでは全loopを停止したままにし、[永続schema migration runbook](migration.md)に従って`migrate --apply`を完了してからdoctorとstartへ進む。

更新後に失敗した場合は`update`が返したbackupを`rollback --backup`へ指定する。binaryだけを戻して新schemaのstateを読み込ませない。旧binaryと互換性のあるstate/config backupへ組で戻し、doctor、start、statusの順で確認する。復旧にstateの削除や未定義のschema変換が必要なら自動実行せずescalationする。

## 8. 登録解除とuninstall

repositoryを管理対象から外す前に、running Issueと未回答request、未commit worktreeを確認する。

```sh
agent-loop status --repo /absolute/path/to/repository --json
agent-loop stop --repo /absolute/path/to/repository --json
agent-loop unregister --repo /absolute/path/to/repository --json
```

`unregister`はregistryとplistを外すがstateとworktreeを保持する。全repositoryを停止・登録解除し、binaryとSkillも削除するときだけ次を実行する。

```sh
agent-loop uninstall --json
```

`uninstall`もstateとworktreeを保持する。worktreeの整理は直接削除せず、[Worktree保持・cleanup・purge runbook](worktree-lifecycle.md)に従う。通常は`cleanup --json`でpreviewし、対象と理由をレビューしてからloop停止中に`cleanup --apply`する。dirty、未push、open PR、未回答requestを含む対象の`purge`はbackupと完全一致確認tokenを必須とする。

## 9. log収集とescalation

raw stateやlogにはrepository内容や質問文が含まれ得る。共有前にcredential、個人情報、機密コードを確認し、必要部分だけを引用する。tokenらしい値を見つけた場合は共有より先にloop停止とcredential失効を行う。

最低限、次を収集する。

```sh
agent-loop --version
sw_vers
uname -m
gh --version
codex --version
agent-loop status --repo /absolute/path/to/repository --json
agent-loop doctor --repo /absolute/path/to/repository --json
agent-loop logs --repo /absolute/path/to/repository --stderr
```

次の場合は推測で再試行せずescalationする。

- state/event/registryが破損し、backupとGitHub上の状態から一意に復元できない
- worktreeに未commit変更があり、GitHub上のPR・branchと不整合である
- 同じIssueの二重claim、異なる回答の競合、誤ったrepositoryへの操作が疑われる
- token漏えい、branch protection bypass、想定外の外部書き込みが疑われる
- update/rollback後にschemaまたはbinary互換性を確認できない

### 障害報告template

```markdown
## 概要
- 発生日時（timezone付き）:
- 対象Mac / macOSユーザー:
- 対象repository / Issue:
- スマートフォン、Mac上のCodex、LaunchAgentのどこから観測したか:

## 期待した状態と実際の状態
- 期待:
- 実際:
- 最初に失敗した操作:

## 診断
- agent-loop / gh / codex / macOS version:
- statusのsupervisor・Issue状態:
- doctorのschema_versionと失敗code:
- 直近eventのtypeと時刻:
- stderrの関連行（secret削除済み）:

## 変更と復旧
- 発生直前のconfig・binary・認証・OS変更:
- 実施済みのread-only確認:
- stop / backupの有無と保管場所:
- 未commit worktreeの有無:

## 秘密情報
- token、API key、raw credentialを含まないことを確認済み: はい / いいえ
```

## 10. 実機受け入れチェック

新しいMac miniへ導入するときは、次を順に記録する。実repositoryで初めて試さず、branch protectionを設定したtest repositoryから始める。

- [ ] 初期状態からbuild、install、認証確認、label preview/apply、register、doctorを完了した
- [ ] AC Powerの`sleep`が`0`であることを確認した
- [x] ChatGPTモバイルアプリからMac miniへのCodex Remote接続を確認した（2026-08-16）
- [ ] スマートフォンからstart、status、watch、stopを実行できた
- [ ] test Issueをclaimし、Codex workerがworktreeで実行され、draft PR作成、CI成功後のReady化、manifestどおりの手動または自動merge待ちへ到達した
- [ ] 意図的な`needs_input`をスマートフォンで受け、request IDを保ってanswerし、workerがresumeした
- [ ] desktop app/taskを終了してもloopが継続し、新しいtaskから監視へ再接続できた
- [ ] network一時切断後、eventの取りこぼしがあってもreconciliationで最新snapshotへ復旧した
- [ ] auth失効をtest credentialで再現し、blockedの検出、再認証、restartを確認した
- [ ] 容量制限されたtest volumeまたはfixtureで書き込み失敗を再現し、停止・backup・復旧手順を確認した
- [ ] backupからtest環境をrestoreし、doctor後に再開できた
- [ ] unregisterとuninstall後もstate/worktreeが保持されることを確認した

認証失効、容量不足は稼働中の本番repositoryでは試さない。実施日時、tester、commit、CLI version、Mac model、macOS version、結果、関連Issueを記録し、未確認項目を「成功」と扱わない。

### 10.1 端末ライフサイクルの運用時確認

display off、screen lock、logout、OS再起動はMac miniの通常運用では発生頻度が低く、意図的な中断を伴うため、導入やmilestone完了を妨げるTODOにはしない。実際に発生したとき、または計画保守時に次を確認して運用記録へ残す。

- display offまたはscreen lock中、Macがawakeでユーザーsessionが維持されていればLaunchAgentが継続する
- logoutするとLaunchAgentとCodex Remoteが停止する
- OS再起動後、FileVault unlockとlogin、Codex Remote再接続、LaunchAgentのstatus確認まで復旧する

異常があった場合だけ、実施日時、tester、CLI version、Mac model、macOS version、観測結果を添えてIssue化する。

## 11. 公式仕様への依存

- [OpenAI: Remote connections](https://learn.chatgpt.com/docs/remote-connections.md)
- [OpenAI: Codex authentication](https://learn.chatgpt.com/docs/auth.md)
- [Apple: Service Management](https://developer.apple.com/documentation/servicemanagement)
- [Apple: Automatic login and FileVault](https://support.apple.com/en-gb/102316)
- [GitHub CLI: gh auth status](https://cli.github.com/manual/gh_auth_status)
- [GitHub CLI: gh auth login](https://cli.github.com/manual/gh_auth_login)
- [Apple: Set sleep and wake settings for your Mac](https://support.apple.com/en-gb/guide/mac-help/mchle41a6ccd/mac)

## 12. Webhook brokerとreverse proxy

Webhook modeはopt-inである。agent-loopはpublic endpoint、TLS certificate、DNS、reverse proxy providerのaccount・credential・daemonを作成または変更しない。運用者が管理する公開HTTPS URLから、同じMacのliteral loopback listener `http://127.0.0.1:8787/github/webhook`（または`http://[::1]:8787/github/webhook`）へraw request bodyとGitHub headerを変更せず転送する。

reverse proxyは次のcontractを満たすものを選ぶ。

- public側でTLS 1.2以上を終端し、certificate hostnameを検証可能にする
- upstreamはloopbackだけとし、LAN/public interfaceやUnix socketを公開先にしない
- `X-Hub-Signature-256`、`X-GitHub-Delivery`、`X-GitHub-Event`とraw bodyを保持する
- body sizeと接続timeoutをbroker設定以下に制限し、request buffering中のdisk permissionも限定する
- providerが対応する場合はGitHub公式Webhook送信元CIDRをallowlistし、更新を監視する。ただしCIDR制限をHMACの代用にしない
- provider固有のaccess token、tunnel credential、daemon設定をrepositoryやagent-loop stateへ保存しない

### Cloudflare Tunnelでの構成例

Cloudflare Tunnelは上のcontractのうち送信元CIDR allowlistまで満たせるため、inbound portを開けずに構成できる。Tunnel本体、DNS、custom rule 1本はいずれも無料プランの範囲で、帯域課金もない。ここに書くのは一例であり、agent-loopはこのproviderに依存しない。

Cloudflareアカウント、対象ドメインのCloudflareへの委任、`cloudflared tunnel login`のbrowser認証、`sudo cloudflared service install`、WAF custom ruleの作成は運用者が行う。agent-loopはこれらを作成も変更もしない。

```sh
brew install cloudflared
cloudflared tunnel login
cloudflared tunnel create agent-loop-hooks
cloudflared tunnel route dns agent-loop-hooks hooks.example.invalid
```

`~/.cloudflared/config.yml`ではpathを`/github/webhook`だけに限定し、それ以外をloopbackへ到達させない。upstreamはliteral loopbackにする。

```yaml
tunnel: <TUNNEL-ID>
credentials-file: /Users/example/.cloudflared/<TUNNEL-ID>.json

ingress:
  - hostname: hooks.example.invalid
    path: ^/github/webhook$
    service: http://127.0.0.1:8787
    originRequest:
      connectTimeout: 5s
  - service: http_status:404
```

送信元CIDRは公式APIから取得し、WAF custom ruleでGitHub以外をBlockする。CIDRは6件程度でruleの式へ直接書けるため、IP Listは不要である。GitHubはchallengeを解けないのでManaged Challengeは選ばない。

```sh
gh api meta --jq '.hooks[]'
```

```text
http.host eq "hooks.example.invalid"
and not ip.src in {192.30.252.0/22 185.199.108.0/22 140.82.112.0/20 143.55.64.0/20 2a0a:a440::/29 2606:50c0::/32}
```

このCIDRは変更されるため、差分監視を運用に含める。記録値は`.github/github-hooks-cidr.txt`にあり、`scripts/check-hooks-cidr.sh`が現在値と比較して差分があれば`exit 1`する。`.github/workflows/hooks-cidr.yml`が毎週これを実行し、差分を検出したらIssueを作成する。同じ内容のopen Issueがある間は重複作成しない。

このIssueにready labelは付かないため、loopは自動で着手しない。reverse proxyのallowlistを更新し、記録値も同じcommitで更新する。CIDR制限はHMACの代用にしないため、更新までの間もsignature検証は有効である。

```sh
./scripts/check-hooks-cidr.sh
```

無料プランでは次の2点がcontractに対して不足する。いずれも受け入れるか、上位プランを選ぶかを明示的に判断する。

- request body sizeによる制限は`http.request.body.size`がEnterprise専用のため設定できない。brokerの`max_body_bytes`が最終防衛線になる。
- WAFのLogアクションが使えないため、allowlistをdry-runで検証できない。Blockのまま投入し、GitHubのRecent Deliveriesが202を返すことで確認する。Blockされたrequestは無料プランでもSecurity Events（保持24時間、sampled）に残る。

`cloudflared`はLaunchDaemon、brokerはLaunchAgentであるため、login前のdeliveryは5xxになる。取りこぼしはredeliveryとsafety sweepが回収する。

### secretとrepository設定

secret fileはrepository外へowner-onlyで作る。値をshell historyへ残さない組織のsecret provisioning手段を使い、最終状態だけ確認する。

配置先はhome配下ではなくhostに依存しない固定pathにする。`.agent-loop.yaml`はrepositoryで共有され、`secret_source.file`は`~`も環境変数展開も受け付けない絶対pathのみを許す。home配下を指定すると、public repositoryではhostのuser名が公開され、他hostでは解決できない設定になる。directoryとfileはbrokerを実行するuserの所有にする。brokerはLaunchAgentとしてそのuser権限で動くため、root所有0600のfileは読めない。

```sh
sudo mkdir -p /usr/local/etc/codex-issue-loop
sudo chown "$(id -un):$(id -gn)" /usr/local/etc/codex-issue-loop
chmod 700 /usr/local/etc/codex-issue-loop
umask 077
touch /usr/local/etc/codex-issue-loop/owner-repository.webhook
chmod 600 /usr/local/etc/codex-issue-loop/owner-repository.webhook
```

対象repositoryのnumeric repository IDとGitHub App installation IDをGitHubの管理画面または認証済みAPIで確認し、次をdefault branchの`.agent-loop.yaml`へ追加する。`public_url_identifier`は監査用の非secret識別子であり、query tokenを含むURLを書かない。LaunchAgent運用では環境変数がログインlaunchdへ安全に注入されていることを保証しにくいため、通常は0600 file sourceを使う。safety sweepやHTTP limitはbrokerの内部運用値なのでrepository設定には記載しない。

```yaml
github:
  repo: owner/repository
  repository_id: 123456789

webhook:
  mode: webhook
  listener_address: 127.0.0.1:8787
  public_url_identifier: hooks.example.invalid/agent-loop/owner-repository
  secret_source:
    file: /usr/local/etc/codex-issue-loop/owner-repository.webhook
  installation_ids: [987654]
  allow_repository_webhook: false
```

GitHub Appまたはrepository webhookのpayload URLは公開HTTPS URLの`/github/webhook`へ合わせ、content typeは`application/json`、SSL verificationは有効、secretは上のcredentialと同一にする。最低限`issues`、`issue_comment`、`pull_request`、`check_run`、`status`を購読し、Actionsの状態だけで必要な場合に`workflow_run`を追加する。登録後の`ping`が202になり、次を確認してからready labelを使う。

GitHub Appでは`installation_ids`をallowlistとして維持する。installationを含まないclassic repository webhookを使う場合だけ、`installation_ids: []`と`allow_repository_webhook: true`を明示する。このopt-inはrepository ID/full nameとHMAC検証を緩和しない。

classic repository webhookをCLIで登録する場合も、secretをargvへ展開しない。owner-onlyの一時fileへrequest bodyを書き、`--input`で渡して直ちに削除する。

```sh
umask 077
# hook.json へ config.url、config.secret、config.content_type、events を書く
gh api repos/OWNER/REPO/hooks --method POST --input hook.json
rm -P hook.json
gh api repos/OWNER/REPO/hooks --jq '.[] | {id, url: .config.url, active}'
```

`ping`の結果は`gh api repos/OWNER/REPO/hooks/<id>/deliveries`でも確認でき、GitHub UIのRecent Deliveriesと同じstatusを返す。

```sh
agent-loop register --repo /absolute/path/to/repository --json
agent-loop doctor --repo /absolute/path/to/repository --json
agent-loop start --repo /absolute/path/to/repository --json
agent-loop status --repo /absolute/path/to/repository --json
```

`status --json`の`broker`でmode、listener、last accepted delivery、queue depth、reject/duplicate countを確認し、`repository_safety_sweep`でlast successful、HTTP status、304/200 count、ETagを確認する。secret、signature、Authorization、payloadは表示されない。listenerへの直接疎通はGitHubのredeliveryまたは署名済みの管理fixtureだけで行い、secretをcommand lineへ展開しない。

### secret rotation

1. 新secretを別の0600 fileへ配置する。
2. 現在のsourceを`previous_secret_source`、新fileを`secret_source`に設定し、`register`、`doctor`、`restart`を行う。
3. GitHub側を新secretへ切り替え、`ping`またはredeliveryがacceptedになることを確認する。
4. `previous_secret_source`を削除して再度`register`、`doctor`、`restart`し、旧fileを組織のcredential廃棄手順で削除する。

rotation期間を長期化しない。どちらのsecretが一致したかはlogへ出さない。

### proxy停止・broker crash・redelivery

proxy停止中のdeliveryは復旧後にGitHub UIからredeliveryできる。同じdelivery IDはdurable inboxでdedupeされる。redeliveryできない取りこぼしは15分のjitter付き条件付きREST sweepが収束させ、変更なし304を正常として記録する。brokerを停止してもrepo別supervisor、worker、state、worktree、未処理mailboxを削除しない。Mac再起動後はFileVault unlockとloginの後、broker LaunchAgentと各repository statusを確認する。

### pollingへのrollback

proxyまたはWebhook設定を安全に復旧できない場合は、対象repositoryを停止し、`webhook.mode: polling`へ明示的に戻して再登録する。

```sh
agent-loop stop --repo /absolute/path/to/repository --json
# .agent-loop.yaml の webhook.mode を polling へ変更
agent-loop register --repo /absolute/path/to/repository --json
agent-loop doctor --repo /absolute/path/to/repository --json
agent-loop start --repo /absolute/path/to/repository --json
```

他にWebhook repositoryが残っている間、共有brokerは停止しない。最後のWebhook repositoryを`unregister`した場合だけbroker LaunchAgentが削除される。rollbackはdurable inbox、repo state、worktreeを消さず、provider daemonやcredentialをagent-loopから変更しない。
