# ADR-0001: macOS実行モデルはユーザーLaunchAgentを継続する

- Status: Accepted
- Date: 2026-08-16
- Decision owners: codex-issue-loop maintainers

## Context

`agent-loop`はGitHub Issueを取得し、`gh`、`git`、`codex exec`を長時間実行する。現在はログイン中のmacOSユーザーに属するLaunchAgentとして動き、そのユーザーのHOME、Codex/GitHub認証、repository、worktreeを同じ所有権境界で利用する。

より無人性を高める案として、LaunchDaemon、専用ユーザー、自動ログインを検討した。ただしMac miniでlogoutや再起動は通常運用では低頻度であり、今回の可用性要件は「電源投入からloginなしで必ず処理を再開すること」ではない。display offやscreen lockはlogoutではなく、system sleepを防いでログインsessionを維持すれば現行モデルで継続できる。

Appleの[Service Management](https://developer.apple.com/documentation/servicemanagement)では、LaunchAgentはログイン中ユーザーのために動き、LaunchDaemonはrootとしてlogin前にも動けるsystem-wide processと区別される。FileVaultが有効なMacでは[自動ログインが無効](https://support.apple.com/en-gb/102316)になり、disk unlockとloginなしにユーザーHOMEを利用する設計にはできない。

Codex CLIはbrowserを使えない環境で[`codex login --device-auth`](https://learn.chatgpt.com/docs/auth#login-on-headless-devices)を利用でき、`codex exec`自体は非対話実行できる。一方、保存credentialは`CODEX_HOME`配下のfileまたはOS keychainに属し、GitHub credentialも同様に実行ユーザーの境界を持つ。CLIがheadlessで動くことと、既存ユーザーcredentialをsystem daemonから安全に利用できることは同義ではない。Codex Remoteはさらに、ログイン中sessionのdesktop appと接続を必要とする。

## Options considered

| Option | reboot/logout | HOME・認証・所有権 | Security | Operability | Decision |
| --- | --- | --- | --- | --- | --- |
| 現行ユーザーのLaunchAgent | login後に自動load、logout中は停止 | 現行HOME、Keychain、repository所有者を維持 | 権限追加なし | 現行runbookと診断を維持 | 採用 |
| 専用標準ユーザーのLaunchAgent | 専用ユーザーのlogin後に自動load | 専用HOMEへCodex/GitHub認証とrepositoryを分離 | blast radiusを縮小可能 | 初回login、Remote、credential rotationの運用が増える | 任意のhardeningとして許容 |
| LaunchDaemon | login前から起動可能。ただしFileVault unlock前はdata volume/HOMEを利用できない | rootまたは指定service userへ認証・repository・worktreeを移行する必要 | system-wide credential複製、root process、ownership混在のrisk | GUI/Keychain/Remoteと別の復旧経路が必要 | 不採用 |
| 自動ログイン + LaunchAgent | reboot後にuser sessionを開始可能 | 現行境界を維持 | 物理アクセス時の保護を弱め、FileVaultと両立しない | logout後は再loginが必要。MDM policyとも競合し得る | 不採用 |
| LaunchDaemon + user helper | daemonはlogin前に待機、helperはlogin後に実行 | IPC、二重状態、二つのcredential境界が必要 | attack surfaceと権限設計が増える | 現在の可用性要件に対して過剰 | 不採用 |

## Decision

repository単位のユーザーLaunchAgentを正式な実行モデルとして継続する。

- 通常運用ではMac miniをawakeに保ち、運用ユーザーのlogin sessionを維持する。
- FileVaultを維持し、自動ログインを要求・設定しない。
- LaunchDaemon、root実行、system-wide credential storeを実装しない。
- Codex/GitHub credentialを別ユーザーやrootへコピーしない。
- 日常利用と分離した標準macOSユーザーはhardeningとして推奨するが、daemon化とは扱わない。
- display off、screen lock、logout、OS再起動はmilestoneの受け入れTODOにしない。実際の発生時または計画保守時にrunbookどおり確認し、異常があった場合だけIssue化する。
- 再起動後はFileVault unlockと運用ユーザーloginを人が行う。login後はLaunchAgentがloadされ、supervisorが永続snapshot・event・GitHubをreconcileする。
- スマートフォン操作を再開する場合はdesktop appとCodex Remoteも再接続する。loop本体の再開とRemote経路の再開を別々に確認する。

## Consequences

### Positive

- 現行ユーザーのHOME、Keychain、Codex/GitHub認証、repository、worktreeの所有権を一つの境界に保てる。
- root権限、system-wide token、credential複製、daemon/helper IPCを追加しない。
- `install`、`register`、`doctor`、`start`、`update`、`rollback`の既存契約を維持できる。
- FileVaultと物理アクセス保護を可用性のために弱めない。

### Negative

- logout中はIssue処理とCodex Remoteが停止する。
- 電源断・OS再起動後はFileVault unlockとloginまで自動復旧しない。
- login後もdesktop appまたはRemote接続に問題があればスマートフォン操作は復旧しない。ただしLaunchAgentのloopは独立して再開できる。

## Operational verification

低頻度の端末ライフサイクルは実際の発生時または計画保守時に記録する。

1. display offまたはscreen lock中に、system sleepせずLaunchAgentが継続していること。
2. logoutでLaunchAgentとRemoteが停止すること。
3. reboot後にFileVault unlockとloginを行い、LaunchAgentがloadedになって永続状態からreconcileすること。
4. desktop appを起動し、Remote接続を再確認できること。

未実施項目を失敗やmilestone blockerとは扱わない。観測が期待と異なる場合だけ、日時、macOS/CLI version、status、doctor、関連logを添えてIssue化する。

## Revisit triggers

次のいずれかが要件になった場合は新しいADRで再検討する。

- login介入なしのreboot復旧が明示的なavailability SLOになる。
- 組織管理された専用service account、短命credential、MDM、remote unlockの運用基盤が用意される。
- Codex/GitHubがsystem daemon向けのmachine identityとcredential brokerを正式提供する。
- repositoryとworktreeを専用service userへ移行し、Remoteを別経路に分離する費用を受け入れる。

このADRを変更せずにLaunchDaemonや自動ログインを追加してはならない。
