# Specification

## 状態契約

| 状態 | 契約 |
| --- | --- |
| `IDLE` | readyまたはrunningの処理対象がない。正常とは推測せず、需要時間へ含めない。 |
| `HEALTHY` | 処理対象があり、各対象の現在phaseが期限内である。 |
| `DOWN` | 少なくとも一つの処理対象が期限に達した。開始は最初の超過deadlineである。 |
| `UNKNOWN` | GitHub観測失敗、矛盾label、phase開始event欠落、またはmonitor観測履歴のtimeoutにより判定できない。 |

open Issueにready labelが一つだけあればready phase、running labelが一つだけあればrunning phaseです。terminal labelまたはexclude labelを持つIssue、Pull Requestはqueueから除外します。readyとrunningを同時に持つIssueは`UNKNOWN`です。

ready phaseのdeadlineは最新のready `labeled` event時刻に`acceptance_timeout`を加えた時刻、running phaseは最新のrunning `labeled` event時刻に`processing_timeout`を加えた時刻です。期限ちょうどから`DOWN`です。title変更、本文変更、comment、renameなどは進捗ではありません。

GitHub APIが失敗したpoll時刻から`UNKNOWN`です。成功pollが`observation_timeout`より長く途切れた場合は、最後のpoll時刻にtimeoutを加えた時刻から`UNKNOWN`区間を補います。復旧時にUNKNOWN開始後の有効なlabel eventが見つかればそのevent時刻、見つからなければ復旧poll時刻から新しい状態を開始します。

## 区間とreport

repositoryごとにopen intervalは常に最大一つです。状態遷移時に旧intervalを確定し、`[started_at, ended_at)`として保存します。ゼロ長intervalは保存せず、同一IDは再保存しません。観測時刻の逆行と確定intervalの重複を拒否します。

指定期間で`H`を`HEALTHY`秒、`D`を`DOWN`秒、`I`を`IDLE`秒、`U`を`UNKNOWN`秒、期間全体を`T`とします。

- 需要時稼働率: `H / (H + D)`。需要時間が0なら`null`。
- 観測coverage: `(I + H + D) / T`。記録開始前と`UNKNOWN`はcoverageに含めない。

## schema

config、current state、interval、reportのschemaは`monitor/schemas/`にあります。monitor schema versionはsupervisor storage schemaから独立した`1`です。
