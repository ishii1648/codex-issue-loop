# スマートフォン直接push通知

> この文書の`.agent-loop.yaml` `notifications`は、Codex Desktopのquestion notificationsとは別のopt-in外部push adapterである。接続中の通常経路は[Codex Desktop監視task運用](codex-desktop-monitoring.md)を使う。

## 1. 目的と選定

監視用Codex taskが`watch`へ接続していない間も、`needs_input`とsupervisorの`blocked`をスマートフォンへ通知する。通知はループの正本ではなく、永続snapshotへ戻るための補助経路である。

初期adapterには`ntfy`を採用する。HTTP POSTとBearer tokenだけで送信でき、iOS/Android appがあり、hosted serviceとself-hostを選べるためである。adapter interfaceはprovider-neutralに保ち、provider固有処理をsupervisorやoutboxへ混ぜない。

2026-08-16時点で、App Server所有threadは`thread/resume`と`turn/start`でprogrammaticに継続できる。一方、任意のChatGPT desktop taskを外部processからwakeし、その表示を`Needs input`へ変えてmobile pushする公開契約は記載されていない。ChatGPTの[Notifications](https://learn.chatgpt.com/docs/notifications)、[Scheduled tasks](https://learn.chatgpt.com/docs/automations)、Codex [App Server](https://learn.chatgpt.com/docs/app-server)、Codex CLIの[`notify`](https://learn.chatgpt.com/docs/config-file/config-advanced#notifications)を確認した。CLIの`notify`が扱うeventは現在`agent-turn-complete`だけである。詳細な判定は[Codex公式仕様確認](codex-capability-review.md)を正本とする。

## 2. provider比較

| 候補 | push/導入 | credential | 費用・運用 | 判断 |
| --- | --- | --- | --- | --- |
| OpenAI公式 | ChatGPT/Codex内の通知、scheduled task、App Server thread継続は利用可能 | OpenAI既存認証 | 任意Desktop task wakeとmobile Needs input連携は公開契約なし | 将来の優先候補 |
| ntfy | 単純なHTTP API、iOS/Android app、self-host可 | publish用access tokenと非推測topic | hosted free/paid tierまたはself-hostのinfra運用 | 初期採用 |
| Pushover | HTTPS Message API、専用mobile app | application tokenとuser key | 個人利用はplatformごとの買い切り、送信上限あり | 次点 |
| Slack | Incoming WebhookとSlack mobile通知 | secretを含むwebhook URL | workspace/app管理が必要。mobile通知はユーザー設定にも依存 | team運用向け候補 |
| APNs直接 | iOS native push | Apple key、device token、backend | 専用app、証明書、配信backendの保守が必要 | 初期範囲外 |

参照: [ntfy publishing/authentication](https://docs.ntfy.sh/publish/)、[ntfy mobile app](https://docs.ntfy.sh/subscribe/phone/)、[ntfy privacy/self-host](https://docs.ntfy.sh/privacy/)、[Pushover API](https://pushover.net/api)、[Pushover pricing](https://pushover.net/pricing)、[Slack Incoming Webhooks](https://api.slack.com/messaging/webhooks)、[Slack rate limits](https://docs.slack.dev/apis/web-api/rate-limits/)。価格とservice条件は変更され得るため、導入時に公式pageを再確認する。

## 3. 配送仕様

通知対象は次のattention eventである。

- `input_requested`: request ID単位で一度だけenqueueする。
- `supervisor_blocked`: blocked理由のdigest単位でenqueueする。
- `issue_blocked`: 将来の明示的なIssue blocked eventに備えたadapter上の種別。現行workerの入力待ちは`input_requested`を使う。

attention状態の永続更新と同じstate transactionでoutboxへenqueueする。送信済み、再送待ち、失敗、取消を`state.json`へ保存するため、supervisor再起動後も重複送信せず再開できる。回答済みrequestの未送信通知は送信前に取消す。

配送失敗は10秒から始まるexponential backoffで既定10分を上限に8回まで再送する。送信間隔は既定5秒以上とし、adapter障害はeventとlogへ残すがsupervisor本体を停止しない。providerからの応答を受けるまでのtimeoutは既定10秒である。

正常なnetworkとproviderの下では、attentionを保存した同じsupervisor cycleで最初の送信を試みる。長時間処理中や再起動直後も次のcycleでoutboxをreconcileする。運用上の目標はattention保存から90秒以内の初回push到達とするが、外部providerとmobile OSの配送時間は保証範囲外である。

## 4. 機密性と脅威

- 機能は既定で無効にし、明示的なopt-inを必要とする。
- `include_details: false`を既定とし、通知本文にはrepository名、Issue番号、request ID、状態だけを含める。
- 詳細を有効にすると質問文や失敗理由がproviderとlock screenへ渡る。機密repositoryでは有効化しない。
- endpointはHTTPSを必須とする。HTTPはloopback testだけを許可する。
- topicは8–128文字の推測困難な値とし、private access controlを設定する。topic名だけを認証手段にしない。
- tokenは`.agent-loop.yaml`、LaunchAgent plist、repository、logへ保存しない。repository別管理fileへmode `0600`で保存し、送信error、state、event、logではmaskする。
- ntfy hosted serviceを使う場合、最小本文であってもproviderがmetadataを処理する。許容できない場合はprivate serverを検討する。iOSで即時pushを維持するself-host構成にはntfy公式のupstream要件があるため、導入時に確認する。

## 5. セットアップ

### 5.1 外部serviceとスマートフォン

1. ntfyのaccountを作成し、推測困難なprivate topicを予約する。
2. publish専用access tokenを発行し、そのtopicへの必要最小限のwrite権限を与える。
3. スマートフォンへntfy appを入れて同じaccountでログインし、topicを購読する。
4. lock screenへ表示してよい情報量を決める。通常は詳細を無効のまま使う。

### 5.2 Mac mini

対象repositoryを`register`した後、tokenを標準入力から保存する。shell historyやprocess引数へtokenを置かない。

```sh
agent-loop notification-token \
  --repo /absolute/path/to/repository \
  --token-file -
```

`.agent-loop.yaml`で通知を有効にする。

```yaml
notifications:
  enabled: true
  provider: ntfy
  endpoint: https://ntfy.sh
  topic: replace-with-opaque-private-topic
  include_details: false
```

設定後に診断してloopを再起動する。

```sh
agent-loop doctor --repo /absolute/path/to/repository
agent-loop restart --repo /absolute/path/to/repository
```

tokenを解除する場合は、先に通知を無効にするかloopを停止してから次を実行する。

```sh
agent-loop notification-token --repo /absolute/path/to/repository --clear
```

## 6. 通知からの復帰導線

通知tapは該当GitHub Issueを開く。公開仕様で安定したCodex task deep linkがないため、通知から既存taskを直接起動しない。Issueで状況を確認した後、ChatGPT mobile appの`[LOOP] <repo> — monitor` taskへ戻り、`status`または`watch`で未回答requestを読み、`answer`で回答する。

## 7. 運用確認

初回導入とprovider変更時は次を確認する。

1. 監視taskを閉じた状態で、テスト用Issueを安全に`needs_input`へ遷移させる。
2. `state.json`のnotificationが`sent`になり、`notification_sent` eventが1件だけ記録されることを確認する。
3. スマートフォンへの到達時刻を記録し、90秒目標を満たすことを確認する。
4. 同じrequestの再読込・supervisor再起動でpushが重複しないことを確認する。
5. 一時的にendpointを到達不能にし、retry中もIssue loopが継続することを確認する。

この実機確認には外部account、credential、mobile appが必要であり、repository内の自動testでは代替しない。
