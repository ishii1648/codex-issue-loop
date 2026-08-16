# Codex Desktop監視task運用

## 1. 目的と保証範囲

通常の`needs_input`対応は、repositoryごとに用意したCodex Desktopの監視chatで完結させる。監視chatが1回のblocking `watch`へ接続し、結果を受け取ったCodex自身がユーザーへ質問する。これにより、質問通知をOS通知として受け取り、通知を閉じた後もActivityの回答待ちからchatを再発見できる。

この導線の正本はActivityやOS通知ではない。質問、request ID、回答状態は`agent-loop`の永続snapshotに、作業要求と公開結果はGitHub IssueとPull Requestに保持する。Codex Desktopが終了中、Macが停止中、または監視chatのtool callが切断中の期間に、新しい項目がActivityへ追加されることは保証しない。再接続後に`status`または`watch`がsnapshotを読み、既存の未回答requestを再表示する。

`.agent-loop.yaml`の`notifications`は、この通常経路とは別のopt-in外部push adapterである。監視chatが未接続の期間にも通知が必要な場合だけ[スマートフォン直接push通知](notifications.md)を追加する。外部processから任意のDesktop chatをwakeしたり、その表示を直接`Needs input`へ変えたりする非公開機能には依存しない。

## 2. DesktopとmacOSのセットアップ

OpenAI公式の[Projects and chats](https://learn.chatgpt.com/docs/projects)と[Notifications](https://learn.chatgpt.com/docs/notifications)に従い、次を設定する。UI名は利用中のaccountやsurfaceで多少異なる場合がある。

1. ChatGPT desktop appを最新にし、監視対象folderへアクセスできるlocal projectをrepositoryごとに用意する。
2. DesktopのSettingsでnotification permissionとquestion notificationsを有効にする。turn completion通知の表示条件は運用に合わせる。
3. macOSの「システム設定 > 通知 > ChatGPT」で通知を許可し、バナーまたは通知パネルに表示できることを確認する。Focus等で抑止される時間帯も確認する。
4. sidebarのbellからActivityを開けることを確認する。macOSでは`Cmd`+`Option`+`U`でも開閉できる。表示filterに利用可能なら`Pinned`を含める。
5. repository専用の監視chatを作成し、後述の名前へ変更してpinする。pinは見つけやすさだけを変え、権限や参照contextを増やさない。

質問通知とActivityが利用できないaccountまたはsurfaceでは、この導線を受け入れ済みとしない。CLIの永続質問は利用できるため、`status --json`で回答待ちを確認するか、必要なら外部pushを設定する。

## 3. repositoryごとの監視chat

1つの監視chatは1つのrepositoryだけを担当する。task名、primary folder、すべてのCLI呼び出しの`--repo`を一致させる。

| task名 | primary folder | 責務 |
| --- | --- | --- |
| `[LOOP] ishii1648/codex-issue-loop — monitor` | `codex-issue-loop`の絶対path | codex-issue-loopのstatus、質問、回答、blocking watch |
| `[LOOP] ishii1648/zeitreise — monitor` | `zeitreise`の絶対path | zeitreiseのstatus、質問、回答、blocking watch |

Issue作成、調査、実装レビュー等は別chatにする。複数repositoryを1つの監視chatで順番または並列にwatchしない。一方のblocking commandがもう一方を隠し、request IDと回答先を取り違えるためである。

各監視chatの最初の依頼には次の契約を記載する。`<repo>`はそのchat専用の絶対pathへ置き換える。

> `<repo>`専用のagent-loop監視taskとして動作してください。最初と再接続時は`agent-loop status --repo <repo> --json`を1回実行し、`pending_requests`があれば新しいwatchより先に表示してください。なければ`agent-loop watch --repo <repo> --until-attention --json`を1回だけblocking実行してください。`needs_input`ではrequest ID、Issue番号、質問文、理由、推奨案、すべての選択肢IDとlabel、自由記述可否を保ったまま、Desktopの質問UIで私に質問してください。回答は同じrequest IDを指定し、本文を標準入力から`agent-loop answer --repo <repo> --request-id <id> --message-file - --json`へ一度記録してください。`status --json`で記録を確認したら、同じtaskで1回のblocking watchへ戻ってください。Codex側のtimer、定期status、polling loopは作らないでください。

## 4. 質問、回答、再監視

### 4.1 接続と質問

監視chatは接続時に必ず次の順序を守る。

1. `agent-loop status --repo <repo> --json`を1回実行する。
2. `pending_requests`があれば、watchを開始せずrequest ID順に質問する。
3. なければ次を1回実行し、commandが返るまで待つ。

```sh
agent-loop watch --repo /absolute/path/to/repository --until-attention --json
```

`needs_input`では単なる進捗報告でturnを完了せず、ユーザー回答を待つ質問として提示する。最低限、次を欠落させない。

- `issue_number`と`id`（request ID）
- `question`と`reason`
- `recommended_option`。選択肢のIDとlabelを対応させ、推奨であることを明記する
- `options`の全IDとlabel
- `allow_free_text`

複数requestがある場合も結合しない。各回答を対応するrequest IDにだけ記録する。

### 4.2 回答と同じtaskでの再監視

回答本文はshell commandへ埋め込まず、標準入力から渡す。

```sh
agent-loop answer \
  --repo /absolute/path/to/repository \
  --request-id req_... \
  --message-file - \
  --json
```

成功後に`status --json`を1回実行し、対象requestが`answered`、対象Issueが再開待ちへ遷移し、他のpending requestが変化していないことを確認する。同じ回答の再送は冪等だが、異なる二重回答はconflictになる。成功を確認したら、新しいchatを作らず同じ監視chatで1回のblocking `watch`へ戻る。

### 4.3 pollingを作らない

`watch`の待機中に、Codexへ「1分ごとにstatusを確認する」等の定期実行をさせない。filesystem event待機と既定60秒の取りこぼしreconciliationはGo process内で行われ、attentionまでstdoutへ途中結果を返さず、model requestを開始しない。保留中tool callを含むCodex製品全体の厳密なzero-token/zero-costは保証範囲外である。

## 5. 切断、Desktop終了、Mac再起動からの復旧

どの復旧でも、以前のwatch processが生きていると仮定せず、stateやworktreeを削除しない。

| 状況 | 再接続手順 |
| --- | --- |
| chatのtool call切断 | pinした同じchatを開き、`status --json`を1回実行する。pending requestを即時再表示し、なければ新しいblocking `watch`を1回実行する |
| Desktop終了・sign out | Desktopを起動して同じaccount/workspaceへsign inし、pinしたchatを開く。通知設定を確認後、上記のstatus-first手順を行う |
| Mac再起動 | user login後に`agent-loop doctor --repo <repo> --json`と`status --json`でLaunchAgent、schema、supervisorを確認する。Desktopを起動し、repositoryごとのpin済みchatでstatus-first手順を行う |
| 元のchatを復元できない | 同じlocal projectに同名の監視chatを作り直し、対象絶対pathを再確認してstatus-first手順を行う。会話履歴ではなくsnapshotから復元する |

未回答requestがある状態では`status`も`watch`も待機せず返る。OS通知を閉じても回答済みにはならず、接続中の監視chatが質問を提示した後はActivityの「waiting for your response」から再発見できる。切断中に発生したrequestは、再接続したchatが質問を提示するまでActivityへ現れない場合がある。

## 6. 実機受け入れ手順

初回セットアップ、Desktop更新、account/surface変更時は、`codex-issue-loop`と`zeitreise`の両方で次を実施し、日時、app/macOS version、task名、repository path、request ID、結果を運用記録へ残す。実データを含まないテストIssueを使い、秘密値を回答へ含めない。

1. 2つのlocal projectとpin済み監視chatを開き、それぞれの`status --json`が正しいrepositoryを示すことを確認する。
2. 各repositoryで安全なテストIssueを1件ずつ`needs_input`へ遷移させる。
3. 接続中の各監視chatがwatchから戻り、request ID、推奨案、全選択肢を含む質問を表示することを確認する。
4. question notificationのOS通知が1件発生することを確認して閉じる。Activityを開き、該当chatを回答待ちから再発見できることを確認する。
5. 推奨案と異なる選択肢を含む回答も識別できる表示であることを確認し、選んだ回答を記録する。state上のanswer recordが1件だけであること、同じchatが次のblocking watchへ戻ることを確認する。
6. 片方のchatを切断したまま新しいテストrequestを作り、再接続時のstatusで即時再表示されることを確認する。切断中のActivity投入は合否条件にしない。
7. Desktopを終了・再起動して手順6を繰り返す。計画保守時にはMacを再起動し、login後のdoctor/status/snapshot再読込も確認する。
8. 2つのchatが互いのrepositoryのrequest IDを表示・回答せず、両方が独立してwatchへ戻ることを確認する。

repository内の自動testは、完全な質問payloadの即時再読込、再接続、回答の冪等性、回答後のblocking watch再開を検証する。OS通知、Activity表示、macOS権限、実account/surfaceは自動testで代替せず、上記の実機記録を受け入れ証跡とする。
