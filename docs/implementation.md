# MVP実装状況

## 実装済み

初期MVPでは次を実装している。

- Go単一バイナリの`agent-loop` CLI
- `install`、`uninstall`、`register`、`unregister`
- `start`、`stop`、`restart`、`status`
- `watch --until-attention`、`watch --until-idle`、`answer`
- `logs`、`doctor`、launchd用`run`
- strictな`.agent-loop.yaml`読み込みと既定値
- repository registryと登録時の外部command絶対パス固定
- repository別LaunchAgent生成
- 原子的な`state.json`とappend-only `events.jsonl`
- `state_revision`、stickyな未回答request、回答の冪等性
- fsnotifyによるmacOS event起床と低頻度reconciliation
- 回答保存時のsupervisor即時起床
- GitHub Issueのfilter、再取得、決定論的sort、write-ahead claim
- Issue別branchとGit worktree
- schema付き`codex exec`、session ID保存、`codex exec resume`
- `standard` / `extended` preflight policy
- retry、extended continuation、再起動時のactive Issue復旧
- 完了、入力待ち、失敗のGitHub反映と未反映状態の再試行
- credential形式をマスクしたworker log
- fake GitHub/Codex統合テスト、実Git worktreeテスト、race test

## 運用前に必要なもの

対象GitHub repositoryには、設定したready、running、needs-input、failed、done、blocked labelを事前に作成する。`doctor`はrepository accessと必須labelを検査するが、外部状態を自動作成しない。

`doctor`では次も検査する。

- `git`、`gh`、`codex`、`launchctl`
- GitHub認証
- `codex exec`と`codex exec resume`の必須option
- `.agent-loop.yaml`と登録状態
- macOS AC電源時のsleep設定

## MVPの制限

- 対象はmacOSのユーザーLaunchAgentのみである。
- 1 repositoryにつきconcurrencyは1である。
- 同じrepositoryを複数hostから処理しない。
- GitHub labelは自動作成しない。
- event logとsupervisor logのrotationは未実装である。
- 起動時reconciliationはactive run、write-ahead claim、未反映GitHub状態を復旧する。既に人手で変更されたbranch/PRを完全に探索して収束させる処理は今後の拡張である。
- Codex taskが接続されていない場合のスマートフォンへの直接push adapterは未実装である。
- 実GitHub repositoryと実Codex workerを使うend-to-end testは、利用者のtest repositoryで実施する必要がある。

## テスト

```sh
go test ./...
go test -race ./...
go vet ./...
```

テストでは、eventを配送しない状態からreconciliationだけでattentionを検出するケース、回答eventによるsupervisor起床、claim途中停止、GitHub同期失敗、Codex JSONL/session解析を検証する。
