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

`service stop`はLaunchAgentだけを停止し、config、current state、interval log、event cursorを保持します。再開は`service start`、設定・binary更新後は`service restart`を使います。停止中は`observation_timeout`以降を一時的に`UNKNOWN`として表示しますが、再開pollでrepository eventのcursorまでの完全な履歴を取得できればdeadlineとevent時刻から区間をreplayします。cursorを発見できない場合はgapを推測せず`UNKNOWN`のまま扱います。

GitHub失敗はrepositoryごとの`last_error`と`UNKNOWN`に記録されます。他repositoryのpollは継続します。認証、rate limit、repository名を修復後は`service stop --config ~/.agent-loop-monitor.yaml`でmonitorを停止してから`run --config ~/.agent-loop-monitor.yaml --once --json`を実行し、`status`で復旧を確認して`service start --config ~/.agent-loop-monitor.yaml`で再開します。`another monitor is already running`で終了した場合は、同じ`state_dir`を使用する別のrunの終了を確認してから再実行します。ロックファイルは削除しないでください。破損したstateを推測で編集せず、該当directoryを保全して原因を調査します。

`last_error`がsnapshot不一致なら、cursorを操作せず次の通常pollで収束を確認します。`queue exit history is insufficient`、再入履歴不足、cursor探索上限の場合は、現在snapshotだけを根拠に正常扱いへ戻しません。cursor探索は最大1,000 eventで停止し、古いcursorの全履歴探索は行いません。継続するUNKNOWNは理由を確認し、履歴不足またはrunningの有効な進捗が期限内にない状態として調査し、cursorの早送りやinterval削除で隠さないでください。完全な履歴を検証できれば、最終成功時点からeventとdeadlineを再生して一時的なUNKNOWNを補完します。履歴が不足する期間はUNKNOWNを残し、現在状態は検証したpoll時刻から始まります。

logはmonitor state rootの`launchd.stdout.log`と`launchd.stderr.log`です。supervisor logとは別です。

判定version 3への更新では、最初のpollで旧履歴を保持したまま契約を切り替えます。旧区間は新契約の稼働率から除外され、reportの`legacy_seconds`に現れます。runningが残る初回・切替・再同期ではUNKNOWNから開始し、次の有効な受付・処理進捗で復旧します。詳細は[判定契約の切替と旧履歴](specification.md#判定契約の切替と旧履歴)を参照してください。
