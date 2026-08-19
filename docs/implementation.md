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
- fsnotify/kqueueによるstate directory event起床、購読失敗時のpolling-only fallback、低頻度reconciliation
- 回答保存時のsupervisor即時起床
- GitHub Issueのfilter、再取得、決定論的sort、write-ahead claim
- Issue番号、作成日時、priority labelを使うconfigurableなqueue orderingと安定tie-break
- Issue別branchとGit worktree
- workerのworktree内実装と、supervisor publisherによるcommit・push・draft PR作成の分離
- dirty PRのimmutable base merge準備、workerによる意味的競合解消、scope検証付き通常push、再起動時の冪等再開
- draft PRのCI監視、成功後のReady化、manifestで選べるbase branch追随・squash merge、merge後のIssue完了
- schema付き`codex exec`、session ID保存、`codex exec resume`
- `extended` continuation限定のoptional App Server Goal adapter、token/time usage永続化、safe fallback
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
- Mac miniへの初回導入、Codex Remoteからの日常操作、障害復旧、backup・restore、手動update・rollback、撤去、実機受け入れを一連で扱う常駐運用runbook
- event checkpointを用いた復旧可能なrotation、gzip世代保持、worker run保持上限、容量reserveによるblocked化
- tag由来version、再現build、SPDX SBOM、checksum、artifact attestationを含むGitHub Release workflow
- binary/Skill manifest、稼働LaunchAgentを保ったupdate、自動rollback、明示backup rollback
- config・registry・state・active event・transactionのv1→v2 migration、checksum backup、journal再開、paired rollback
- config・registry・state・active event・transactionのv3→v4 migration、旧外部配送data除去、旧credential非破壊保持、paired rollback
- status別保持期間、dirty・未push・open PR・未回答request検査、dry-run cleanup、確認token付きpurge、削除監査event

## 運用前に必要なもの

対象GitHub repositoryには、設定したready、running、needs-input、failed、done、blocked、priority labelを事前に作成する。`doctor`はrepository accessと必須labelを検査するが、外部状態を自動作成しない。

`doctor`では次も検査する。

- `git`、`gh`、`codex`、`launchctl`
- GitHub認証
- `codex exec`と`codex exec resume`の必須option
- `.agent-loop.yaml`と登録状態
- macOS AC電源時のsleep設定
- `codex login status`、登録済みcommandの絶対path、LaunchAgent plist
- raw state/event/logの整合性とblocked/stopped状態。`schema_version: 1`と安定したdiagnostic codeで結果を返し、修復案は自動実行しない

## MVPの制限

- 対象はmacOSのユーザーLaunchAgentのみである。
- LaunchDaemonと自動ログインは採用せず、logout・再起動は運用時確認とする。[ADR-0001](adr/0001-macos-execution-model.md)を参照する。
- 1 repositoryにつき`queue.concurrency`までworkerを並列実行する。resource definitionがない既存設定は`repo:*` leaseにより安全に直列化される。
- schema v3のresource leaseはIssueごとにslot、resource、run ownerを保持し、scheduler再起動時は全active/open-PR Issueを照合して排他を復元する。typed worker environment blockはcontinuationと元lease provenanceを`resource_park`へ保持したままactive admissionから外し、限定resume時だけ新generationで再取得する。`area:` resource claimとIssue本文の`depends_on` metadataは[Resource admission契約](resource-admission.md)に従う。
- 同じrepositoryを複数hostから処理しない。
- local `flock`はhostをまたぐ排他ではない。複数hostを登録するだけでは安全にならず、[ADR-0002](adr/0002-concurrency-and-multi-host.md)のcoordinatorとpublication gatewayが実装されるまで禁止する。
- GitHub labelは自動作成しない。
- 起動時reconciliationは全active/open-PR Issue、write-ahead claim、残存worker process group、未反映GitHub状態、未記録のpush/PR、merge/close済みPRをIssue単位で復旧する。所有確認済みのorphan groupは全group共通grace periodで終了し、PID再利用、branch・worktree・labelの人手変更は推測で上書きせずblockedへ移す。
- 通常schedulerは保存済み未merge PRを持つterminal Issueを低頻度で1件ずつ再照合する。保存URL・head branch・単一PRが一致し、自動blocked markerを持つIssueのmergeだけをauthoritativeとしてcompletedへ収束させ、他workerを停止せずlease解放とdone同期を行う。手動exclusion、未merge、mergeなしclose、複数PR、不一致はstickyのまま保持する。
- PR conflictの検出自体はblocked理由にしない。`resolving_conflict`にbase SHA・競合file・試行履歴を保存し、budget超過またはworktree破損、scope違反等の非回復障害だけを最終blockedとして同期する。
- 実GitHub repositoryと実Codex workerを使うMac mini E2Eの結果は[`docs/e2e/2026-08-15-mac-mini.md`](e2e/2026-08-15-mac-mini.md)に記録している。スマートフォンからのCodex Remote接続は確認済みである。display off、logout、OS再起動はM3の受け入れTODOとせず、発生時または計画保守時の運用確認としてrunbookで扱う。
- Codex CLI 0.136.0以降とGitHub CLI 2.69.0以降を対応下限とし、起動時に必須capabilityを検査する。resume非対応時は既存worktreeと永続状態を使う新規sessionへfallbackする。詳細は`docs/compatibility.md`を正本とする。
- worker timeout、stop、restart時はIssue別の独立process groupへSIGTERMを送り、既定30秒のgrace period後も親子processが残る場合だけSIGKILLへ進む。複数workerのstopは全groupを先にcancelし、終了段階をIssue別eventへ残す。worktreeと途中成果は削除しない。
- `bootstrap-labels`は必須GitHubラベルのpreviewと冪等な不足分作成を提供する。既存metadataは保持し、部分成功後の再実行を安全にする。

## テスト

```sh
go test ./...
go test -race ./...
go vet ./...
```

テストでは、eventを配送しない状態からreconciliationだけでattentionを検出するケース、read-subscribe-read race、複数watchと切断、worker/supervisor停止、claim・worktree作成・GitHub同期の途中停止、Codex JSONL/session解析、deterministicなclock/random sourceを用いたbackoffと失敗counterのresetに加え、fake GitHubでのPR・label競合と実Git repositoryでのworktree・branch検査を検証する。詳細は `docs/testing.md` の対応表を正本とする。
