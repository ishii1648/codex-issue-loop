# Specification

## 状態契約

| 状態 | 契約 |
| --- | --- |
| `IDLE` | readyまたはrunningの処理対象がない。正常とは推測せず、需要時間へ含めない。 |
| `HEALTHY` | readyだけの受付window内、またはIssue横断の有効な進捗に基づくprocessing window内である。個別Issueの処理時間達成率を意味しない。 |
| `DOWN` | runningがなく、readyだけの処理需要が受付期限まで継続しても受付進捗がない。開始はqueue-level acceptance deadlineである。 |
| `UNKNOWN` | runningの進捗証拠の期限切れ、GitHub観測失敗、矛盾label、phase開始event欠落などにより現在の状態を証明できない。cursor欠落などで再生できない過去区間もUNKNOWNとして保存する。 |

open Issueにready labelが一つだけあればready phase、running labelが一つだけあればrunning phaseです。terminal labelまたはexclude labelを持つIssue、Pull Requestはqueueから除外します。readyとrunningを同時に持つIssueは`UNKNOWN`です。

queueにrunningが一つ以上あれば現在phaseはrunningであり、ready Issueの待ち時間は判定に使いません。検証済みqueueの`ready -> running`を受付進捗、同じ実行の履歴からrunningだったことを確認できるIssueの`closed`を処理進捗とします。どのIssueの進捗でも、event後にrunningが残ればそのevent時刻から`processing_timeout`のwindowを開始・更新します。期限ちょうどから`UNKNOWN`です。runningがなくreadyだけになれば、その処理進捗event時刻から`acceptance_timeout`を開始します。空queueはevent時刻から`IDLE`です。

検証済み空queueへのready追加は、そのevent時刻から受付windowを開始します。初回・契約切替では、過去に存在したrunningの退出時刻が分からないため、readyのみのqueueを確認した観測時刻から開始します。後続ready追加や同phaseの再labelでは延長せず、期限ちょうどから`DOWN`です。`DOWN`や`UNKNOWN`中の別Issueの有効な受付・処理進捗でもevent時刻に復旧し、古いIssueの退出を待ちません。title変更、本文変更、comment、rename、重複event、同phase再label、running labelだけでの再入場は進捗ではありません。done/failed/terminal/exclude labelの付与・削除やrunning label解除によるqueue退出は、処理進捗ではありません。closedは取り下げを含む処理終了の証拠であり、実装成功を証明しません。readyのままのcloseは処理進捗ではありません。

例えば古いrunning Aが残り、B/Cが20分ごとに受付・処理進捗を示せば、`processing_timeout=2h`では`HEALTHY`を維持します。単独runningが10:00の受付後に変化しなければ12:00から`UNKNOWN`です。全Issueが外部待ちの場合も同じです。GitHubのlabel/eventだけでは正常な長時間処理・外部待ち・supervisor停止を区別できないため、無変化を`DOWN`へ読み替えません。readyのみの需要が10:00に始まり`acceptance_timeout=10m`なら10:10から`DOWN`です。これは受付進捗の期限超過であり、supervisor processの停止を直接証明するものではありません。

初回・契約切替・履歴欠損による再同期では、現在runningのlabel開始時刻からキュー全体の進捗時計を復元しません。runningがあれば`UNKNOWN`を維持し、その後の有効な進捗で時計を開始します。再起動時は同じ判定versionの永続queue時計を継続します。readyだけの再同期では検証した観測時刻から受付windowを開始し、以前から残るreadyがあれば既存の早い受付期限を維持します。欠損区間を正常へ遡及補完しません。

永続cursorより新しいready、running、terminal label eventとclose eventを古い順にreplayします。各eventの前にqueue deadlineを評価するため、一回の復旧pollでも`HEALTHY -> DOWN -> HEALTHY -> IDLE`のような複数区間をevent時刻どおりに確定できます。同じcursor範囲を再適用しても、cursor以下のeventと同じtransition IDは再確定しません。

GitHub API失敗または履歴検証失敗はpoll時刻から`UNKNOWN`です。失敗したpollが`observation_timeout`より長く途切れた場合は、最後のpoll時刻にtimeoutを加えた時刻から`UNKNOWN`区間を補います。ただし、最後の観測と失敗・timeout境界の間にqueue deadlineがある場合は、そのdeadlineから`UNKNOWN`にします。同じgapでは観測不能が期限超過判定に優先し、未検証の時間を`HEALTHY`や`DOWN`へ補完しません。次回の定期pollで最終成功cursor以降の完全な履歴と現在snapshotの整合性を検証できた場合は、保持したqueueとqueue-level deadlineを起点に最終成功時刻から再生し、一時的なUNKNOWN区間を復元した状態で置き換えます。履歴が不足する場合はUNKNOWNを残し、検証したpoll時刻から現在状態へ復帰します。

停止中に失敗pollがなく、復旧pollでcursorまでの完全なevent列とsnapshotの整合性を検証できた場合は、timeoutを超えていてもdeadlineとevent時刻から区間を復元します。

## event取得と検証

repository issue eventsは1ページ100件、最大10ページのGETで取得し、自動paginationを使いません。通常pollは検証済みcursorを発見したページで停止します。10ページ以内または履歴終端までにcursorが見つからなければ、現在の全actionable Issueとその履歴を検証して再同期します。取得失敗やsnapshot不整合ではcursorを保持し、poll内の即時retryは行わず、次回の定期pollで再取得します。初回はrepository eventの先頭ページでcursorを取得し、現在actionableなIssueごとのevent履歴からphase開始を取得します。phase開始を証明できない場合は`UNKNOWN`です。

取得順序はevent列、open Issue snapshot、Issue履歴、open Issue snapshotの再取得、event headの再取得です。snapshotまたはheadが取得中に変化した場合は採用しません。Issue履歴は現在の開閉・label状態から逆算し、除外中のlabel変更を除いてreopenや最後の除外label解除による再入場を判定します。両者はtransactionではないため、検証済みqueueへevent ID順にreplayした結果とsnapshotのIssue番号・phase・取得できたphase開始時刻を照合します。再生不能でも現在snapshotと履歴の取得境界が検証できた場合は、その観測時点で再同期します。再生不能な過去は最後の観測境界からUNKNOWNとし、既存のUNKNOWN区間をHEALTHYへ変更しません。開始時刻を証明できない需要はUNKNOWNを維持し、次pollでも履歴を再取得します。phase開始へ取得時刻を代入しません。完全に確認した空queueはIDLEへ復帰します。eventを含まないsnapshot側の先行更新も同じ扱いです。検証済み観測より古いeventや時刻が逆行するeventも推測で丸めず`UNKNOWN`にします。

queue退出は開閉・label履歴と現在snapshotから判定します。ready/runningの`unlabeled`が次phaseへのlabel置換である場合は、その次phaseまで既存windowを維持します。旧phase解除が新phase付与や退出より後でも中間の`IDLE`を作りません。処理進捗の境界はclosed event時刻です。close時点のrunning label残存は必要ありません。pollや保存・再読込をまたいでもIssue全履歴から同じ実行のrunningを確認し、readyへの戻りやreopenを越えて古い根拠を流用しません。履歴を証明できないcloseは進捗にしません。exclude/terminalの`unlabeled`と`reopened`は、その時点でopenかつ除外labelがなくready/runningを持つ場合のみ再入場として扱います。title/comment/renameとPull Requestのeventはqueue進捗から除外します。

batch全体の検証後にevent境界とdeadlineを順に反映します。失敗したbatchでは区間をreplayせずUNKNOWNへの遷移のみ記録し、次回pollでの同じbatchの再取得による確定intervalの二重計上を防ぎます。

## 区間とreport

repositoryごとにopen intervalは常に最大一つです。状態遷移時に旧intervalを確定し、`[started_at, ended_at)`として保存します。一つのevent batchから複数の旧intervalを確定できます。ゼロ長intervalは保存せず、再生で置き換える範囲より前の区間は保持し、同じ結果の再保存でも区間を重複させません。観測時刻の逆行と確定intervalの重複を拒否します。

指定期間で`H`を`HEALTHY`秒、`D`を`DOWN`秒、`I`を`IDLE`秒、`U`を`UNKNOWN`秒、期間全体を`T`とします。

- 需要時稼働率: `H / (H + D)`。需要時間が0なら`null`。
- 観測coverage: `(I + H + D) / T`。記録開始前と`UNKNOWN`はcoverageに含めない。

## schema

config、current state、interval、reportのschemaは`monitor/schemas/`にあります。monitor schema versionはsupervisor storage schemaから独立した`1`です。

## 判定契約の切替と旧履歴

JSON形式の`schema_version=1`とは独立して、current state・interval・reportに`decision_version=3`を記録します。省略または`1`、`2`は旧判定です。current stateの`decision_since`は新契約の開始境界です。旧snapshotを最初にpollした際、旧currentを最終観測時刻で閉じ、そこから切替pollまでの未検証時間を旧契約の`UNKNOWN`として保存します。新currentは切替poll時刻から開始し、旧queue deadlineを引き継ぎません。切替後のエラー・再同期・replayもこの境界より前を新契約へ変更しません。migrationは既存のatomic commit境界を使い、再実行しても区間を重複させません。

確定済み旧intervalは状態・時刻・判定versionを保持し、`history`/`api/history`で参照できます。reportは旧区間の秒数を`legacy_seconds`に明示し、状態別集計では`UNKNOWN`へ含めます。新契約の`HEALTHY`/`DOWN`、需要時稼働率、coverageには加算しません。timelineも旧区間を`UNKNOWN`で表示し、reasonに旧状態、decision_versionに旧契約を残します。未移行snapshotの現在表示は`UNKNOWN`です。

保存しているのはqueue snapshot・cursor・intervalであり、過去の全event列ではありません。旧区間の自動再計算は行いません。切替後に完全なcursor範囲とsnapshotを検証できる場合のみ、その契約内で再生できます。取得上限を超えた履歴や初回以前の完了Issueの進捗は推測せず、履歴削除や本番state編集による補正は不要です。
