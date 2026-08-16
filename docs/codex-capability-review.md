# Codex Goal・Desktop通知・task wake・token計測の公式仕様確認

- 確認日: 2026-08-17
- 確認したlocal CLI: `codex-cli 0.136.0`
- OpenAI公式Codex manual SHA-256: `40f7ed6c2b2b08b0e45ef4f60acbaf258348b0633db9b4757b6fff83a57f6849`
- 判定語: **利用可能**、**利用不可**、**未保証**を区別する

## 結論

| 項目 | 判定 | `agent-loop`での扱い |
| --- | --- | --- |
| 対話surfaceのGoal | 利用可能 | 単一目的の監視・復旧taskで利用できる |
| headless Goal | App Server経由で利用可能 | optional `extended` worker adapterをIssue #53で検証する。現行workerは変更しない |
| `codex exec --goal`相当 | 利用不可 | 公式non-interactive interfaceにGoal optionはないため推測で呼ばない |
| App Server所有threadのprogrammatic resume/start | 利用可能 | `thread/resume`と`turn/start`を将来adapterで利用できる |
| Desktopのquestion notifications | 利用可能 | 接続中の監視taskが質問した際の通常OS通知に使う |
| Desktop Activityの回答待ち | 利用可能 | OS通知dismiss後に回答待ちchatを再発見する |
| project/chatのpin | 利用可能 | repositoryごとの監視chatを見つけやすくする。権限やcontextは増えない |
| 任意のDesktop taskを外部processからwake | 利用不可 | 公開契約がない。App Server所有threadの制御とDesktop taskへの注入を同一視しない |
| 外部processからモバイルUIを`Needs input`へ遷移 | 利用不可 | App Serverのserver requestはclientが表示・回答する契約であり、ChatGPT mobile通知連携は保証されない |
| chat内scheduled taskによる定期再開 | 利用可能 | 時刻ベースで同じchatへ戻れるが、event-driven wakeやtoken-free monitorの代替にしない |
| CLI外部`notify` | 利用可能だがturn完了のみ | 現在のsupported eventは`agent-turn-complete`だけ。入力待ちpushには使わない |
| turn/Goalのtoken counters | 利用可能 | App Server eventまたはGoal stateから観測できる |
| 保留中tool call・long command待機中の厳密な無課金 | 未保証 | 製品全体のzero-token/zero-costを要件にしない。Go pollingがmodelを呼ばない範囲だけ保証する |

## Goal

OpenAIの[Long-running work](https://learn.chatgpt.com/docs/long-running-work)では、Goal modeはCodex app、対話的CLI、IDE extensionで利用でき、同じchat/session内でpause、resume、edit、clearできる。Goalはsandboxとapproval policyを拡張せず、判断が必要なら停止する。2026-05-21の[公式changelog](https://learn.chatgpt.com/docs/changelog#codex-2026-05-21)ではexperimentalを卒業したと案内されている。

[Configuration Reference](https://learn.chatgpt.com/docs/config-file/config-reference)は`features.goals`をstable・既定有効とし、persisted goalsとautomatic continuationを提供すると記載する。

[Codex App Server](https://learn.chatgpt.com/docs/app-server)には次の公開methodとeventがある。

- `thread/goal/set`、`thread/goal/get`、`thread/goal/clear`
- `thread/resume`、`turn/start`、`turn/steer`
- `thread/goal/updated`、`thread/goal/cleared`
- Goal status: `active`、`paused`、`blocked`、`usageLimited`、`budgetLimited`、`complete`
- `tokenBudget`、`tokensUsed`、`timeUsedSeconds`

localのCodex CLI 0.136.0で`codex app-server generate-json-schema`を実行し、上記method、status、fieldが生成schemaにも存在することを確認した。Goal methodは公式App Server API overviewでexperimentalとは表示されていない。

一方、[Non-interactive mode](https://learn.chatgpt.com/docs/non-interactive-mode)が自動化用に説明する`codex exec`にはGoal optionが記載されていない。したがって現行の`codex exec` workerへ未文書化flagを追加しない。App Serverはrich client integration向けであり、自動jobにはSDKを推奨するという公式の位置づけも踏まえ、adapter化は既存方式とのfailure・security比較を行う。

Goalを利用しても責務境界は変えない。

- Goalは1件のIssue、特に`extended` profileの目的・budget・continuationだけを扱う。
- Issue選択、claim、worktree、GitHub公開、次Issueへのloop、process再起動はGo supervisorが所有する。
- Goal stateをqueueの正本やLaunchAgentの代替にしない。
- App Server adapterが失敗しても、現行worktreeと永続stateを失わない。

実装検証は[#53](https://github.com/ishii1648/codex-issue-loop/issues/53)へ切り出した。

## External wakeとNeeds input

[Notifications](https://learn.chatgpt.com/docs/notifications)は、Desktop Settingsでpermission notificationsとquestion notificationsを個別に設定でき、OSがChatGPT desktop appの通知権限を要求する場合があると記載する。ActivityはsidebarのbellまたはmacOSの`Cmd`+`Option`+`U`から開き、unread、running、回答待ちのchatを表示できる。利用可能なsurfaceのpetも`Running`、`Needs input`、`Ready`、`Blocked`を表示できる。

[Projects and chats](https://learn.chatgpt.com/docs/projects)は、頻繁に戻るchatをpinでき、別の成果ごとにchatを分けることを推奨する。pinはsidebar内の位置だけを変え、contextやaccessを追加しない。これらを根拠に、接続中の通常経路は「repositoryごとのpin済み監視chatがblocking watchから戻る → Codexが質問する → question notificationとActivityの回答待ちへ残る」とする。

ただし公式文書は、Desktop taskが切断中でも外部processからActivityへ新規項目を投入できるとは記載していない。ActivityとOS通知は発見経路であり正本ではない。切断中のrequestは永続snapshotへ保存し、再接続後のstatus-first手順で質問を再表示する。

App Server clientは、自分が接続するApp Server上でpersisted threadを`thread/resume`し、`turn/start`で新しいturnを開始できる。これはdocumentedなprogrammatic continuationである。また、active turnには`turn/steer`で追加inputを送れる。

ただし次は公式文書にないため利用不可と判定する。

- 任意のChatGPT desktop task IDへ別processがmessageを注入するAPI
- 外部supervisorがdesktop taskの表示状態を直接`Needs input`へ変更するAPI
- App Serverの`tool/requestUserInput`やapproval requestをChatGPT mobile pushへ自動転送する契約
- 外部processがChatGPT desktop app内の既存taskをevent駆動でwakeするwebhook

App Serverの`thread/status/changed`とserver requestは、そのApp Server clientがstreamを読み、UI・永続化・回答を実装するためのprotocolである。Desktop/Remote UIとの暗黙の共有を前提にしない。

[Remote connections](https://learn.chatgpt.com/docs/remote-connections)では、mobile appから接続host上の新しいchatを開始し、既存chatを継続し、follow-up、回答、approvalを送れる。hostには最新のdesktop appが起動し、awake、online、同一account/workspaceでsign-inしている必要がある。これは人がRemoteから操作する経路であり、Go supervisorからtaskをwakeするAPIではない。

[Scheduled tasks](https://learn.chatgpt.com/docs/automations)は同じchatへ時刻ベースで戻り、long-running operationをpollできる。ただし各runはChatGPT/Codex workを実行する。filesystem eventで即時wakeする仕組みでも、modelを呼ばないmonitorでもないため、`agent-loop watch`の代替にはしない。

[CLI notifications](https://learn.chatgpt.com/docs/config-file/config-advanced#notifications)の外部`notify`が現在扱うeventは`agent-turn-complete`だけである。よって、監視task未接続時の`needs_input`とsupervisor blockedには引き続き永続outbox + ntfy adapterを使う。

## Token usageと待機

利用可能な観測値は次のとおりである。

- `codex exec --json`の`turn.completed.usage`: input、cached input、output、reasoning output tokens
- App Serverの`thread/tokenUsage/updated`: active threadのlast/total token breakdown
- App Server Goal stateの`tokensUsed`、`tokenBudget`、`timeUsedSeconds`
- App Serverの`account/usage/read`: ChatGPT account token activity summary

これらはusageの観測・budget制御を可能にするが、tool callやlong-running commandのprocess待機時間に関する課金式、保留中connectionの厳密なzero-token、Claude Code monitor相当の無課金契約までは定義していない。したがって、待機中のCodex製品全体についてzero-token/zero-costと断定しない。

`agent-loop`が保証するのは次の狭い境界である。

- `watch`のfsnotify待機とreconciliation pollingはGo内で完結し、model requestを開始しない。
- Codex taskへ定期的に`status`を問い合わせさせない。
- App Server Goal adapterを導入する場合も、token eventとGoal budgetを観測し、無制限continuationを許さない。

## 再確認条件

次のいずれかが公式文書・generated schema・release noteへ追加されたら再評価する。

- Desktop taskを指定してmessage、wake、status、mobile notificationを操作するAPI
- `codex exec`のGoal optionまたはautomation向けGoal lifecycle
- App Server threadとDesktop/Remote taskのidentity・表示同期contract
- waiting tool call、command execution、scheduled runの課金・token保証
- `notify`の`needs_input`、approval、blocked event

再確認時は、確認日、CLI/app version、公式URL、generated schema差分、利用可能/不可/未保証の判定を本書へ追記する。推測やUI観察だけで安全境界を変更しない。
