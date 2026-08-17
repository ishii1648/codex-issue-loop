# Mac mini常駐運用runbook

最終確認日: 2026-08-16

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

対象repositoryのrootに`.agent-loop.yaml`を置き、[設定例](../.agent-loop.example.yaml)をもとにrepository名、ラベル、base branch、sandbox、timeoutを確認する。Go publisher整形を使うrepositoryでは`formatters.go.enabled: true`を明示し、`gofmt`を利用できるPATHでregisterする。任意formatter commandや追加引数は指定できない。GitHub Issue本文や設定へtokenを保存しない。

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

記録後、同じrequest IDがansweredになったことをstatusで確認し、1回のwatchへ戻る。古いrequestや異なる二重回答はconflictとして扱い、推測で別requestへ転用しない。

### 停止

対象repositoryを確認してから、監視taskへ「状態とworktreeを残してloopを停止して」と依頼する。

```sh
agent-loop stop --repo /absolute/path/to/repository --json
agent-loop status --repo /absolute/path/to/repository --json
```

停止はstate、event、Issue worktree、未commit変更を削除しない。

## 5. 停止・障害からの復旧

復旧の基本順序は、変更を増やさずに`status`、`doctor`、logを集め、原因を1つずつ直し、`restart`することである。stateやworktreeを最初に削除しない。

```sh
agent-loop status --repo /absolute/path/to/repository --json
agent-loop doctor --repo /absolute/path/to/repository --json
agent-loop logs --repo /absolute/path/to/repository
agent-loop logs --repo /absolute/path/to/repository --stderr
```

`logs`は保持中のgzip世代と現行`supervisor.log`を古い順に連結して表示する。`--stderr`はlaunchdが捕捉した起動失敗を同様に表示する。既定のrotation・保持値は`.agent-loop.yaml`の`logs`で変更できるが、容量reserveを小さくしすぎない。

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

PR conflictは検出時点では`resolving_conflict`となり、base SHAごとの自動復旧とCI再確認を行う。budget超過等で最終`blocked`になった場合は、`status --json`の`conflict_recovery`にある試行履歴、base SHA、競合file、最終理由を確認する。worktree・branch・open PRの対応と停止原因を直した後だけ、次の明示操作を使う。

```sh
agent-loop retry --repo /absolute/path/to/repository --issue 123 --json
agent-loop status --repo /absolute/path/to/repository --json
```

`retry`は無関係なblocked原因を初期化せず、保存済みbranchとPRが一致する場合だけ`resolving_conflict`へ戻す。新しいbranch/PRやforce pushは作らない。

workerが返した環境起因`blocked`は`status --json`の`blocked_cause`が`origin=worker`、`kind=environment`、`resumable=true`であることを確認する。導入前のworker blockだけは`failure_kind=issue`とsupervisor生成の`worker blocked` errorからCLIがprovenanceを正規化し、失われたleaseを`repo:*`として再予約する。外部前提を修復し、対象Issueにactive workerがなく、保存worktree・branch・run・resource leaseとGitHub label/PRが一致するときだけ次を使う。v0.6.0以前のlegacy recordでは`lease`または`lease.base_sha`がないことがあるが、stateを手編集しない。

```sh
agent-loop resume-blocked --repo /absolute/path/to/repository --issue 123 --confirm-prerequisite-resolved --json
```

この操作はconfigured base branchのremote-tracking commitを検証し、leaseを補う場合は非空の`base_sha`も同じtransactionで保存する。`resolve configured base branch`で拒否された場合はGitHub labelとdurable stateは未変更なので、`git -C <saved-worktree> fetch origin <base-branch>`で正しいremote-tracking commitを取得し、`status --json`でblocked状態を再確認して同じコマンドを再実行する。既存の非空`base_sha`は上書きしない。

resumeはdirty changes、branch、worktree、session/continuation、回答、resource metadataを削除せず、`environment_resume_requested` eventと冪等GitHub markerを残す。成功後は`status --json`で`lease.base_sha`が非空であることを確認する。GitHub sync失敗時もstateを手編集せず、network復旧後にsupervisorを起動するか同じ`resume-blocked`を再実行して収束させる。旧版の競合で`status=blocked`、`environment_resume.status=requested|github_synced`、`lease=null`となった場合も、修正版の同じコマンドがeventに保存したbase SHAとGitHub/worktree/run/PRを再検証し、競合のない`repo:*` leaseを再予約する。event historyからbase SHAを回復できない場合や、`conflict_recovery`、手動`blocked`/`do-not-automate`、security block、active worker、unanswered request、running/completed、closed-without-merge、worktree/branch/PR不整合がある場合は修復・再開せず原因別runbookへ戻る。

v0.6.0から修正版へ更新する場合は、[Release artifact検証](release.md#artifact検証)に従ってchecksum、GitHub artifact attestation、`version --json`のtag/commitを確認した新binaryだけを使い、そのbinaryから`update --json`を実行する。update後に`doctor --repo <repository> --json`を通してから上記resumeを実行し、返されたbackup pathはpublication完了まで保持する。

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
- 設定で`git.worktree_root`を変更している場合はそのdirectory
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

release artifactの検証と更新方針は[Release・install・update方針](release.md)を正本とする。

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

secret fileはrepository外へowner-onlyで作る。値をshell historyへ残さない組織のsecret provisioning手段を使い、最終状態だけ確認する。

```sh
install -d -m 700 "$HOME/Library/Application Support/codex-issue-loop/credentials"
umask 077
touch "$HOME/Library/Application Support/codex-issue-loop/credentials/owner-repository.webhook"
chmod 600 "$HOME/Library/Application Support/codex-issue-loop/credentials/owner-repository.webhook"
```

対象repositoryのnumeric repository IDとGitHub App installation IDをGitHubの管理画面または認証済みAPIで確認し、次をdefault branchの`.agent-loop.yaml`へ追加する。`public_url_identifier`は監査用の非secret識別子であり、query tokenを含むURLを書かない。LaunchAgent運用では環境変数がログインlaunchdへ安全に注入されていることを保証しにくいため、通常は0600 file sourceを使う。

```yaml
github:
  repo: owner/repository
  repository_id: 123456789

webhook:
  mode: webhook
  listener_address: 127.0.0.1:8787
  public_url_identifier: hooks.example.invalid/agent-loop/owner-repository
  secret_source:
    file: /Users/example/Library/Application Support/codex-issue-loop/credentials/owner-repository.webhook
  installation_ids: [987654]
  allow_repository_webhook: false
  safety_sweep_interval: 15m
  safety_sweep_jitter: 10%
  max_body_bytes: 2097152
  read_timeout: 10s
  read_header_timeout: 5s
  idle_timeout: 30s
  max_concurrent: 16
```

GitHub Appまたはrepository webhookのpayload URLは公開HTTPS URLの`/github/webhook`へ合わせ、content typeは`application/json`、SSL verificationは有効、secretは上のcredentialと同一にする。最低限`issues`、`issue_comment`、`pull_request`、`check_run`、`status`を購読し、Actionsの状態だけで必要な場合に`workflow_run`を追加する。登録後の`ping`が202になり、次を確認してからready labelを使う。

GitHub Appでは`installation_ids`をallowlistとして維持する。installationを含まないclassic repository webhookを使う場合だけ、`installation_ids: []`と`allow_repository_webhook: true`を明示する。このopt-inはrepository ID/full nameとHMAC検証を緩和しない。

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
