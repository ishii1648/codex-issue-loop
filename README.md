# codex-issue-loop

GitHub Issueをキューとして、着手可能なIssueが存在する限りCodex CLI、Claude Code、またはOpenCode workerを繰り返し実行する、Apple Silicon macOS向けの常駐ループです。

## セットアップ

Macへのinstallと、対象リポジトリごとの設定・登録・起動を行います。

### 1. Macへインストールする

Macへ`git`、`gh` 2.69.0以降と、選択するworker runtimeを用意し、LaunchAgentを動かすmacOSユーザーでGitHubとruntimeへログインします。backend未指定時は後方互換な`codex`です。

```sh
gh auth status
codex login status
```

workerは次のように選択します。`command`を省略するとbackendごとの既定commandを使います。設定変更後は`register`を再実行し、絶対pathとruntime versionを記録してください。

```yaml
worker:
  backend: claude-code
  model: claude-sonnet-4-5
  variant: high
```

```yaml
worker:
  backend: opencode
  model: opencode-go/kimi-k2.7-code
  variant: high
```

OpenCodeのmodelは最初の`/`でprovider IDとmodel IDへ分割されます。OpenCode Goも専用backendではなく`opencode-go/<model-id>`としてそのまま扱います。credentialはmanifest、argv、plist、stateへ保存せず、各runtimeの既存ユーザー認証領域を利用します。

最新のGitHub ReleaseからApple Silicon用artifactを取得し、checksum、provenance、versionを確認してインストールします。

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

agent_loop_bin="$HOME/Library/Application Support/codex-issue-loop/bin/agent-loop"
"$agent_loop_bin" init --json
"$agent_loop_bin" init --apply --json
"$agent_loop_bin" init --json
"$agent_loop_bin" doctor --json
```

`init`はCodexとClaude Codeのuser scopeへ、`.agent-loop.yaml`があるrepositoryの変更依頼をIssueへ委譲するルールを設定します。preview、対象agentの限定、競合、backupと復旧は[user-scope Issue作成ルール](docs/user-rules.md)を参照してください。`install`、`update`、`doctor`、`uninstall`がこの設定を暗黙に変更することはありません。

更新・rollbackを含む詳細は[Release・install・update](docs/release.md)を参照してください。

### 2. 対象リポジトリを準備する

対象リポジトリをMacへcloneし、rootに`.agent-loop.yaml`を置きます。設定にはtokenや秘密値を記載しません。

```sh
git clone https://github.com/owner/repository.git
cd repository

agent_loop_version="$(gh release view \
  --repo ishii1648/codex-issue-loop \
  --json tagName \
  --jq .tagName)"

gh api \
  -H 'Accept: application/vnd.github.raw+json' \
  "repos/ishii1648/codex-issue-loop/contents/.agent-loop.example.yaml?ref=$agent_loop_version" \
  > .agent-loop.yaml
```

release binaryと同じtagの設定例を使い、`github.repo`、label、base branch、worker、完了条件を対象リポジトリに合わせます。設定項目は[設定例](.agent-loop.example.yaml)と[システム仕様](docs/specification.md)を参照してください。

### 3. ラベルを作成して起動する

ラベルの変更計画を確認してから不足分を作成し、リポジトリをLaunchAgentへ登録します。

```sh
agent_loop_bin="$HOME/Library/Application Support/codex-issue-loop/bin/agent-loop"

"$agent_loop_bin" bootstrap-labels --repo "$PWD" --json
"$agent_loop_bin" bootstrap-labels --repo "$PWD" --apply --json
"$agent_loop_bin" bootstrap-labels --repo "$PWD" --json

"$agent_loop_bin" register --repo "$PWD" --json
"$agent_loop_bin" start --repo "$PWD" --json
sleep 3
"$agent_loop_bin" doctor --repo "$PWD" --json
"$agent_loop_bin" status --repo "$PWD" --json
```

`doctor`が`ok: true`を返し、LaunchAgentとsupervisorが稼働していることを確認します。LaunchAgentのPATH、aqua利用時の登録、初回セットアップの詳細は[Mac mini常駐運用runbook](docs/mac-mini-runbook.md)を参照してください。

`completion.auto_merge: true`でPR conflictが発生した場合は、同じworktree・branch・PRを使う`resolving_conflict`へ自動遷移します。規定回数後に最終`blocked`となったconflictだけ、原因を修復したうえで次の明示操作から再開します。

```sh
"$agent_loop_bin" retry --repo "$PWD" --issue 123 --json
```

### 4. 複数リポジトリを並列実行する

CLIのinstallはMacごとに1回だけ行います。各リポジトリへ`.agent-loop.yaml`とラベルを用意し、それぞれを`register`、`start`します。

```sh
agent_loop_bin="$HOME/Library/Application Support/codex-issue-loop/bin/agent-loop"

"$agent_loop_bin" register --repo /absolute/path/to/repo-a --json
"$agent_loop_bin" register --repo /absolute/path/to/repo-b --json

"$agent_loop_bin" start --repo /absolute/path/to/repo-a --json
"$agent_loop_bin" start --repo /absolute/path/to/repo-b --json

"$agent_loop_bin" doctor --repo /absolute/path/to/repo-a --json
"$agent_loop_bin" doctor --repo /absolute/path/to/repo-b --json
```

リポジトリごとにLaunchAgent、supervisor、永続状態、ログ、worktreeが分かれるため、異なるリポジトリのループは並列に動作します。同一リポジトリでは`resources.definitions`とIssueの`area:` claimを設定した場合に`queue.concurrency`まで並列実行できます。resource設定がない既存configは`queue.concurrency: 1`と`repo:*`で安全に直列実行されます。

- 同じGitHubリポジトリを複数のcloneやhostから同時に動かさないでください。
- `status`、`watch`、`stop`などでは常に`--repo`で対象を明示してください。
- Codexの利用枠とMacのCPU、メモリ、ディスク、networkは全ループで共有されます。

並列化と複数hostの制約は[ADR-0002](docs/adr/0002-concurrency-and-multi-host.md)を参照してください。

## 運用

登録済みリポジトリのIssue投入、監視、回答、停止、更新を行います。

### 1. Issueをキューへ投入する

着手可能なopen Issueへready labelを付けます。既定例では次のとおりです。

```sh
gh issue edit 123 --add-label codex-loop:ready
```

PR作成、CI再試行、自動merge、Issue closeの動作は`.agent-loop.yaml`で設定します。詳細は[システム仕様](docs/specification.md)を参照してください。

同一repository内並列実行で使う`resources.definitions`、`area:` resource claim、Issue本文の`depends_on` metadata、ready付与前のproducer責務は[Resource admission契約](docs/resource-admission.md)を参照してください。publisherは保存済みbase SHAからtracked/untracked変更pathを検査し、actual resourceが宣言claimを超える場合はcommit・pushせず`needs_input`へ移します。

### 2. 状態を確認・監視する

```sh
"$agent_loop_bin" status --repo "$PWD" --json

"$agent_loop_bin" watch \
  --repo "$PWD" \
  --until-attention \
  --json
```

短い間隔で`status`を繰り返さず、入力や復旧操作が必要になるまで1回のblocking `watch`で待機します。Codex Desktopではrepositoryごとに専用chatをpinし、質問通知とActivityの回答待ちを通常の発見経路にします。セットアップ、回答、切断・再起動後の再接続、複数repositoryの分離は[Codex Desktop監視task運用](docs/codex-desktop-monitoring.md)を参照してください。Codex Remoteからの監視方法は[Mac mini常駐運用runbook](docs/mac-mini-runbook.md)を参照してください。

### 3. 質問へ回答する

`watch`または`status`が返したrequest IDを変えずに回答します。回答に秘密値を含めないでください。

```sh
printf '%s\n' '回答内容' | "$agent_loop_bin" answer \
  --repo "$PWD" \
  --request-id req_... \
  --message-file - \
  --json
```

### 4. 停止・再開する

```sh
"$agent_loop_bin" status --repo "$PWD" --json
"$agent_loop_bin" stop --repo "$PWD" --json

"$agent_loop_bin" start --repo "$PWD" --json
"$agent_loop_bin" status --repo "$PWD" --json
```

`stop`は永続状態やIssue worktreeを削除しません。restart、cleanup、復旧は[Mac mini常駐運用runbook](docs/mac-mini-runbook.md)と[doctor・復旧runbook](docs/doctor.md)を参照してください。

### 5. 更新する

新しいrelease artifactをインストール時と同じ手順で検証してから更新します。

```sh
./agent-loop_Darwin_arm64 update --json
"$agent_loop_bin" doctor --json
```

schema migrationが必要な場合はloopを開始せず、[migration runbook](docs/migration.md)に従ってください。

## 詳細ドキュメント

- 運用: [Codex Desktop監視task](docs/codex-desktop-monitoring.md)、[Mac mini常駐運用](docs/mac-mini-runbook.md)、[concurrency 2 rollout・rollback](docs/concurrency-rollout.md)、[user-scope Issue作成ルール](docs/user-rules.md)、[doctor・復旧](docs/doctor.md)、[Release・更新](docs/release.md)、[migration](docs/migration.md)、[外部push通知](docs/notifications.md)、[worktree](docs/worktree-lifecycle.md)
- 設定・設計: [設定例](.agent-loop.example.yaml)、[システム仕様](docs/specification.md)、[App Server Goal adapter](docs/app-server-goal-adapter.md)、[Resource admission契約](docs/resource-admission.md)、[アーキテクチャ](docs/architecture.md)、[要件](docs/requirements.md)、[ADR](docs/adr/)
- 実測: [Mac mini実機E2E](docs/e2e/2026-08-15-mac-mini.md)、[LLM内ループとのtoken消費比較](docs/e2e/2026-08-16-llm-loop-token-comparison.md)
- 開発: [Build・test](Makefile)、[実装状況](docs/implementation.md)、[脅威モデル](docs/threat-model.md)、[セキュリティ運用](docs/security-runbook.md)、[CLI互換性](docs/compatibility.md)
