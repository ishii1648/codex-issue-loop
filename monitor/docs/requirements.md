# Requirements

## 機能要件

- GitHubのopen Issue、label、Issue event時刻だけを観測する。
- synthetic/canary Issueを作らず、GitHubへのmutationを行わない。
- repositoryごとに`IDLE`、`HEALTHY`、`DOWN`、`UNKNOWN`の非重複区間を記録する。
- `ready -> running`を受付進捗、`running -> done|needs-input|failed|blocked`を処理進捗とする。
- runningが存在する間はprocessing deadlineを使い、待機中readyのacceptance deadlineを無視する。
- runningのterminal event後にreadyが残る場合は、そのevent時刻から次のadmission windowを開始する。
- `DOWN.started_at`、復旧、terminalによる`IDLE`をdeadlineまたはevent時刻に記録し、poll時刻に丸めない。
- `IDLE`を需要時稼働率の分母から除外し、`UNKNOWN`を正常へ補完しない。
- cursorとtransition IDでreplayを冪等にし、event ID集合を無制限に保持せず、再起動後も確定区間を重複させない。
- repository eventはcursorを発見したpageで取得を止め、履歴の完全性を証明できない場合は`UNKNOWN`とする。
- 一つのprocessで複数repositoryを監視し、repository単位のAPI・判定失敗を隔離する。
- `run`、`status`、`history`、`report`をJSONでも提供し、未設定repositoryを指定した読み取りcommandを入力エラーにする。

## 非機能要件

- supervisor lifecycle packageへ依存しない独立binary・独立LaunchAgentとする。
- runtime config、state、interval log、launchd logをsupervisorと共有しない。
- monitor stateと出力にschema versionを持たせる。
- fake GitHubによるtestはnetworkとIssue作成を必要としない。
