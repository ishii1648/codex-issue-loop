# doctor診断・復旧runbook

最終確認日: 2026-08-16

`agent-loop doctor --repo PATH`はhostと指定repositoryを診断する。`--repo`を省略するとhostとregistry内の全repositoryを診断する。診断はread-onlyであり、state、設定、label、認証、macOS設定を自動修復しない。

```sh
agent-loop doctor --repo /absolute/path/to/repository
agent-loop doctor --repo /absolute/path/to/repository --json
agent-loop doctor --json
```

## JSON契約

JSON consumerは`schema_version: 1`を確認し、`diagnostics[].code`と`ok`で分岐する。翻訳・改善され得る`summary`や`detail`の文字列を分岐条件にしない。

各remediationには`kind`、`summary`、任意の`command`または`settings`、`automatic`、`destructive`が含まれる。doctorが提示する修復はすべて`automatic: false`であり、表示しただけでは実行しない。未知のschema versionやdiagnostic codeは推測で処理せず、CLIとSkillのversion不一致として利用者へ提示する。

## 主な失敗code

| code | 意味 | 最初の確認・復旧 |
| --- | --- | --- |
| `DEPENDENCY_<NAME>_MISSING` | 必須commandがPATHにない | commandをinstallまたはPATHへ追加 |
| `INSTALL_NOT_PRESENT` | source treeから実行中でinstallなし | 必要なら検証済みreleaseをinstall |
| `INSTALL_MANIFEST_MISSING` / `INSTALL_MANIFEST_INVALID` | install metadataがない・破損 | install directoryをbackupして再install |
| `INSTALL_VERSION_MISMATCH` | binary、Skill、manifestのversion/checksum不一致 | 検証済みreleaseからupdateまたはrollback |
| `INSTALL_SCHEMA_INCOMPATIBLE` | installed binaryが想定する永続schemaと不一致 | binary updateとschema migrationを組で実行 |
| `INSTALL_VERSION_CONSISTENT` | binary、Skill、manifestが一致 | 対応不要 |
| `SCHEMA_MIGRATION_REQUIRED` | v1のconfig・registry・state・event等が残る | 全loop停止後にpreviewを確認して`migrate --apply` |
| `SCHEMA_VERSION_UNSUPPORTED` | v3以上など対応外schema | fileを変更せず対応binary・migration手順を確認 |
| `SCHEMA_INSPECTION_FAILED` | schema version自体を安全に読み取れない | fileを削除せずbackupして調査 |
| `SCHEMA_VERSION_SUPPORTED` | 全永続schemaがv2 | 対応不要 |
| `GITHUB_AUTH_INVALID` | `gh auth status`が失敗 | `gh auth login`後に対象repository権限を確認 |
| `CODEX_AUTH_INVALID` | `codex login status`が失敗 | `codex login`、headless時は`codex login --device-auth` |
| `GH_CLI_INCOMPATIBLE` / `CODEX_CLI_INCOMPATIBLE` | versionまたはcapability不足 | 対応versionへ更新しdoctorを再実行 |
| `MACOS_SLEEP_ENABLED` / `MACOS_SLEEP_STATUS_UNKNOWN` | AC電源時のsleepが有効または判定不能 | System Settings > Energyで「Prevent automatic sleeping when the display is off」を有効化 |
| `REGISTRY_CORRUPT` | registryを解釈不能 | 元fileを削除せず退避・確認し、repositoryを再登録 |
| `CONFIG_INVALID` | `.agent-loop.yaml`が無効 | 表示されたpathとvalidation errorを修正 |
| `NOTIFICATION_CREDENTIAL_MISSING` | 外部pushが有効だが管理tokenがない | `notification-token --token-file -`で標準入力から保存 |
| `NOTIFICATION_CREDENTIAL_UNSAFE` | token fileがregular fileでない、0600でない、または内容が無効 | symlinkやpermissionを確認し専用commandで保存し直す |
| `NOTIFICATION_CREDENTIAL_VALID` / `NOTIFICATIONS_DISABLED` | tokenを安全に読める、または外部pushが無効 | 対応不要 |
| `REGISTRATION_MISSING` | repositoryが未登録 | `agent-loop register --repo PATH` |
| `REGISTERED_BINARY_MISSING` | 登録時の絶対command pathが移動 | install/update後に同じrepositoryを再register |
| `LAUNCH_AGENT_MISSING` / `LAUNCH_AGENT_UNREADABLE` | plistがない、または読めない | 再register、所有者・permission確認 |
| `GITHUB_REPOSITORY_INACCESSIBLE` | repository参照権限または認証不足 | `gh auth status`とtoken/GitHub App権限を確認 |
| `GITHUB_LABELS_MISSING` | 必須label不足 | `bootstrap-labels`をpreviewし、確認後に`--apply` |
| `STATE_MISSING` / `STATE_UNREADABLE` | durable stateがない、または読めない | 再register、所有者・permission確認 |
| `STATE_CORRUPT` / `EVENT_LOG_INVALID` | stateまたはevent logの破損・不整合 | stopし、state directory全体を削除せずbackupして調査 |
| `LOG_UNREADABLE` | supervisor logを読めない | logの所有者・permission確認 |
| `SUPERVISOR_BLOCKED` | supervisor全体障害で停止 | status、stderr log、直近eventを確認し、原因修復後にrestart |
| `SUPERVISOR_STOPPED` | supervisorが停止中 | 意図した停止か確認後、必要ならstart |

`SUPERVISOR_BLOCKED`と`SUPERVISOR_STOPPED`のdetailは、snapshotの状態・message、最後に解釈できたeventのtype/time、直近supervisor log行を相関して表示する。token値は表示せず、stateとlogで既にredactされた情報だけを使用する。

## 認証とmacOS設定の根拠

- Codex CLIは`codex login status`で認証方式を確認し、`codex login`でbrowser flow、headless環境では`codex login --device-auth`を利用できる。[OpenAI Authentication](https://learn.chatgpt.com/docs/auth.md)
- GitHub CLIは`gh auth status`でactive accountの認証状態を検証し、問題があれば非0で終了する。再認証は`gh auth login`を使う。[gh auth status](https://cli.github.com/manual/gh_auth_status)、[gh auth login](https://cli.github.com/manual/gh_auth_login)
- Mac miniではSystem SettingsのEnergyから「Prevent automatic sleeping when the display is off」を有効にする。Appleは消費電力が増える点も案内している。[Apple: Set sleep and wake settings for your Mac](https://support.apple.com/en-gb/guide/mac-help/mchle41a6ccd/mac)
