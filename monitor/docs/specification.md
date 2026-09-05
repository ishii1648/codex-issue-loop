# Specification

## 状態契約

| 状態 | 契約 |
| --- | --- |
| `IDLE` | readyまたはrunningの処理対象がない。正常とは推測せず、需要時間へ含めない。 |
| `HEALTHY` | 処理対象があり、repository queueの現在phaseが期限内である。 |
| `DOWN` | repository queueの現在phaseが期限に達した。開始はqueue-level deadlineである。 |
| `UNKNOWN` | GitHub観測失敗、cursor欠落、矛盾label、phase開始event欠落などにより履歴を証明できない。 |

open Issueにready labelが一つだけあればready phase、running labelが一つだけあればrunning phaseです。terminal labelまたはexclude labelを持つIssue、Pull Requestはqueueから除外します。readyとrunningを同時に持つIssueは`UNKNOWN`です。

queueにrunningが一つ以上あれば現在phaseはrunningであり、ready Issueの待ち時間は判定に使いません。running phaseのdeadlineは対象runningの`labeled` event時刻に`processing_timeout`を加えた時刻です。runningがterminal labelまたはclose eventでqueueを出てreadyだけが残ると、そのterminal event時刻から新しいready phaseと`acceptance_timeout`を開始します。readyだけのphaseへ後続readyが追加されてもadmission windowは延長しません。期限ちょうどから`DOWN`です。title変更、本文変更、comment、renameなどは進捗ではありません。

永続cursorより新しいready、running、terminal label eventとclose eventを古い順にreplayします。各eventの前にqueue deadlineを評価するため、一回の復旧pollでも`HEALTHY -> DOWN -> HEALTHY -> IDLE`のような複数区間をevent時刻どおりに確定できます。同じcursor範囲を再適用しても、cursor以下のeventと同じtransition IDは再確定しません。

GitHub APIが失敗したpoll時刻から`UNKNOWN`です。最後の成功pollから`observation_timeout`を超えた未観測状態を表示・reportするときはtimeout時刻から`UNKNOWN`として扱います。復旧pollでcursorまでのevent列を完全に取得できれば、その列とqueue deadlineから停止中の区間をreplayします。cursorを取得page内で発見できない場合など、履歴の完全性を証明できないgapは`UNKNOWN`とし、正常状態を推測しません。

## 区間とreport

repositoryごとにopen intervalは常に最大一つです。状態遷移時に旧intervalを確定し、`[started_at, ended_at)`として保存します。一つのevent batchから複数の旧intervalを確定できます。ゼロ長intervalは保存せず、同一transition IDは再保存しません。観測時刻の逆行と確定intervalの重複を拒否します。

指定期間で`H`を`HEALTHY`秒、`D`を`DOWN`秒、`I`を`IDLE`秒、`U`を`UNKNOWN`秒、期間全体を`T`とします。

- 需要時稼働率: `H / (H + D)`。需要時間が0なら`null`。
- 観測coverage: `(I + H + D) / T`。記録開始前と`UNKNOWN`はcoverageに含めない。

## schema

config、current state、interval、reportのschemaは`monitor/schemas/`にあります。monitor schema versionはsupervisor storage schemaから独立した`1`です。
