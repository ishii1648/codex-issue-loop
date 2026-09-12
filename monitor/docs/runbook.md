# Runbook

## installと更新

独立した `monitor-v*` Release から monitor だけを取得します。`monitor/config.example.yaml`を基にowner-onlyの`~/.agent-loop-monitor.yaml`を作ります。`github_cli`未指定時は`gh`を使うため、LaunchAgentユーザーで`gh auth status`が成功することを確認します。

初回独立版の取得・検証例です。commit は公開タグが指す確認済みの40桁SHAを指定します。checksum・provenance の成功前にはbinaryを実行しません。

```sh
tag=monitor-v0.1.0
commit=FULL_40_CHARACTER_COMMIT
gh release download "$tag" --repo ishii1648/codex-issue-loop --dir ./monitor-release
cd monitor-release
shasum -a 256 -c checksums.txt
for asset in agent-loop-monitor_Darwin_arm64 checksums.txt; do
  gh attestation verify "$asset" --repo ishii1648/codex-issue-loop \
    --signer-workflow ishii1648/codex-issue-loop/.github/workflows/monitor-release.yml \
    --source-ref "refs/tags/$tag" --source-digest "$commit" --deny-self-hosted-runners
done
chmod 0755 agent-loop-monitor_Darwin_arm64
./agent-loop-monitor_Darwin_arm64 version --json |
  jq -e --arg tag "$tag" --arg commit "$commit" \
    '.version == $tag and .commit == $commit and .target == "darwin/arm64" and .monitor_schema_version == 1'
```

初回installは検証成功後に実行します。

```sh
chmod 0755 agent-loop-monitor_Darwin_arm64
./agent-loop-monitor_Darwin_arm64 install --config ~/.agent-loop-monitor.yaml --json
agent-loop-monitor service register --config ~/.agent-loop-monitor.yaml --json
agent-loop-monitor service start --config ~/.agent-loop-monitor.yaml --json
agent-loop-monitor service status --config ~/.agent-loop-monitor.yaml --json
```

dashboard を使わない観測サービスだけの環境では、検証済み新binaryを使い、旧binaryを作業directoryへ保存してから更新します。`STATE_ROOT` は既存configの `state_dir` に置き換えます。

```sh
cp STATE_ROOT/bin/agent-loop-monitor ./monitor-before-update
agent-loop-monitor service stop --config ~/.agent-loop-monitor.yaml --json
./agent-loop-monitor_Darwin_arm64 install --config ~/.agent-loop-monitor.yaml --json
agent-loop-monitor service start --config ~/.agent-loop-monitor.yaml --json
agent-loop-monitor service status --config ~/.agent-loop-monitor.yaml --json
```

切戻しは同じmonitorサービスを停止し、保存した `./monitor-before-update` の `status`、`history`、`report` に既存configと必要な期間を渡して読めることを確認してから、そのbinaryの `install --config ~/.agent-loop-monitor.yaml --json` を実行し、サービスを再開します。config/state/historyは復元・削除しません。読めない場合は停止を維持して原因を調査します。

dashboard を含む既存launchd環境の更新は、repository rootから次の配備操作を実行します。実行主体は両LaunchAgentの所有ユーザー（ログイン中のDarwin arm64ユーザー）、起動条件は運用者による検証済みstable tagとcommitの明示指定です。Python 3、既存の`gh`とPrometheusを使用します。`gh`には対象releaseの読み取り権限と`attestation verify --source-digest`対応が必要です。

```sh
python3 monitor/dashboard/manage.py deploy --tag monitor-vX.Y.Z --commit FULL_40_CHARACTER_COMMIT \
  --config ~/.agent-loop-monitor.yaml
```

`--root`は既存dashboard rootです。観測用`com.codex-issue-loop.monitor`と表示用`com.codex-issue-loop.monitor-dashboard.api`が登録・起動済みで、同じ指定configを使い、APIが`127.0.0.1:19110`、Prometheusが`127.0.0.1:19090`にあることを前提とします。初回もそれぞれのplistと実行中プロセスから参照先を確認するため、旧版で別々のbinaryを使っていても両方を更新します。観測binaryは既存installが配置する`<state root>/bin/agent-loop-monitor`を使用します。未導入環境では上記installと[dashboardの準備](dashboard.md#準備・起動)を先に完了します。

配備は対象tag/commitの製品別workflow成功、stable metadata、annotated tagのcommit、checksum、release workflow・tag・commitに拘束したattestationを検証してから成果物を実行します。旧binary・plistを保存し、両サービスをbootoutして旧PIDの終了を確認後、両参照先を同じ成果物へ置換し、bootstrapします。再起動後は実行ファイルのpath・inode・digest、登録引数・plist・PID、候補binaryが配信するHTMLとの完全一致、status/report/timeline、再起動後の観測成功、45秒未満のPrometheus freshnessを確認します。health待機は最大180秒で、観測も180秒未満を要求します。長いpoll間隔や外部障害により確認できなければ成功にはしません。

配備記録は観測binaryのstate root内`deployments/<tag>-<commit>/deployment.json`です。標準出力はtag・commit・工程をJSONで通知し、失敗時は`failed_phase`と記録先を示します。`complete`は上記機械検証の成功です。初回提供確認では[ブラウザ確認](dashboard.md#更新後の提供確認)も実施してください。GitHubの`production` environmentはRelease公開の承認境界であり、ホスト配備の起動や完了を意味しません。Release公開、ホスト配備、ブラウザ確認は別々に記録します。

同梱版から独立版への移行も同じ `deploy` を使い、保存した旧binaryへ `rollback` できます。旧 `v*` の配備記録の再実行・切戻しは従来の本体workflowとmanifestで検証します。monitorの停止や更新は本体binary・repository assignmentを変更せず、supervisorを停止しません。Prometheusの再起動・設定変更・TSDB操作も行いません。

## 配備の再実行・切戻し

同じtag/commitの`deploy`を再実行すると、`complete`なら稼働状態を再検証し、中断工程があれば保存済み成果物と旧版を保持して停止・配置・起動・検証を再実行します。同じstate rootの配備はロックで直列化し、別releaseの未完了記録がある場合は拒否します。記録やmonitorのrunロックを削除して再開しないでください。

```sh
python3 monitor/dashboard/manage.py rollback --tag monitor-vX.Y.Z --commit FULL_40_CHARACTER_COMMIT \
  --config ~/.agent-loop-monitor.yaml
```

切戻しには戻す先の旧tagではなく、取り消す配備のtag/commitを指定します。両サービスを停止して、保存した旧binaryのstatus/history/reportが現在のstate・全履歴を読み取れることを確認してから、旧binary・plistを復元して再起動・提供検証します。更新前後ともconfig、観測履歴、cursor、Prometheus TSDBは上書きしません。旧版のHTMLとの一致も確認します。`rolled-back`が切戻しの機械検証完了です。切戻しが中断した場合は同じ`rollback`を再実行します。

互換性検証が失敗した場合は両monitorサービスを停止したままにし、データの巻戻しや推測による変換は行いません。保存済み旧版で読めないdecision/schemaのデータには、そのデータを読める版への復旧が必要です。config・plist・binaryに配備外の変更がある場合も自動上書きせず、記録と実参照先を照合してから復旧します。稼働検証に失敗した配備は自動切戻しをせず失敗工程を残します。運用者が原因を修正して再実行するか、上記切戻しを選択します。

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
