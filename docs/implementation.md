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
- write-ahead state transaction、partial event修復、破損時の隔離とrecovery blocked
- `state_revision`、stickyな未回答request、回答の冪等性
- fsnotifyによるmacOS event起床と低頻度reconciliation
- 回答保存時のsupervisor即時起床
- GitHub Issueのfilter、再取得、決定論的sort、write-ahead claim
- Issue別branchとGit worktree
- schema付き`codex exec`、session ID保存、`codex exec resume`
- `standard` / `extended` preflight policy
- typed failure分類、±20% jitter付き上限5分のretry/polling backoff、extended continuation
- 完了、入力待ち、失敗のGitHub反映と未反映状態の再試行
- Issue/commentのprompt injection境界、入力上限、制御文字除去
- worktree root逸脱・symbolic link・unsafe refの拒否
- 既知credential形式と設定secretをworker log/result、state/event、GitHub通知でmask
- state、log、plistのprivate permission強制
- 脅威モデル、最小権限runbook、CIの`govulncheck`
- fake GitHub/Codex統合テスト、実Git worktreeテスト、race test
- `TestFault` prefixで独立実行できる障害注入・復旧suiteと仕様17.2の対応表

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
- 起動時reconciliationはactive run、write-ahead claim、未反映GitHub状態、未記録のpush/PR、merge/close済みPRを復旧する。branch・worktree・labelの人手変更と二重workerの可能性は自動上書きせず、理由を残してblockedへ移す。
- Codex taskが接続されていない場合のスマートフォンへの直接push adapterは未実装である。
- 実GitHub repositoryと実Codex workerを使うend-to-end testは、利用者のtest repositoryで実施する必要がある。
- Codex CLI 0.136.0以降とGitHub CLI 2.69.0以降を対応下限とし、起動時に必須capabilityを検査する。resume非対応時は既存worktreeと永続状態を使う新規sessionへfallbackする。詳細は`docs/compatibility.md`を正本とする。

## テスト

```sh
go test ./...
go test -race ./...
go vet ./...
```

テストでは、eventを配送しない状態からreconciliationだけでattentionを検出するケース、read-subscribe-read race、複数watchと切断、worker/supervisor停止、claim・worktree作成・GitHub同期の途中停止、Codex JSONL/session解析、deterministicなclock/random sourceを用いたbackoffと失敗counterのresetに加え、fake GitHubでのPR・label競合と実Git repositoryでのworktree・branch検査を検証する。詳細は `docs/testing.md` の対応表を正本とする。
