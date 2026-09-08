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
| `USER_RULE_CODEX_MISSING` / `USER_RULE_CLAUDE_MISSING` | user-scope Issue作成ruleが未導入 | `agent-loop init --json`を確認後、明示的に`--apply` |
| `USER_RULE_CODEX_OUTDATED` / `USER_RULE_CLAUDE_OUTDATED` | agent-loop管理ruleが旧version | 対象agentの`init --json`を確認後、明示的に`--apply` |
| `USER_RULE_CODEX_CONFLICT` / `USER_RULE_CLAUDE_CONFLICT` | marker不整合または所有できないfile | `agent-loop init --json`のpathとdetailを確認し、手動で競合を解消 |
| `SCHEMA_MIGRATION_REQUIRED` | v1のconfig・registry・state・event等が残る | 全loop停止後にpreviewを確認して`migrate --apply` |
| `SCHEMA_VERSION_UNSUPPORTED` | v2以下、v5以上など対応外schema | fileを変更せず対応binary・migration手順を確認 |
| `SCHEMA_INSPECTION_FAILED` | schema version自体を安全に読み取れない | fileを削除せずbackupして調査 |
| `SCHEMA_VERSION_SUPPORTED` | 全永続schemaがv2 | 対応不要 |
| `GITHUB_AUTH_INVALID` | `gh auth status`が失敗 | `gh auth login`後に対象repository権限を確認 |
| `CODEX_AUTH_INVALID` | `codex login status`が失敗 | `codex login`、headless時は`codex login --device-auth` |
| `GH_CLI_INCOMPATIBLE` / `CODEX_CLI_INCOMPATIBLE` | versionまたはcapability不足 | 対応versionへ更新しdoctorを再実行 |
| `MACOS_SLEEP_ENABLED` / `MACOS_SLEEP_STATUS_UNKNOWN` | AC電源時のsleepが有効または判定不能 | System Settings > Energyで「Prevent automatic sleeping when the display is off」を有効化 |
| `REGISTRY_CORRUPT` | registryを解釈不能 | 元fileを削除せず退避・確認し、repositoryを再登録 |
| `CONFIG_INVALID` | `.agent-loop.yaml`が無効 | 表示されたpathとvalidation errorを修正 |
| `REGISTRATION_MISSING` | repositoryが未登録 | `agent-loop register --repo PATH` |
| `REGISTERED_BINARY_MISSING` | 登録時の絶対command pathが移動 | install/update後に同じrepositoryを再register |
| `FORMATTER_GO_NOT_REGISTERED` / `FORMATTER_GO_UNAVAILABLE` / `FORMATTER_GO_CAPABILITY_MISSING` | 有効なbuilt-in Go formatterの固定pathが未登録、実行不能、またはstdin整形capability不一致 | loop停止中にGo toolchainとPATHを確認し、同じrepositoryを再registerしてdoctorを再実行 |
| `CODEX_LOCALHOST_NETWORK_PROXY_READY` | opt-in localhost-only workerに必要なstrict config、user config isolation、network proxy、hosted tool disable capabilityを確認済み | なし。実機E2Eは別途実行する |
| `CODEX_LOCALHOST_NETWORK_PROXY_UNAVAILABLE` | 設定はlocalhost-onlyだがCodex runtimeに必須capabilityがない | loopを開始せずCodex CLIを更新し、再register後にdoctorを再実行 |
| `FORMATTER_GO_AVAILABLE` / `FORMATTER_GO_DISABLED` | Go formatter capabilityが利用可能、または明示的に無効 | 対応不要 |
| `LAUNCH_AGENT_MISSING` / `LAUNCH_AGENT_UNREADABLE` | plistがない、または読めない | 再register、所有者・permission確認 |
| `WEBHOOK_SAFETY_SWEEP_STALE` | ready collectionの成功記録が2 sweep intervalを超えて更新されない | brokerのGitHub accessとsafety sweep logを確認 |
| `WEBHOOK_QUEUE_HEALTH_UNAVAILABLE` / `WEBHOOK_QUEUE_STALLED` | mailboxまたはcanonical snapshotを読めない、ready Issueが2 local reconciliation intervalを超えて未取得、またはmailboxが非有界 | `status --json`の`broker.queue_health`とroot `active_execution`を確認 |
| `WEBHOOK_QUEUE_DEFERRED_MAINTENANCE` | validなrepository assignment maintenance fenceによりqueue処理が意図的に停止中 | assignment完了後、fence不在と通常の`WEBHOOK_QUEUE_PROGRESSING`を確認 |
| `WEBHOOK_SAFETY_SWEEP_FRESH` / `WEBHOOK_QUEUE_PROGRESSING` | safety sweepと実装queueが観測上正常 | 対応不要 |
| `GITHUB_REPOSITORY_INACCESSIBLE` | repository参照権限または認証不足 | `gh auth status`とtoken/GitHub App権限を確認 |
| `GITHUB_LABELS_MISSING` | 必須label不足 | `bootstrap-labels`をpreviewし、確認後に`--apply` |
| `LOG_UNREADABLE` | supervisor logを読めない | logの所有者・permission確認 |
| `SUPERVISOR_BLOCKED` | supervisor全体障害で停止 | status、stderr log、直近eventを確認し、原因修復後にrestart |
| `SUPERVISOR_STOPPED` | supervisorが停止中 | 意図した停止か確認後、必要ならstart |

`SUPERVISOR_BLOCKED`と`SUPERVISOR_STOPPED`のdetailは、snapshotの状態・message、最後に解釈できたeventのtype/time、直近supervisor log行を相関して表示する。token値は表示せず、stateとlogで既にredactされた情報だけを使用する。

## 認証とmacOS設定の根拠

- Codex CLIは`codex login status`で認証方式を確認し、`codex login`でbrowser flow、headless環境では`codex login --device-auth`を利用できる。[OpenAI Authentication](https://learn.chatgpt.com/docs/auth.md)
- GitHub CLIは`gh auth status`でactive accountの認証状態を検証し、問題があれば非0で終了する。再認証は`gh auth login`を使う。[gh auth status](https://cli.github.com/manual/gh_auth_status)、[gh auth login](https://cli.github.com/manual/gh_auth_login)
- Mac miniではSystem SettingsのEnergyから「Prevent automatic sleeping when the display is off」を有効にする。Appleは消費電力が増える点も案内している。[Apple: Set sleep and wake settings for your Mac](https://support.apple.com/en-gb/guide/mac-help/mchle41a6ccd/mac)

`status` と `doctor` の durable state 診断は snapshot/events を既存 writer lock の下で読み、transaction の完了、event tail の切り詰め、隔離、recovery の実行を行わない。writer が lock を保持中、lock が欠損、prepared transaction（不正な内容を含む）、partial event tail の場合は `STATE_UNCONFIRMED` として終了コード1を返す。transaction がある場合は commit の確定を判定せず、修復も rollback もしない。snapshot の欠損は `STATE_MISSING`、整合性違反は `STATE_CORRUPT`、schema/semantic/lifecycle version 非互換は `STATE_VERSION_UNSUPPORTED`、既存 recovery marker は `STATE_RECOVERY_REQUIRED` であり、いずれも正常とは表示しない。正常な durable state の `status` は従来の形式、診断失敗時の JSON は `code`・`ok: false`・`detail` を返す。doctor は同じ診断を `diagnostics` に含め、host schema 検査で検知した `SCHEMA_VERSION_UNSUPPORTED` 等も併記する。

repository assignment がある環境では `~/.agent-loop-delivery.yaml` の対象 assignment、slot manifest と binary SHA-256 を検証し、実行中の管理 CLI の version・commit・digest が一致しなければ `STATE_RUNTIME_INCOMPATIBLE` で非破壊に拒否する。lifecycle API version が一致するだけでは validator の互換性を保証しない。配備の health check 中は、対象 repository の validating transaction・現 assignment の generation/current・maintenance fence・plist がすべて一致する場合に限り、検証済み candidate slot を照合先とする。別 binary は自動実行せず、他 repository の assignment は変更しない。配備設定がない従来環境では、この CLI 自身の snapshot 契約で非破壊に検査する。

旧管理 CLI の `status` には正常な新 snapshot を隔離する版があり、既配布 binary の動作を本修正で変更することはできない。repository assignment の更新は global CLI の更新とは独立しているため、診断前に `command -v agent-loop` と `agent-loop version --json` で管理 CLI を特定し、本修正を含む検証済み版を global CLI にも別途配備する。PATH、alias、監視スクリプトの固定 path に旧版が残っていないことを確認する。assignment runtime を直接使う場合も、本修正を含む版であることと配備情報との一致を確認してから実行する。旧版を新 snapshot に対して実行して安全性を試さない。複数 assignment の版が異なるときは対象ごとの検証済み runtime を使う。supervisor の明示的な recovery は別の操作として扱い、診断失敗を理由に自動再起動・復元を行わない。
