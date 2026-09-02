# Incident自動対応runbook

Incident automationは、supervisorと既存のdurable state eventを構造化signalへ変換し、決定論的な分類を先に適用してから、必要なepisodeだけをCodexへ渡す。既定値は`enabled: false`かつ`dry_run: true`であり、設定を明示しない限りGitHubへ書き込まない。

## 処理と安全境界

1. scheduler、GitHub、worker、CI、review、retry、recoveryのsignalをprivateなrepository stateへ追記する。
2. signalをstable fingerprint単位に集約し、`analysis/incident-taxonomy/rules.json`でprimary classificationを1つだけ決める。
3. `expected_transient`はAIへ渡さない。それ以外はsanitized evidenceだけを`codex exec --sandbox read-only`へ渡し、versioned output schemaを検証する。
4. `suspected_bug`または`confirmed_bug`で、決定論的証拠、AI recommendation、medium以上のAI confidenceがすべて揃った場合だけIssue候補にする。
5. local stateとGitHub本文の`incident-fingerprint`を照合し、既存Issueを再利用する。作成後はnumber、URL、ready label、fingerprintをreadbackする。
6. 起票したIssueは通常queueへ入り、既存workerが修正、test、PR作成へ進む。GitHubのrequired checksと`reviewDecision`を観測し、`CHANGES_REQUESTED`または`REVIEW_REQUIRED`中はmergeへ進まない。approvalを本機能が合成することはなく、repository側の自動reviewまたは実reviewがauthoritativeである。

AI timeout、不正JSON、GitHub失敗は指数backoffで有限回だけ再試行する。上限到達後はcircuitを開き、別の`operator_attention` episodeとして残す。AIの判断だけで`confirmed_bug`へ昇格することはない。

## 設定

`worker.backend`は`codex`でなければならない。Issueには`.agent-loop.yaml`の`github.ready_labels`だけを付ける。

```yaml
incident_automation:
  enabled: false
  dry_run: true
  interval: 15m
  analyzer_timeout: 10m
  max_analysis_attempts: 3
  retry_backoff: 1m
  max_episode_items: 128
  degradation_threshold: 2m
```

`interval`は1分以上である。`degradation_threshold`を超えたscheduler cycleだけが`degraded`候補になり、閾値前はAIやIssue producerへ渡らない。signalのrotation、保持世代、最大sizeは既存の`logs.rotate_interval`、`logs.generations`、`logs.rotate_bytes`を共有する。episode内のsignal、evidence、lifecycleのcardinality上限は`max_episode_items`で16〜128件に設定する。

## 導入と確認

設定変更後は対象repositoryを固定して互換性を確認する。

```sh
agent-loop doctor --repo "$PWD" --json
agent-loop incident status --repo "$PWD" --json
agent-loop incident analyze-once --repo "$PWD" --json
```

`analyze-once`はautomationが無効でも実行でき、`dry_run: true`ならIssueを作成しない。出力と`issue-dry-run.json`でtitle、body、label、fingerprintを確認する。定期実行は`enabled: true`にした次回supervisor起動時から開始し、起動直後と設定intervalごとに動く。同一repositoryのprocess lockにより、重複schedulerは同時に分析・起票しない。

live作成を許可するときだけ`dry_run: false`へ変更する。設定変更を反映する`register`または`restart`は通常の運用手順どおり対象と影響を確認して明示実行する。本番有効化前に次が必要である。

- LaunchAgent利用者のCodex loginが有効である。
- `gh`にIssue/PR/checkのread権限とIssue作成権限がある。
- ready labelが既に存在し、`doctor`がlabel診断を通過する。
- repositoryの自動reviewまたはreview運用がGitHub `reviewDecision`へ結果を記録する。

## 状態、保存先、secret

既定のmanaged root配下にある`repos/<repo-id>/incidents/`へ次を保存する。directoryは`0700`、fileは`0600`で作る。

- `signals.jsonl`: schema version、時刻、repository、correlation ID、outcome、reason codeを持つsanitized signal
- `state.json`: episode、classification、AI結果、Issue identity、retry/circuit、fix lifecycle
- `metrics.json`: signal/outcome/classification/Issue/analysis回数、duration、open episode、open circuitのbounded aggregate
- `issue-dry-run.json`: 次回作成予定のIssue payload

schemaは`schemas/incident-*.schema.json`にある。token、credential、raw worker transcript、raw AI transcript、user home pathは保存しない。設定済みsecretと既知markerは永続化前にredactし、高cardinality IDをmetrics keyへ使わない。

```sh
agent-loop incident status --repo "$PWD" --json
```

restart時は保存済みstateとactive/archive signalを読み直す。event再送、入力順、rotation後の新規lifecycle signalをdedupし、Issue identityとattemptを引き継ぐ。

## 停止と復旧

定期分析だけを止めるには`enabled: false`へ戻し、通常の設定反映手順でsupervisorを再起動する。保存済みincidentは削除しない。loop全体を止める場合は、対象repositoryを確認して通常の`agent-loop stop --repo ...`を使う。

circuitの原因を解消した後は`status`から対象fingerprintを確認し、次の明示操作でそのepisodeのretry budgetだけをresetする。evidence、Issue identity、他episodeは保持され、対応するoperator-attention episodeは`resolved`になる。

```sh
agent-loop incident retry \
  --repo "$PWD" \
  --fingerprint '<64-character fingerprint>' \
  --json
```

resolved episode、存在しないfingerprint、開いていないcircuitは拒否する。state fileやlabelを手編集しない。

## Offline E2E

次の1コマンドはnetworkとlive GitHubを使わず、fake AI、fake GitHub、fake clockで分類、AI failure、dry-run、dedup、restart、rotation、CI/review/merge/close、retry exhaustionとrecoveryを検証する。

```sh
make incident-e2e
```

historical corpusとgolden resultは別の1コマンドで検証する。

```sh
go run ./cmd/incident-eval
```
