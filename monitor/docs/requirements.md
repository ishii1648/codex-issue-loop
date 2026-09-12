# Requirements

## 機能要件

- GitHubのopen Issue、label、Issue event時刻だけを観測する。
- synthetic/canary Issueを作らず、GitHubへのmutationを行わない。
- repositoryごとに`IDLE`、`HEALTHY`、`DOWN`、`UNKNOWN`の非重複区間を記録する。
- `ready -> running`を受付進捗、`running -> done|needs-human|failed|blocked`を処理進捗とする。
- 測定対象はrepository全体のキュー処理進行であり、個別Issueの滞留・失敗・隔離だけでは全体を`DOWN`にしない。
- runningが存在する間は、Issue横断の最後の有効な進捗からprocessing windowを使い、待機中readyのacceptance deadlineを無視する。期限後は正常な長時間処理・外部待ち・停止を識別できないため`UNKNOWN`とする。
- 古いrunning Aが残っていても、B/Cの受付・処理進捗がprocessing window内に継続すれば`HEALTHY`を維持する。`DOWN`または`UNKNOWN`からも別Issueの有効な進捗event時刻で復旧する。
- runningがなくreadyだけの需要が継続し、acceptance deadlineに達した場合のみ`DOWN`とする。ready追加・同phase再label・重複eventは期限を延長しない。
- runningのterminal event後にreadyが残る場合は、そのevent時刻から次のadmission windowを開始する。
- `DOWN.started_at`、復旧、terminalによる`IDLE`をdeadlineまたはevent時刻に記録し、poll時刻に丸めない。
- 履歴の検証失敗ではcursorとqueueを保持し、次回pollで完全な履歴を検証できれば最終成功時刻から欠測期間を補完する。即時retryは行わない。
- `IDLE`を需要時稼働率の分母から除外し、履歴で証明できない`UNKNOWN`を正常へ補完しない。
- cursorとtransition IDでreplayを冪等にし、event ID集合を無制限に保持せず、再起動後も確定区間を重複させない。
- repository eventはcursorを発見したpageで取得を止め、履歴の完全性を証明できない場合は`UNKNOWN`とする。
- 一つのprocessで複数repositoryを監視し、repository単位のAPI・判定失敗を隔離する。
- `run`、`status`、`history`、`report`をJSONでも提供し、未設定repositoryを指定した読み取りcommandを入力エラーにする。

## 非機能要件

- supervisor lifecycle packageへ依存しない独立binary・独立LaunchAgentとする。
- runtime config、state、interval log、launchd logをsupervisorと共有しない。
- monitor stateと出力にschema versionを持たせる。
- fake GitHubによるtestはnetworkとIssue作成を必要としない。
- 判定versionと切替境界を永続化し、旧区間を保持・識別する。旧契約の正常・異常時間を新契約の稼働率へ混算せず、根拠のない過去再計算をしない。
