# CLI互換性マトリクス

最終確認日: 2026-08-18

`agent-loop`はversion番号だけでCLIの挙動を推測せず、起動時と`doctor`で実際のhelp出力から必要なcapabilityを検査する。minimum versionは継続的に検証する下限であり、capability検査を省略する条件ではない。

## 対応範囲

| CLI | minimum supported | 実環境で確認したversion | 必須capability |
| --- | --- | --- | --- |
| Codex CLI | 0.136.0 | 0.136.0、0.147.0 / macOS arm64 | `codex exec`の`--json`、`--output-schema`、`--output-last-message`、`--sandbox`、`--cd` |
| Claude Code | 2.1.119 | fake runtime conformance | print mode、stream JSON、JSON Schema、resume、model、effort、`dontAsk`、OS sandbox hard-fail設定 |
| OpenCode | 1.1.1 | fake Server API conformance | loopback `serve`、session/message/abort API、JSON Schema output、provider/model、variant、inline permission policy |
| GitHub CLI | 2.69.0 | 2.69.0 / macOS arm64 | Issue listの`--json`、`--limit`、`--label`、`--assignee`、`--milestone`、label追加・削除、comment追加 |

Codexの`exec resume`と、その`--json`、`--output-schema`、`--output-last-message`は任意capabilityとして扱う。probeはadapterと同じ`codex exec --cd . resume --help`の順序を検証する。利用できる場合は保存したsession IDを再開し、利用できない場合は同じIssue worktree、run ID、回答履歴をpromptへ再構成して新規sessionを起動する。新規sessionでもsandboxと承認禁止の制約は変えない。

## App Server Goal adapter削除時の互換性

`worker.app_server`は現行設定ではない。既存repositoryの安全な更新を妨げないため、`enabled: false`だけをinertなlegacy互換として読み込む。`enabled: true`と未知fieldはstrict YAML decoderまたはvalidationで拒否し、App Server経路を暗黙に再有効化しない。legacy blockは次回の意図的な設定更新時に削除でき、更新前の一括書き換えを必須としない。

旧durable stateの`goal` snapshotは読み込み時に無視され、次の通常state更新で書き戻されない。`session_id` / `session`、worktree、branch、answers、attempts、continuationsなどの継続情報はそのまま保持されるため、state fileやworktreeを削除しない。

App Server方式は恒久禁止ではなくdeferred featureである。中核worker lifecycleが安定し、`codex exec resume`では満たせない具体的要件、token/time budget、再接続、二重実行防止の継続的なintegration/replay testを定義できた場合に、[#189](https://github.com/ishii1648/codex-issue-loop/issues/189)とは別のIssueで再評価する。

Claude Codeは`claude -p`へpromptをstdinで渡し、`--json-schema`の`structured_output`と`session_id`を正規化する。OpenCodeはrunごとにloopback serverをprocess group内で起動し、promptをmessage API bodyへ渡す。timeout/cancel時はsession abortを試行してからserver process groupを終了する。OpenCode CLIのprompt argv fallbackは実装しない。

最低version未満、Codexの構造化初回実行、またはGitHubのIssue取得・label・comment操作が欠ける環境は安全なfallbackを定義できないため、`doctor`を失敗させ、supervisorの開始を拒否する。

## 既知の形式差

- session IDはJSONL event内の`thread_id`または`session_id`を受け付ける。
- event wrapper内に同じfieldが入る形式と、`thread.id`または`session.id`形式も再帰的に受け付ける。
- 全backendのprocessのOS working directoryとworkspace APIへ渡すdirectoryは、初回session、保存sessionのresume、自動continuation、回答後のresume、generic checkpointからのresume、resume非対応fallbackのすべてで同じ正規化済みIssue worktreeへ固定する。CLIには`codex exec --cd <worktree> resume ...`のように`--cd`を`resume`より前へ置き、追加のwritable directoryは渡さない。
- 初回spawn前にworktree path、branch、Git common dir、repository ID、main checkoutとの非同一性をprovenanceとして保存する。以後のspawn直前にrun/session/lease owner generationとともに再検証し、欠損・symlink・別branch/repository・provenance不一致ではbackendを起動せず`blocked`へ収束する。`worker_workspace_validated`、`worker_workspace_rejected`、`worker_process_started` eventでexpected/actual cwdと検証結果を監査できる。
- v4の旧scenario別fieldはstopped-host migration decoderだけがgeneric checkpointへ変換する。session/workspace/lease/result/PR identityがcanonical snapshotで不足または矛盾する場合は対象Issueをquarantineし、event historyやerror文言からprovenanceを合成しない。schema v5の旧field/status/syncはruntimeで拒否する。
- GitHub Issueは`gh issue list --limit 1000`で取得し、Issue番号順に選択する。100件を超えるqueueをfixtureで検証する。1000件を超える単一queueは現在の対応上限である。

## 確認手順

実CLIを更新する前後に、対象Macで次を実行する。

```sh
codex --version
codex exec --help
codex exec --cd . resume --help
claude --version
claude --help
opencode --version
opencode serve --help
opencode models
gh --version
gh issue list --help
gh issue edit --help
gh issue comment --help
agent-loop doctor --repo /absolute/path/to/repository --json
```

`doctor`の`CODEX_CLI_COMPATIBLE`と`GH_CLI_COMPATIBLE` diagnosticが`ok`で、detailのversionとminimumが意図どおりであることを確認する。`session_resume=fresh-session-fallback`が表示された場合はresumeのみ非対応である。開始後はテスト用Issueで初回実行と回答後の継続を1回ずつ確認する。

互換性範囲を更新するときは、次を同じPull Requestへ含める。

1. 新旧CLIのhelp出力に対応するcapability fixtureとparser test
2. 初回実行、resumeまたは新規session fallback、100件超のIssue取得test
3. 本文書とREADMEのminimum version・実環境確認version
4. 実CLIによる`doctor`結果とテスト用IssueのE2E結果

参照する一次資料は、[Codex CLI command line options](https://learn.chatgpt.com/docs/developer-commands?surface=cli)、[Claude Code CLI reference](https://code.claude.com/docs/en/cli-usage)、[Claude Code sandboxing](https://code.claude.com/docs/en/sandboxing)、[OpenCode CLI](https://opencode.ai/docs/cli/)、[OpenCode Server](https://opencode.ai/docs/server/)、[GitHub CLI manual](https://cli.github.com/manual/gh)である。更新時は必ず最新の公式仕様と実際のhelp出力を照合する。
