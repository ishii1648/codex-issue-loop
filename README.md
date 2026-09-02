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

production Releaseはstableだけを配布し、公開だけではrepositoryを更新しません。hostの設定を明示migrationした後、exact stable versionをrepository単位でpreview/applyします。設定は`$HOME/.agent-loop-delivery.yaml`、artifactはimmutable slotへ置かれます。

```sh
"$agent_loop_bin" delivery assignment migrate --json
"$agent_loop_bin" delivery assignment migrate --apply --json
"$agent_loop_bin" delivery assignment status --json
```

詳細は[Repository別stable delivery](docs/per-repository-delivery.md)を参照してください。

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

release binaryと同じtagの設定例を使い、repository、入口label、並列実行境界、base branch、公開方針を対象リポジトリに合わせます。polling間隔やretry、保持期間などの内部運用値は記載しません。設定項目は[設定例](.agent-loop.example.yaml)と[システム仕様](docs/specification.md)を参照してください。

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

多数のrepositoryを常駐させる場合は、明示的な`webhook.mode: webhook`で共有localhost brokerを利用できます。公開HTTPS endpointはagent-loopが用意せず、既存のreverse proxyから`127.0.0.1`または`::1`へ配送します。署名検証、secret管理、GitHub event、rotation、15分の条件付きREST safety sweep、pollingへのrollbackは[Mac mini常駐運用runbook](docs/mac-mini-runbook.md#12-webhook-brokerとreverse-proxy)を参照してください。

`completion.auto_merge: true`でPR conflictが発生した場合は、同じworktree・branch・PRを使う`resolving_conflict`へ自動遷移します。通常の自動lifecycleで処理できない`blocked` / `failed` Issueは、scenario別commandではなく共通のread-only planと型付きresolutionで扱います。

```sh
"$agent_loop_bin" issue plan --repo "$PWD" --issue 123 --json
"$agent_loop_bin" issue resolve --repo "$PWD" --issue 123 --action resume --json
"$agent_loop_bin" issue resolve --repo "$PWD" --issue 123 --action retry-stage --json
"$agent_loop_bin" issue resolve --repo "$PWD" --issue 123 --action adopt-pr --json
"$agent_loop_bin" issue resolve --repo "$PWD" --issue 123 --action cancel --json
```

`issue plan`はcanonical snapshotと現在のprocess、worktree/git、GitHub状態だけを読み、event件数や順序には依存しません。state revision、checkpoint、suspension、全actionの可否と理由をJSONで返し、state/eventを変更しないことも検査します。`issue resolve`はplan時のrevisionとsuspensionを再照合し、`resume` / `retry-stage`では新generationのExecutionLeaseをtransaction内で取得します。`adopt-pr`は同一repository/base/branchの単一merged PRとclean・fully pushedな同一HEADだけを採用し、`cancel`はpending requestをcanceledへ収束させます。

実行中のcapacityは`execution_lease`だけが表し、中断可能なworkspace・session・base/resource・PR情報は`continuation_checkpoint`、理由・recoverability・許可actionは`suspension`が保持します。terminal `blocked` / `failed`はExecutionLeaseとPID/PGIDを保持しないため、ambiguousな1 Issueをquarantineしてもrepository全体のqueueは停止しません。state、label、checkpointは手編集せず、planが拒否した場合は観測された不一致を解消して再planします。

local HTTP/CDP検証が必要なrepositoryだけ、固定の`worker.command_network` localhost-only policyへopt-inできます。既定はnetwork無効です。設定と残余リスクは[localhost-only command network](docs/localhost-network.md)を参照してください。

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

リポジトリごとにLaunchAgent、supervisor、永続状態、ログ、worktreeが分かれるため、異なるリポジトリのループは並列に動作します。同一リポジトリのproduction/self-hostingは安定化期間中`queue.concurrency: 1`で直列実行します。複数workerの実装とresource taxonomyは保持しますが、再有効化にはconformanceとisolated canaryが必要です。resource設定がない既存configも`repo:*`で安全に直列実行されます。

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

同一repository内並列実行で使う`resources.definitions`、`area:` resource claim、Issue本文の`depends_on` metadata、ready付与前のproducer責務は[Resource admission契約](docs/resource-admission.md)を参照してください。publisherは保存済みbase SHAからtracked/untracked変更pathを検査し、actual resourceが宣言claimを超える場合はcommit・pushせず`needs_input`へ移します。`formatters.go.enabled: true`を明示したrepositoryでは、register済み`gofmt`が変更対象Go fileだけをcommit前に整形します。CIは引き続きread-onlyの`make fmt-check`を最終防衛線とします。

Issueが必要とするnetwork、browser/CDP、download、外部時刻前提は[Issue capability admission契約](docs/capability-admission.md)のversioned metadataで宣言します。supervisorはworker profileと実起動経路からeffective capabilityを導出し、claim・lease・worktree・worker spawnより前にfail-closedで照合します。不一致のIssueは副作用なくskipし、compatibleな後続Issueを選択します。

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

出力が`claim_waiting: true`なら回答は保存済みです。`status --json`の`resource_admission.claim_waiting_candidates[].blocked_by`を確認し、相手Issueの通常処理を待ちます。`ready`/`running` labelやstate fileを手動編集せず、同じ回答を別requestへ再送しないでください。

### 4. 停止・再開する

```sh
"$agent_loop_bin" status --repo "$PWD" --json
"$agent_loop_bin" stop --repo "$PWD" --json

"$agent_loop_bin" start --repo "$PWD" --json
"$agent_loop_bin" status --repo "$PWD" --json
```

`stop`は永続状態やIssue worktreeを削除しません。restart、cleanup、復旧は[Mac mini常駐運用runbook](docs/mac-mini-runbook.md)と[doctor・復旧runbook](docs/doctor.md)を参照してください。

### 5. 更新する

新しいstable release artifactをrepository単位で検証してから更新します。previewが返したgenerationをapplyへ渡します。

```sh
"$agent_loop_bin" delivery assignment preview --repo "$PWD" --version v0.9.0 --json
"$agent_loop_bin" delivery assignment apply --repo "$PWD" --version v0.9.0 --expected-generation 1 --json
"$agent_loop_bin" delivery assignment verify --repo "$PWD" --json
```

schema migrationが必要な場合はloopを開始せず、[migration runbook](docs/migration.md)に従ってください。

## 詳細ドキュメント

- 運用: [Codex Desktop監視task](docs/codex-desktop-monitoring.md)、[Mac mini常駐運用](docs/mac-mini-runbook.md)、[Repository別stable delivery](docs/per-repository-delivery.md)、[break-glass repair](docs/break-glass-repair.md)、[concurrency 2 rollout・rollback](docs/concurrency-rollout.md)、[user-scope Issue作成ルール](docs/user-rules.md)、[doctor・復旧](docs/doctor.md)、[Release・更新](docs/release.md)、[migration](docs/migration.md)、[worktree](docs/worktree-lifecycle.md)
- 設定・設計: [設定例](.agent-loop.example.yaml)、[システム仕様](docs/specification.md)、[Resource admission契約](docs/resource-admission.md)、[アーキテクチャ](docs/architecture.md)、[要件](docs/requirements.md)、[ADR](docs/adr/)
- 実測: [Mac mini実機E2E](docs/e2e/2026-08-15-mac-mini.md)、[LLM内ループとのtoken消費比較](docs/e2e/2026-08-16-llm-loop-token-comparison.md)
- 開発: [Build・test](Makefile)、[実装状況](docs/implementation.md)、[脅威モデル](docs/threat-model.md)、[セキュリティ運用](docs/security-runbook.md)、[CLI互換性](docs/compatibility.md)
