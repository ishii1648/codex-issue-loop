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

多数のrepositoryを常駐させる場合は、明示的な`webhook.mode: webhook`で共有localhost brokerを利用できます。公開HTTPS endpointはagent-loopが用意せず、既存のreverse proxyから`127.0.0.1`または`::1`へ配送します。署名検証、secret管理、GitHub event、rotation、15分の条件付きREST safety sweep、pollingへのrollbackは[Mac mini常駐運用runbook](docs/mac-mini-runbook.md#12-webhook-brokerとreverse-proxy)を参照してください。

`completion.auto_merge: true`でPR conflictが発生した場合は、同じworktree・branch・PRを使う`resolving_conflict`へ自動遷移します。規定回数後に最終`blocked`となったconflictだけ、原因を修復したうえで次の明示操作から再開します。

```sh
"$agent_loop_bin" retry --repo "$PWD" --issue 123 --json
```

workerが外部環境前提を理由にtyped `blocked`を返すと、supervisorはPID/PGID不在を確認し、run・worktree・branch・dirty changes・session/Goal・answers・resource/base provenanceを`resource_park`へ保持したままactive leaseだけを自動parkします。GitHubは`blocked`のままですが、後続queueは同じresourceを予約できます。`status --json`の`resource_admission.resource_parks`で保存claimとpark状態、`claim_waiting_candidates`でresumeを妨げるIssue/resource/slotを確認できます。

前提をoperatorが解消し、active processがないことを確認した後だけ、次の明示操作で同じworktree・branch・sessionから再開します。park済みclaimは他Issueのactive leaseとworker slotを同じtransactionで再検証し、新しいowner generationを1回だけ取得します。競合中は他Issueのleaseを奪わず拒否します。PR conflict、手動exclusion、security block、failed、completed/closed Issueには適用されません。

```sh
"$agent_loop_bin" resume-blocked --repo "$PWD" --issue 123 --confirm-prerequisite-resolved --json
```

park済みstateでは元のresource集合、base SHA、reservation provenanceを使い、legacy stateでleaseが欠けている場合だけ、既存の厳密なdurable history検証後に保守的な`repo:*` leaseを補います。base SHAを検証できない場合はstateとGitHub labelを変更せず拒否するため、state fileを編集せずremote-tracking branchを復旧して再実行します。

worker完了後、commit/push/PR作成前のpublisherで`durable_base_sha_missing`として最終`failed`になったIssueは、保存済みcompleted resultとdirty worktreeが一致する場合だけpublication-only recoveryを明示要求できます。workerは再実行せず、元のattempt budget、run、worktree、branch、回答、session、resource metadataを保持します。

```sh
"$agent_loop_bin" recover-publication --repo "$PWD" --issue 123 --confirm-prerequisite-resolved --json
```

このコマンドは汎用failed retryではありません。manual exclusion、worker failure、security block、PR conflict、closed Issue、unknown failure provenance、missing/changed resultやworktreeをfail closedで拒否します。

保存済みPRのrequired checks失敗でretry budgetを使い切ったIssueは、同じbranchへ外部修正をpushした後だけ、明示操作で既存PR lifecycleへ戻せます。旧headと異なるclean・fully pushedなhead、open Issue/PR、typed failure provenance、retained leaseを検証し、worker retry budgetはresetしません。

```sh
"$agent_loop_bin" recover-checks --repo "$PWD" --issue 123 --confirm-external-fix --json
```

checksがpendingまたはgreenなら`awaiting_checks`から通常のDraft解除・auto mergeへ収束し、failureならterminal `failed`を維持します。manual/security exclusion、active worker、pending request、dirty/unpushed worktree、別branch/PR/head、closed-without-mergeでは拒否します。

terminal `blocked` / `failed`の保存branchからoperatorがPRを作成・merge済みなのに、durable stateへPR URLが保存されずretained leaseがqueueを止めている場合は、限定adoptionを明示実行できます。

```sh
"$agent_loop_bin" adopt-merged-pr --repo "$PWD" --issue 123 --confirm-merged-pr-adoption --json
```

同一repo・保存branchのmerged PRがちょうど1件で、cleanかつfully pushedなworktree/head、lease owner/generation/base SHA、supervisor-owned terminal provenance、process/request不在がすべて一致する場合だけterminal stateへ採用します。新しいcommit、push、branch、PR、mergeは作成せず、attempt、continuation、session、回答を保持したままPR情報と監査metadataを保存し、leaseを1回だけ解放します。

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

同一repository内並列実行で使う`resources.definitions`、`area:` resource claim、Issue本文の`depends_on` metadata、ready付与前のproducer責務は[Resource admission契約](docs/resource-admission.md)を参照してください。publisherは保存済みbase SHAからtracked/untracked変更pathを検査し、actual resourceが宣言claimを超える場合はcommit・pushせず`needs_input`へ移します。`formatters.go.enabled: true`を明示したrepositoryでは、register済み`gofmt`が変更対象Go fileだけをcommit前に整形します。CIは引き続きread-onlyの`make fmt-check`を最終防衛線とします。

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

- 運用: [Codex Desktop監視task](docs/codex-desktop-monitoring.md)、[Mac mini常駐運用](docs/mac-mini-runbook.md)、[concurrency 2 rollout・rollback](docs/concurrency-rollout.md)、[user-scope Issue作成ルール](docs/user-rules.md)、[doctor・復旧](docs/doctor.md)、[Release・更新](docs/release.md)、[migration](docs/migration.md)、[worktree](docs/worktree-lifecycle.md)
- 設定・設計: [設定例](.agent-loop.example.yaml)、[システム仕様](docs/specification.md)、[App Server Goal adapter](docs/app-server-goal-adapter.md)、[Resource admission契約](docs/resource-admission.md)、[アーキテクチャ](docs/architecture.md)、[要件](docs/requirements.md)、[ADR](docs/adr/)
- 実測: [Mac mini実機E2E](docs/e2e/2026-08-15-mac-mini.md)、[LLM内ループとのtoken消費比較](docs/e2e/2026-08-16-llm-loop-token-comparison.md)
- 開発: [Build・test](Makefile)、[実装状況](docs/implementation.md)、[脅威モデル](docs/threat-model.md)、[セキュリティ運用](docs/security-runbook.md)、[CLI互換性](docs/compatibility.md)
