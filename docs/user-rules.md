# user-scope Issue作成ルール

`agent-loop init`は、通常のCodex / Claude Codeセッションが`.agent-loop.yaml`のあるrepositoryで変更依頼を受けたとき、自ら実装せず、ready label付きIssueとしてloopへ委譲するためのuser-level ruleを管理する。repositoryにはruleをcommitしない。

## Previewと適用

既定ではCodexとClaude Codeの両方を対象にする。`--apply`なしのpreviewは、agent-loopの内部ディレクトリ、対象file、backupを含めて一切作成・変更しない。

```sh
agent-loop init --json
agent-loop init --apply --json
agent-loop init --json
```

最後の再実行では各targetが`status: current`、`action: none`、`apply_result: not_applied`となる。適用を再実行した場合も`apply_result: unchanged`となり、fileを変更しない。対象を限定する場合は次のように指定する。

```sh
agent-loop init --agents codex --json
agent-loop init --agents codex --apply --json
agent-loop init --agents claude --json
```

JSONには`agent`、user scope上の`path`、実際の書き込み先`resolved_path`、`symlink`、`status`、予定`action`、`applied`、`apply_result`、`backup_path`を出力する。`status`は`missing`、`current`、`outdated`、`conflict`のいずれかである。

## 対象file

Codexは`$CODEX_HOME/AGENTS.md`、`CODEX_HOME`未指定時は`~/.codex/AGENTS.md`を使う。既存内容のうち、次のversion付きmarkerで囲まれたblockだけを管理する。

```md
<!-- agent-loop:rules:start version=1 -->
...
<!-- agent-loop:rules:end -->
```

非空の`AGENTS.override.md`が`CODEX_HOME`直下にある場合、Codexではそちらが優先されるため、有効なoverride fileへ管理blockを追加・更新し、そのpathを結果に表示する。空のoverride fileは対象を切り替えない。

Claude Codeは`$CLAUDE_CONFIG_DIR/rules/codex-issue-loop.md`、`CLAUDE_CONFIG_DIR`未指定時は`~/.claude/rules/codex-issue-loop.md`を使う。このfileはagent-loop専用であり、`paths` frontmatterを付けないため、全projectで読み込まれるuser-level ruleになる。

markerの片側欠落、複数block、解決できないsymlink、agent-loopが所有できない既存Claude ruleは`conflict`となり、`--apply`でも上書きしない。symlinkの場合はlink自体を置換せず、解決先をatomicに更新する。既存fileのpermissionと管理block外の内容は維持する。

## Backupと復旧

既存fileを更新する直前に、変更前の内容を次の管理領域へ同じpermissionでbackupする。

```text
$AGENT_LOOP_HOME/user-rules-backups/<timestamp>/<agent>/<file>
```

`AGENT_LOOP_HOME`未指定時は`~/Library/Application Support/codex-issue-loop/user-rules-backups/`となる。作成結果の`backup_path`と`resolved_path`を保存する。復元時は対象セッションを終了し、JSONで示された実pathへbackupを戻してからpreviewする。

```sh
cp -p "$backup_path" "$resolved_path"
agent-loop init --agents codex --json
```

Claude Codeを復元する場合は`--agents claude`を指定する。backupは変更前file全体であり、Codexの管理外内容も復元される。`install`、`update`、`doctor`の自動修復、`uninstall`はuser ruleを変更・削除しない。`doctor`は不足・旧version・競合を検出したとき、明示的な`agent-loop init`をremediationとして表示するだけである。

## Ruleの判断境界

埋め込まれるruleは次だけを指示する。

- 変更依頼では対象repositoryを確定し、rootまたはdefault branchの`.agent-loop.yaml`を確認する。
- 設定があり、agent-loop implementation workerでなければ、重複Issueを確認してから自ら実装せずIssueを起票する。設定がなければ通常どおり作業する。
- `.agent-loop.yaml`の`github.ready_labels`に設定されたready labelだけを使い、Issue作成後に再取得して確認する。不明・不足時は推測やlabel作成をしない。
- implementation workerは割り当てられたIssueを実装し、同じ依頼を再起票しない。
- 読み取り専用タスクでは起票しない。ユーザーがloopを使わないよう明示した場合は直接実装してよい。
