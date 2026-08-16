# codex-issue-loop

GitHub Issue をキューとして、着手可能な Issue が存在する限り Codex CLI のワーカーを繰り返し実行する、macOS 向けの常駐ループです。

Go製の`agent-loop` CLI、launchd supervisor、GitHub/Codex adapter、永続状態、監視・回答フローを含みます。現在は初期MVPです。本番リポジトリへ登録する前に、テスト用リポジトリで権限・ラベル・worker promptを確認してください。

## Documents

- 設計: [アーキテクチャ](docs/architecture.md)、[要件](docs/requirements.md)、[仕様](docs/specification.md)、[実装状況](docs/implementation.md)
- 運用: [Mac mini runbook](docs/mac-mini-runbook.md)、[doctor・復旧](docs/doctor.md)、[GitHubラベル](docs/github-labels.md)、[CLI互換性](docs/compatibility.md)
- 安全性: [脅威モデル](docs/threat-model.md)、[セキュリティrunbook](docs/security-runbook.md)

![codex-issue-loop アーキテクチャ](docs/images/architecture-overview.png)

## 設計の要点

- 独立した`agent-loop` CLIを`launchd`が常駐させ、Codexのtaskやgoalからループの寿命を分離する
- Issueごとに専用worktreeと`codex exec`ワーカーを用意する
- 状態と質問を永続化し、Codex Skill経由で監視・回答する
- `watch`はイベント通知と定期的なreconciliationを組み合わせる

## Requirements

- Apple Silicon macOS
- Go 1.22以降（ソースからビルドする場合）
- `git`
- `gh` 2.69.0以降と対象リポジトリへの認証
- `codex` 0.136.0以降と有効なCodex認証
- ログイン中のmacOSユーザーセッション（LaunchAgentを使用）

Mac miniでは、macOSの「ディスプレイがオフのときに自動でスリープさせない」を有効にすることを推奨します。初回導入、Codex Remote接続、スマートフォン操作、障害復旧、backup、更新、撤去は[Mac mini常駐運用runbook](docs/mac-mini-runbook.md)に従ってください。

## Build and test

```sh
make ci
./bin/agent-loop --version
```

`make ci`はformat、schema、依存関係、test、race、vet、脆弱性、buildを含むGitHub Actionsと同じ品質ゲートを実行します。個別targetと障害注入ケースは[テストマトリクス](docs/testing.md)を参照してください。

## Setup

1. 対象リポジトリに`.agent-loop.yaml`を追加します。[設定例](.agent-loop.example.yaml)を参照してください。
2. CLIとCodex Skillをユーザー領域へインストールします。
3. 必須GitHubラベルの変更計画を確認し、作成します。
4. 対象リポジトリを登録し、診断後に開始します。

```sh
./bin/agent-loop install
~/Library/Application\ Support/codex-issue-loop/bin/agent-loop bootstrap-labels --repo /absolute/path/to/repository
~/Library/Application\ Support/codex-issue-loop/bin/agent-loop bootstrap-labels --repo /absolute/path/to/repository --apply
~/Library/Application\ Support/codex-issue-loop/bin/agent-loop register --repo /absolute/path/to/repository
~/Library/Application\ Support/codex-issue-loop/bin/agent-loop doctor --repo /absolute/path/to/repository
~/Library/Application\ Support/codex-issue-loop/bin/agent-loop start --repo /absolute/path/to/repository
```

`install`はCLIとCodex Skillをユーザー領域へ配置します。`bootstrap-labels`は既定ではpreviewのみ、`register`はリポジトリ別の状態領域とLaunchAgentを作成します。`doctor`は修復を行わず、開始前に互換性・設定・GitHub操作を診断します。詳細は[ラベルrunbook](docs/github-labels.md)と[doctor runbook](docs/doctor.md)を参照してください。

## 複数リポジトリでの並列運用

CLIの`install`は1回だけ行い、設定、ラベル、登録、診断、起動はリポジトリごとに実施します。

```sh
agent-loop bootstrap-labels --repo /path/to/repo-a
agent-loop bootstrap-labels --repo /path/to/repo-a --apply
agent-loop register --repo /path/to/repo-a
agent-loop doctor --repo /path/to/repo-a --json
agent-loop start --repo /path/to/repo-a

agent-loop bootstrap-labels --repo /path/to/repo-b
agent-loop bootstrap-labels --repo /path/to/repo-b --apply
agent-loop register --repo /path/to/repo-b
agent-loop doctor --repo /path/to/repo-b --json
agent-loop start --repo /path/to/repo-b
```

登録したリポジトリごとにLaunchAgent、永続状態、ログ、Issue worktree、supervisorが分かれるため、各ループは並列に動作します。ただし、各リポジトリ内のIssue処理は現在`queue.concurrency: 1`の直列実行です。

注意点:

- 同じGitHubリポジトリを複数のcloneや別ホストから同時に動かさない。ローカルのsupervisor lockはホストをまたぐ二重claimを防止しません。
- 手動で`agent-loop run`を多重起動せず、各リポジトリを`register`して`start`する。
- `status`、`watch`、`stop`などの操作では常に`--repo`で対象を明示する。引数なしの`doctor`はhostと登録済みリポジトリ全体を診断します。
- Codexの利用枠とMacのCPU、メモリ、ディスク、networkは共有されるため、リポジトリ数を段階的に増やす。

本番運用の構成とリポジトリごとの監視taskについては[Mac mini常駐運用runbook](docs/mac-mini-runbook.md)を参照してください。

## Operation

```sh
# 状態確認
agent-loop status --repo /path/to/repository --json

# attentionが必要になるまで1回のblocking watch
agent-loop watch --repo /path/to/repository --until-attention --json

# 質問へ回答
printf '%s\n' '選択した方針' | agent-loop answer \
  --repo /path/to/repository \
  --request-id req_... \
  --message-file - \
  --json

# 状態を残して停止
agent-loop stop --repo /path/to/repository
```

Codex側で定期的に`status`を呼ぶ必要はありません。`watch`内部のmacOS event通知とreconciliation pollingが永続状態を確認します。

## State and safety

- 通常のworking treeは変更せず、Issueごとのworktreeを使用します。
- force push、sandbox bypass、状態やworktreeの自動削除は行いません。`stop`、`unregister`、`uninstall`後も状態とworktreeを保持します。
- timeoutや未回答requestがあっても途中状態を保持し、安全に再試行・再開します。
- Issue入力を未信頼データとして扱い、既知token形式と`security.redact_env`の値をログや状態からマスクします。秘密をIssueや回答へ含めないでください。
- 権限、認証、backupの本番チェックは[セキュリティ運用runbook](docs/security-runbook.md)に従ってください。
