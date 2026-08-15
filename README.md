# codex-issue-loop

GitHub Issue をキューとして、着手可能な Issue が存在する限り Codex CLI のワーカーを繰り返し実行する、macOS 向けの常駐ループです。

Go製の`agent-loop` CLI、launchd supervisor、GitHub/Codex adapter、永続状態、監視・回答フローを含みます。現在は初期MVPです。本番リポジトリへ登録する前に、テスト用リポジトリで権限・ラベル・worker promptを確認してください。

## Documents

- [アーキテクチャ概要](docs/architecture.md)
- [MVP実装状況](docs/implementation.md)
- [要件定義](docs/requirements.md)
- [システム仕様](docs/specification.md)

![codex-issue-loop アーキテクチャ](docs/images/architecture-overview.png)

## 設計の要点

- ループ本体は Codex の task や goal ではなく、独立した `agent-loop` CLI が担う
- macOS の `launchd` がループの生存を管理する
- Issue ごとに独立した `codex exec` ワーカーを起動する
- Codex Skill は起動・停止・監視・回答を CLI に橋渡しする薄い操作層とする
- スマートフォンでは、監視用 task と Issue 作成用 task の2つを主な入口にする
- ユーザーへの質問が必要になった場合は状態を永続化し、監視用 task を通して回答できるようにする
- `watch` は永続状態を正本とし、イベント通知と60秒間隔のreconciliationを併用する
- Codex Goalは外側のIssueループには使わず、単一目的の長時間作業に限定して活用する

## Requirements

- Apple Silicon macOS
- Go 1.22以降（ソースからビルドする場合）
- `git`
- `gh`と対象リポジトリへの認証
- `codex`と有効なCodex認証
- ログイン中のmacOSユーザーセッション（LaunchAgentを使用）

Mac miniでは、macOSの「ディスプレイがオフのときに自動でスリープさせない」を有効にすることを推奨します。

## Build and test

```sh
make test
make build
./bin/agent-loop --version
```

## Setup

1. 対象リポジトリに`.agent-loop.yaml`を追加します。[設定例](.agent-loop.example.yaml)を参照してください。
2. CLIとCodex Skillをユーザー領域へインストールします。
3. 対象リポジトリを登録し、診断後に開始します。

```sh
./bin/agent-loop install
~/Library/Application\ Support/codex-issue-loop/bin/agent-loop register --repo /absolute/path/to/repository
~/Library/Application\ Support/codex-issue-loop/bin/agent-loop doctor --repo /absolute/path/to/repository
~/Library/Application\ Support/codex-issue-loop/bin/agent-loop start --repo /absolute/path/to/repository
```

`install`は次を配置します。

- `~/Library/Application Support/codex-issue-loop/bin/agent-loop`
- `~/.codex/skills/agent-loop/SKILL.md`

`register`はリポジトリ別の永続状態ディレクトリと`~/Library/LaunchAgents/com.codex-issue-loop.<repo-id>.plist`を作成します。認証tokenはコピーしません。

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
- force push、sandbox bypass、状態やworktreeの自動削除は行いません。
- `stop`、`unregister`、`uninstall`後もIssueの状態とworktreeを保持します。
- 未回答requestは回答されるまでstickyに保持されます。
- GitHub Issue本文は信頼済みのshell入力として扱いません。
