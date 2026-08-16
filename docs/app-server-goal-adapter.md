# App Server Goal adapter

## 位置づけ

App Server Goal adapterはCodex backendのopt-in検証実装である。最初のworkerは常に既存の`codex exec`でpreflightと実装を連続実行する。その結果が`extended`なら、保存sessionのcontinuationをApp Serverへ切り替え、recoverable failureでfresh sessionが必要な後続attemptも`thread/start`で起動する。`standard`、Claude Code、OpenCodeには適用しない。

責務は次のように分離する。

| 責務 | Go supervisor / 既存adapter | App Server Goal adapter |
| --- | --- | --- |
| Issue選択、claim、queue順序、resource lease | 所有する | 所有しない |
| worktree、worker process group、timeout、LaunchAgent | 所有する | 1 continuationの子processとして動く |
| Git差分監査、commit、push、PR、Issue完了 | 所有する | 所有しない |
| 1 Issueの`extended`目的、Goal status、token/time usage | 永続stateへ保存する | thread-local Goalとして更新・取得する |
| workerの構造化結果と入力要求 | lifecycleへ写像する | protocol eventから返す |

Desktop taskやモバイルRemoteとの表示同期には依存しない。App Server thread IDはworker session IDとしてだけ扱い、任意のDesktop taskへmessageを注入する未公開integrationを仮定しない。

## 有効化

Codex CLI 0.136.0以上を前提とし、versionだけでなくgenerated schemaのcapabilityを検査する。2026-08-17にmacOS arm64上の`codex-cli 0.136.0`と`0.147.0`で公開methodとgenerated schemaを確認した。

```yaml
worker:
  backend: codex
  app_server:
    enabled: true
    goal_token_budget: 200000
    goal_time_budget: 2h
```

`goal_token_budget`は`thread/goal/set.tokenBudget`へ渡す。`goal_time_budget`はApp Server processのdeadlineとしてsupervisor側で強制し、Goal snapshotの`time_budget_seconds`へ保存する。いずれも有効時は正数を必須とする。設定変更後は通常の設定変更手順どおりrepositoryを再登録し、`doctor`を実行する。

## Protocol lifecycle

各extended continuationでstdio App ServerをIssue workerのprocess groupとして起動し、次の順で処理する。

1. `initialize`で`experimentalApi` capabilityを宣言し、`initialized`を通知する。
2. 保存済みthread IDがあれば`thread/resume`し、fresh extended attemptなら`thread/start`する。idleなら`turn/start`、再起動後に`inProgress` turnへrejoinした場合は`turn/steer`を使う。
3. `thread/goal/get`で既存stateを検査し、`thread/goal/set`で安全なIssue番号とworker promptへの参照をobjective、設定値をtoken budget、statusを`active`にする。untrustedなIssue title/bodyはGoal objectiveへ複製しない。
4. `turn/start`では既存worker-result JSON Schemaを`outputSchema`として渡す。approval policyは`never`、sandboxとcwdはIssue worktreeの設定を維持する。
5. `thread/tokenUsage/updated`のtotal breakdownとGoalの`tokensUsed`、`timeUsedSeconds`を収集する。
6. `turn/completed`後にworker resultをdecodeし、`completed`をGoal `complete`、`needs_input` / `blocked`をGoal `blocked`、再試行を`paused`へ写像する。App Serverが`usageLimited`または`budgetLimited`を返した場合はそのterminal statusを優先する。
7. 最終Goalを`thread/goal/get`で再取得し、完了時だけ`thread/goal/clear`する。blocked・budget terminal stateはthread上に残し、次回resume時に再取得する。

永続Issue stateの`goal`にはthread ID、objective、status、token/time budget、tokens/time used、input/cached input/output token breakdownを保存する。Goalが消えてもIssue、worktree、session、requestは失われない。

## 入力・approval

`item/tool/requestUserInput`はserver requestへ空回答を返して現在turnを完了させ、質問、最大3選択肢、自由記述可否を既存worker `needs_input`へ正規化する。supervisorが通常の永続`pending_requests`を作り、GitHubのneeds-input状態へ同期する。ユーザー回答後は保存済み回答をcontinuation promptへ含め、新しいturnで再開する。secret指定の質問には秘密値を回答へ含めない注意を付ける。

`approvalPolicy: never`を防御境界とし、command/file approval requestが届いた場合もadapterは`decline`を返す。追加permissions requestには空のpermission profileを返す。App Serverを有効にしてもsandbox escape、network、外部公開、credential取得の権限は増えない。

## Failure model

| 障害点 | 処理 |
| --- | --- |
| capability不足 | App Serverを起動せず既存`codex exec` adapterを選ぶ |
| process起動、initialize、resume、Goal設定の失敗 | turn開始前なので同じsession/worktreeの`codex exec resume`へfallbackする |
| `turn/start` / `turn/steer`送信後のtransport切断 | 二重実行を避けてfallbackせず、取得済みGoal/sessionと既存worktreeを保存してretryへ移す |
| timeout / cancel | process groupへSIGTERM、grace後にSIGKILLし、Goal usageと`budgetLimited`を可能な範囲で保存する |
| blocking user input | `needs_input`と永続requestへ変換する |
| approval request | denyし、権限を拡大しない |
| app-server / supervisor再起動 | 保存threadを`thread/resume`し、active turnなら`turn/steer`、idleなら`turn/start`する |

App Server protocol logはrun directoryの`codex-app-server.jsonl`、stderrは`codex-app-server.stderr.log`へrotation付きで保存し、設定secretを既存redactorでmaskする。promptはargvにもprotocol logにも複製しない。

## Test

通常suiteのfake App Server contract testは認証もnetworkも使わない。実Codexとのintegrationは課金・認証・既存threadへの影響を避けるためopt-inである。

```sh
AGENT_LOOP_CODEX_APP_SERVER_INTEGRATION=1 \
AGENT_LOOP_CODEX_THREAD_ID=<disposable-thread-id> \
go test ./internal/worker -run '^TestCodexAppServerIntegration$' -count=1 -v
```

integration用threadは使い捨てとし、書き込み不要のworktreeで実行する。testはqueue、Issue label、PR、LaunchAgentを操作しない。

## Rollback

最短のrollbackは`worker.app_server.enabled: false`へ戻し、repositoryを再登録してsupervisorをrestartすることである。次の実行から既存`codex exec` / `codex exec resume`だけを使う。adapter導入前の古いbinary自体へrollbackする場合は、strict YAML decoderが未知fieldを拒否するため`app_server` block全体を設定から削除してから再登録する。`goal` field、session ID、worktree、回答、run stateは互換な追加fieldとして保持されるため、state fileやworktreeを削除しない。必要なら対象threadのGoalをCodexの公開UIまたはApp Server `thread/goal/clear`で後からclearできるが、adapter rollbackの必須条件ではない。
