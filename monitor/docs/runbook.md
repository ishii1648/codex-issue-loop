# Runbook

## installと更新

同じreleaseの`agent-loop-monitor_Darwin_arm64`と`agent-loop_Darwin_arm64`を取得し、checksumを検証します。`monitor/config.example.yaml`を基にowner-onlyの`~/.agent-loop-monitor.yaml`を作ります。`github_cli`未指定時は`gh`を使うため、LaunchAgentユーザーで`gh auth status`が成功することを確認します。

```sh
chmod 0755 agent-loop-monitor_Darwin_arm64
./agent-loop-monitor_Darwin_arm64 install --config ~/.agent-loop-monitor.yaml --json
agent-loop-monitor service register --config ~/.agent-loop-monitor.yaml --json
agent-loop-monitor service start --config ~/.agent-loop-monitor.yaml --json
agent-loop-monitor service status --config ~/.agent-loop-monitor.yaml --json
```

更新は新release binaryで`install`を再実行し、`service register`、`service restart`の順に実行します。monitorの停止や更新はsupervisorを停止しません。

## 日常確認

```sh
agent-loop-monitor status --config ~/.agent-loop-monitor.yaml --json
agent-loop-monitor history --config ~/.agent-loop-monitor.yaml --from 2026-09-01T00:00:00Z --json
agent-loop-monitor report --config ~/.agent-loop-monitor.yaml --from 2026-09-01T00:00:00Z --to 2026-09-02T00:00:00Z --json
```

実地確認では対象repositoryの既存Issueだけを読みます。synthetic Issueを作成せず、GitHub audit上でmonitor由来のmutationがないことを確認します。

## 停止・再起動・復旧

`service stop`はLaunchAgentだけを停止し、config、current state、interval logを保持します。再開は`service start`、設定・binary更新後は`service restart`を使います。停止が`observation_timeout`を超えるとreportと次回pollはその区間を`UNKNOWN`として扱います。

GitHub失敗はrepositoryごとの`last_error`と`UNKNOWN`に記録されます。他repositoryのpollは継続します。認証、rate limit、repository名を修復後に`run --once --json`を実行し、`status`で復旧を確認します。破損したstateを推測で編集せず、該当directoryを保全して原因を調査します。

`last_error`がsnapshot不一致なら、cursorを操作せず次の通常pollで収束を確認します。`queue exit history is insufficient`、再入履歴不足、cursor探索上限の場合は、現在snapshotだけを根拠に正常扱いへ戻しません。cursor探索は最大1,000 eventで停止し、古いcursorの全履歴探索は行いません。継続するUNKNOWNは履歴不足として調査し、cursorの早送りやinterval削除で隠さないでください。観測不能期間内のdeadlineはDOWNと断定せずUNKNOWNに含め、復旧状態は検証したpoll時刻から始まります。

rollbackは旧releaseのmonitor binaryで`install`、`service register`、`service restart`を実行します。事前にmonitorを停止して専用state directoryを保全し、schema version 1のcurrent stateとinterval logは維持します。旧版はcursor/snapshotの検証が弱いため、rollback後の観測結果でUNKNOWN期間を補完しないでください。supervisorのassignmentとstateを変更する必要はありません。

logはmonitor state rootの`launchd.stdout.log`と`launchd.stderr.log`です。supervisor logとは別です。
