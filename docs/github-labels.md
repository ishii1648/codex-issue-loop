# GitHubラベルbootstrap runbook

`agent-loop`は`.agent-loop.yaml`で指定されたready、running、needs-input、failed、done、`blocked`除外ラベル、およびqueueのpriority labelを使用する。`bootstrap-labels`は対象リポジトリの不足ラベルだけを安全に作成する。priorityの順位と運用は[Queue ordering](queue-ordering.md)を参照する。

## 標準手順

最初に変更計画をpreviewする。この操作はGitHubを変更しない。

```sh
agent-loop bootstrap-labels --repo /absolute/path/to/repository --json
```

出力の各`actions`を確認する。

- `create`: `--apply`で作成する不足ラベル
- `preserve`: 既存ラベルをそのまま保持する
- `metadata_differs: true`: 推奨する色・説明と異なるが、既存metadataを上書きしない
- `deletes_labels: false`: 実行時にもラベルを削除しない

計画どおりなら適用し、再度previewまたは`doctor`で確認する。

```sh
agent-loop bootstrap-labels --repo /absolute/path/to/repository --apply --json
agent-loop bootstrap-labels --repo /absolute/path/to/repository --json
agent-loop doctor --repo /absolute/path/to/repository --json
```

同じコマンドは冪等である。既存ラベルに`--force`を使わず、色・説明・大文字小文字が異なる同名ラベルも保持する。ラベルの更新・削除が必要な場合は、repository管理者が別の明示的なGitHub操作として行う。

## 権限と部分成功

適用前に`gh auth status`と対象リポジトリへのIssue metadata書き込み権限を確認する。権限不足やGitHubの一時障害が発生しても、作成に成功したラベルをrollbackまたは削除しない。全ラベルの作成を試み、`created`と`failures`を返して非0で終了する。権限を修正して同じコマンドを再実行すれば、不足分だけを再試行する。

```sh
gh auth status
agent-loop bootstrap-labels --repo /absolute/path/to/repository --apply --json
```

実GitHub確認では、空のtest repositoryに対してpreview、apply、2回目のapplyを順に実行する。1回目だけがラベルを作成し、2回目の`created`が空であること、既存ラベルのmetadataが変わらないことを確認する。本番リポジトリで初めて試さない。
