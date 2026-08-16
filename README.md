# codex-issue-loop

GitHub Issueをキューとして、着手可能なIssueが存在する限りCodex CLI workerを繰り返し実行する、Apple Silicon macOS向けの常駐ループです。

## 使い方

### 1. Macへインストールする

Macには`git`、`gh` 2.69.0以降、`codex` 0.136.0以降を用意し、LaunchAgentを動かすものと同じmacOSユーザーでGitHubとCodexへログインします。

```sh
gh auth status
codex login status
```

最新のGitHub ReleaseからApple Silicon用artifactを取得し、checksum、provenance、versionを確認してからインストールします。

```sh
agent_loop_version="$(gh release view \
  --repo ishii1648/codex-issue-loop \
  --json tagName \
  --jq .tagName)"
agent_loop_download_dir="$PWD/agent-loop-release-$agent_loop_version"

mkdir -p "$agent_loop_download_dir"
gh release download "$agent_loop_version" \
  --repo ishii1648/codex-issue-loop \
  --dir "$agent_loop_download_dir"

cd "$agent_loop_download_dir"
shasum -a 256 -c checksums.txt
gh attestation verify agent-loop_Darwin_arm64 \
  --repo ishii1648/codex-issue-loop
chmod 0755 agent-loop_Darwin_arm64
./agent-loop_Darwin_arm64 version --json
./agent-loop_Darwin_arm64 install --json
```

インストール後のCLIは次の場所にあります。以降の例ではこのpathを使用します。

```sh
agent_loop_bin="$HOME/Library/Application Support/codex-issue-loop/bin/agent-loop"
"$agent_loop_bin" doctor --json
```

### 2. 対象リポジトリを準備する

対象リポジトリをMacへcloneし、rootに`.agent-loop.yaml`を置きます。設定fileはリポジトリへcommitし、tokenや秘密値は記載しません。

```sh
git clone git@github.com:owner/repository.git
cd repository
test -f .agent-loop.yaml
```

新しく設定を作る場合は、インストールするrelease tagと同じrevisionの設定例を取得します。`main`の設定例には未releaseのfieldが追加されることがあるため、release binaryと`main`の設定例を組み合わせません。

```sh
agent_loop_version="$(gh release view \
  --repo ishii1648/codex-issue-loop \
  --json tagName \
  --jq .tagName)"

gh api \
  -H 'Accept: application/vnd.github.raw+json' \
  "repos/ishii1648/codex-issue-loop/contents/.agent-loop.example.yaml?ref=$agent_loop_version" \
  > .agent-loop.yaml
```

取得した設定例について、最低限次を対象リポジトリに合わせます。

- `github.repo`
- ready、exclude、状態遷移に使うGitHub label
- queueの選択順
- `git.base_branch`と`git.branch_prefix`
- worker timeoutと継続回数
- draft PR作成、CI成功後の自動merge、merge後のIssue close方針

`ready_labels`は「依存関係を含め、今すぐ自動着手してよい」という明示的な境界です。Issue本文独自の依存記法は自動解釈しないため、依存関係を使うリポジトリでは、依存解決を確認するproducerまたはautomationだけがready labelを付けます。

### 3. GitHubラベルを作成する

まず変更計画だけを確認し、問題がなければ不足ラベルを作成します。既存ラベルの色・説明は上書きせず、ラベルの削除も行いません。

```sh
agent_loop_bin="$HOME/Library/Application Support/codex-issue-loop/bin/agent-loop"

"$agent_loop_bin" bootstrap-labels --repo "$PWD" --json
"$agent_loop_bin" bootstrap-labels --repo "$PWD" --apply --json
"$agent_loop_bin" bootstrap-labels --repo "$PWD" --json
```

最後のpreviewに`create`が残っていないことを確認します。

### 4. LaunchAgentへ登録して起動する

`register`はclone先の絶対pathを登録し、リポジトリ別の永続状態とユーザーLaunchAgentを作成します。リポジトリを別のpathへcloneし直した場合は再登録が必要です。

LaunchAgentは対話shellの初期化fileを読みません。`gh`をaquaで管理している場合は、aqua proxyではなく実体binaryのdirectoryを先頭へ追加したPATHで登録します。aquaを使っていない環境では現在のPATHをそのまま使用します。

```sh
agent_loop_bin="$HOME/Library/Application Support/codex-issue-loop/bin/agent-loop"
agent_loop_register_path="$PATH"

if command -v aqua >/dev/null 2>&1; then
  agent_loop_gh_binary="$(aqua which gh 2>/dev/null || true)"
  if [ -n "$agent_loop_gh_binary" ]; then
    agent_loop_register_path="$(dirname "$agent_loop_gh_binary"):$agent_loop_register_path"
  fi
fi

env PATH="$agent_loop_register_path" \
  "$agent_loop_bin" register --repo "$PWD" --json
"$agent_loop_bin" start --repo "$PWD" --json
sleep 3
"$agent_loop_bin" doctor --repo "$PWD" --json
"$agent_loop_bin" status --repo "$PWD" --json
```

repository単位の`doctor`は停止中のsupervisorを異常として扱うため、初回は`start`の後に実行します。`doctor`が`ok: true`、`status`のLaunchAgentが`running`、supervisorが`polling`またはworker実行中の状態を返すことを確認します。同じリポジトリを複数hostから同時に処理しないでください。

すでにaqua proxyのpathで登録して起動に失敗している場合は、状態を保持したまま停止し、同じ手順で再登録します。

```sh
"$agent_loop_bin" stop --repo "$PWD" --json
env PATH="$agent_loop_register_path" \
  "$agent_loop_bin" register --repo "$PWD" --json
"$agent_loop_bin" start --repo "$PWD" --json
```

### 5. Issueをキューへ投入する

open Issueへmanifestのready labelを付けるとキューへ入ります。既定例では次の操作です。

```sh
gh issue edit 123 --add-label codex-loop:ready
```

supervisorはIssueをclaimし、専用worktreeでCodex workerを実行します。workerはcommit、push、PR作成を直接行わず、構造化結果を返します。supervisorが検証済みの変更をcommit・pushしてdraft PRを作り、CIを監視します。

CIがすべて成功するとPRをReady for reviewへ移します。既定の`completion.auto_merge: false`では、ここから人手によるmergeを待ちながら次のIssueへ進みます。`completion.auto_merge: true`にした対象リポジトリでは、base branchより遅れているPRを更新してCIを再確認し、squash mergeまで行ってから次のIssueへ進みます。いずれの場合もIssueの完了・`close_issue`の適用はPRのmerge確認後です。

```yaml
completion:
  create_draft_pr: true
  auto_merge: false
  close_issue: true
```

CI失敗時はPRをdraftのまま維持し、同じworktreeと失敗理由をCodex workerへ渡して再試行します。merge conflictは自動解消せず、対象Issueをattentionが必要な状態にします。

### 6. 状態を確認・監視する

現在の永続状態は`status`で確認できます。

```sh
agent_loop_bin="$HOME/Library/Application Support/codex-issue-loop/bin/agent-loop"
"$agent_loop_bin" status --repo "$PWD" --json
```

入力または復旧操作が必要になるまで待つ場合は、短い間隔で`status`を繰り返さず、blocking `watch`を1回実行します。内部ではfilesystem eventとreconciliation pollingを併用します。

```sh
"$agent_loop_bin" watch \
  --repo "$PWD" \
  --until-attention \
  --json
```

Codex Remoteを使う場合は、スマートフォンの監視用taskへ対象リポジトリの絶対pathを伝え、`doctor`、`status`、1回の`watch`を順に実行させます。監視taskが切断されてもLaunchAgent上のループは継続します。

### 7. 質問へ回答する

`needs_input`になった場合は、`watch`または`status`が返したrequest IDを変えずに回答します。回答に秘密値を含めないでください。

```sh
printf '%s\n' '回答内容' | "$agent_loop_bin" answer \
  --repo "$PWD" \
  --request-id req_... \
  --message-file - \
  --json
```

回答後は同じ監視taskから`watch`を再実行できます。

### 8. 停止・再開する

`stop`は永続状態、Issueごとのworktree、未回答requestを削除せずにLaunchAgentを停止します。

```sh
"$agent_loop_bin" stop --repo "$PWD" --json
"$agent_loop_bin" status --repo "$PWD" --json

"$agent_loop_bin" start --repo "$PWD" --json
"$agent_loop_bin" status --repo "$PWD" --json
```

設定や認証を変更してLaunchAgentを再起動する場合は`restart`を使います。

```sh
"$agent_loop_bin" restart --repo "$PWD" --json
```

### 9. 更新する

新しいrelease artifactのchecksum、provenance、versionをインストール時と同じ手順で確認したあと、そのartifactから`update`を実行します。

```sh
./agent-loop_Darwin_arm64 update --json
agent_loop_bin="$HOME/Library/Application Support/codex-issue-loop/bin/agent-loop"
"$agent_loop_bin" doctor --json
```

schema migrationが必要と表示された場合はloopを開始せず、migration runbookに従ってbackupとpreviewを確認してから適用します。

## For development

- [Build・test](Makefile)
- [アーキテクチャ概要](docs/architecture.md)
- [MVP実装状況](docs/implementation.md)
- [要件定義](docs/requirements.md)
- [システム仕様](docs/specification.md)
- [脅威モデル](docs/threat-model.md)
- [セキュリティ運用](docs/security-runbook.md)
- [CLI互換性](docs/compatibility.md)
- [GitHubラベル](docs/github-labels.md)
- [Queue ordering](docs/queue-ordering.md)
- [doctor・復旧](docs/doctor.md)
- [Mac mini常駐運用](docs/mac-mini-runbook.md)
- [スマートフォン直接push通知](docs/notifications.md)
- [Release・install・update](docs/release.md)
- [永続schema migration](docs/migration.md)
- [Worktree lifecycle](docs/worktree-lifecycle.md)
- [Codex公式仕様確認](docs/codex-capability-review.md)
- [ADR-0001: macOS実行モデル](docs/adr/0001-macos-execution-model.md)
- [ADR-0002: 単一ホスト並列化と複数ホスト冗長化](docs/adr/0002-concurrency-and-multi-host.md)
- [ADR-0003: event通知方式](docs/adr/0003-event-notification.md)
