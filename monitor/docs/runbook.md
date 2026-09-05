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

GitHub失敗はrepositoryごとの`last_error`と`UNKNOWN`に記録されます。他repositoryのpollは継続します。認証、rate limit、repository名を修復後に`run --once --json`を実行し、`status`で復旧を確認します。破損したstateを推測で編集せず、該当directoryを保全して原因を調査します。

logはmonitor state rootの`launchd.stdout.log`と`launchd.stderr.log`です。supervisor logとは別です。
