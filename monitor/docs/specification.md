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

GitHub API失敗または履歴検証失敗はpoll時刻から`UNKNOWN`です。失敗したpollが`observation_timeout`より長く途切れた場合は、最後のpoll時刻にtimeoutを加えた時刻から`UNKNOWN`区間を補います。ただし、最後の観測と失敗・timeout境界の間にqueue deadlineがある場合は、そのdeadlineから`UNKNOWN`にします。同じgapでは観測不能が期限超過判定に優先し、未検証の時間を`HEALTHY`や`DOWN`へ補完しません。復旧時はsnapshotを検証したpoll時刻から新しい状態を開始し、UNKNOWN中のevent時刻へ遡及しません。

停止中に失敗pollがなく、復旧pollでcursorまでの完全なevent列とsnapshotの整合性を検証できた場合は、timeoutを超えていてもdeadlineとevent時刻から区間を復元します。

## event取得と検証

repository issue eventsは1ページ100件、最大10ページのGETで取得し、自動paginationを使いません。通常pollは検証済みcursorを発見したページで停止します。10ページ以内または履歴終端までにcursorが見つからなければ、そのrepositoryを`UNKNOWN`にしてcursorを保持します。初回はrepository eventの先頭ページでcursorを取得し、現在actionableなIssueごとのevent履歴からphase開始を取得します。phase開始を証明できない場合は`UNKNOWN`です。

取得順序はevent列、open Issue snapshotです。両者はtransactionではないため、検証済みqueueへevent ID順にreplayした結果とsnapshotのIssue番号・phase・取得できたphase開始時刻を照合します。不一致ならbatch全体を不採用にし、cursor・queueを保持します。次pollは同じcursorから再取得し、収束して初めてcommitします。eventを含まないsnapshot側の先行更新も同じ扱いです。検証済み観測より古いeventや時刻が逆行するeventも推測で丸めず`UNKNOWN`にします。

queue退出を証明するeventはterminal/excludeの`labeled`または`closed`です。ready/runningの単独`unlabeled`は退出を証明せず、同じbatchの次phase `labeled`または退出eventが必要です。旧phase解除が新phase付与や退出より後でも中間の`IDLE`を作りません。runningからterminal/closeへの境界はそのterminal/close event時刻です。exclude/terminalの`unlabeled`と`reopened`は、現在snapshotだけでは再入時のlabel履歴を証明できないため`UNKNOWN`にします。title/comment/renameとPull Requestのeventはqueue進捗から除外します。

batch全体の検証後にevent境界とdeadlineを順に反映します。失敗したbatchでは区間をreplayせずUNKNOWNへの遷移のみ記録し、同じbatchのretryによる確定intervalの二重計上を防ぎます。

## 区間とreport

repositoryごとにopen intervalは常に最大一つです。状態遷移時に旧intervalを確定し、`[started_at, ended_at)`として保存します。一つのevent batchから複数の旧intervalを確定できます。ゼロ長intervalは保存せず、同一transition IDは再保存しません。観測時刻の逆行と確定intervalの重複を拒否します。

指定期間で`H`を`HEALTHY`秒、`D`を`DOWN`秒、`I`を`IDLE`秒、`U`を`UNKNOWN`秒、期間全体を`T`とします。

- 需要時稼働率: `H / (H + D)`。需要時間が0なら`null`。
- 観測coverage: `(I + H + D) / T`。記録開始前と`UNKNOWN`はcoverageに含めない。

## schema

config、current state、interval、reportのschemaは`monitor/schemas/`にあります。monitor schema versionはsupervisor storage schemaから独立した`1`です。
