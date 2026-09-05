# Requirements

## 機能要件

- GitHubのopen Issue、label、Issue event時刻だけを観測する。
- synthetic/canary Issueを作らず、GitHubへのmutationを行わない。
- repositoryごとに`IDLE`、`HEALTHY`、`DOWN`、`UNKNOWN`の非重複区間を記録する。
- `ready -> running`を受付進捗、`running -> done|needs-input|failed|blocked`を処理進捗とする。
- 検証できた連続観測では`DOWN.started_at`を期限超過時刻とする。観測不能gapは`UNKNOWN`とし、復旧poll以前の期限超過へ遡及しない。
- `IDLE`を需要時稼働率の分母から除外し、`UNKNOWN`を正常へ補完しない。
- event IDとcursorでreplayを冪等にし、再起動後も確定区間を重複させない。
- 一つのprocessで複数repositoryを監視し、repository単位のAPI・判定失敗を隔離する。
- `run`、`status`、`history`、`report`をJSONでも提供する。

## 非機能要件

- supervisor lifecycle packageへ依存しない独立binary・独立LaunchAgentとする。
- runtime config、state、interval log、launchd logをsupervisorと共有しない。
- monitor stateと出力にschema versionを持たせる。
- fake GitHubによるtestはnetworkとIssue作成を必要としない。
