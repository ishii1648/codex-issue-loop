# codex-issue-loop 用語集

詳細な不変条件は[仕様書](specification.md)と[アーキテクチャ概要](architecture.md)を正本とする。

### intake

openかつreadyなIssueを取得し、Issue作成者をGitHub APIで検証して、実装候補として受理またはskipする処理。Issue本文は権限を拡張しない。

### active execution

同一repositoryで現在実行権を持つ唯一のIssue identity。snapshot rootの`active_execution`にIssue番号、run ID、generationを保存する。active lifecycleだけが保持でき、待機・terminal・quarantineへ遷移すると同じtransactionで解放する。

### generation

Issueの実行世代。新規開始または継続開始のたびに単調増加し、古いworker callbackや古いcontinuation操作を拒否するfenceとして使う。

### lifecycle API

Issue status、許可遷移、実行枠の取得・解放、continuation境界を外部契約としてversion化したもの。互換でない変更はmajor versionを更新し、未知majorをfail closedで拒否する。

### continuation

中断したIssueを同じworktree、branch、session、stageから再開するためのIssue-local checkpoint。scenario別の復旧statusやsubstateを増やさず、再開に必要なprovenanceだけを保持する。

### continuation evidence

continuationの由来と整合性を後続処理が検証するための監査証跡。欠損情報をevent履歴やerror文言から推測して合成しない。

### suspension

Issueを実行枠から外した理由、recoverability、許可されたoperator actionを表すIssue-local state。ambiguousなIssueはquarantineして後続queueを継続する。

### attention

ユーザーまたはoperatorへ提示すべき`needs_input`、`blocked`、`stopped`などの永続状態。回答または定義済みresolutionまでstickyに保持する。

### publication

worker完了後の差分を検証し、commit、push、Pull Request作成または再利用を行う決定論的処理。外部副作用のintent/resultはroot `pending_effects`へ保存し、再実行を冪等にする。

### reconciliation

canonical snapshotをGitHub、worker process、worktreeと照合し、通知欠落や再起動後も定義済み遷移へ収束させる処理。eventはhintと監査であり実行authorityではない。

### snapshot

supervisor、root `active_execution`、Issue、request、pending effectの現在状態を`state.json`へ原子的に保存した正本。`state_revision`は有効な更新ごとに単調増加する。

### event log

状態更新を連続sequence付きJSON行で保存する監査記録。snapshotとの整合性を検証するが、eventだけから欠落したauthorityを復元しない。

### needs_input

repository内の情報だけでは決められない判断をユーザーへ求めるIssue状態。active executionを保持せず、request、continuation、attentionを回答まで保存する。

### maintenance fence

Release適用中に全repositoryの新規dispatchを止めるhost単位の永続ガード。実行中workerをdrainし、適用・health check・rollbackが終わるまで解除しない。

### delivery controller

stable Releaseの取得、checksum・attestation・version検証、repository別assignment、適用、health check、rollbackを担う管理component。

### recovery

transaction replay、generic reconciliation、または`issue plan` / `issue resolve`で整合状態へ戻すこと。scenario別runtime経路は追加せず、証拠不足のIssueだけをfail closedに隔離する。

### blessed fixture

productionからread-onlyで取得・sanitizeし、完全性検証とreviewを通過したmigration fixture。v4入力をv5へ変換するdecoder検証にだけ使う。
