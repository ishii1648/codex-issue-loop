# codex-issue-loop 用語集

この文書は、docs・コード・Issueで使う中核用語の表記と意味を一覧化する。各用語の詳細な振る舞いと不変条件は「正本」に示す既存文書に従い、この用語集では新しい意味を追加しない。

## 中核用語

### claim

supervisorが着手可能なIssueを選び、そのIssueを処理する所有権をrunとローカル状態に確定すること。resource leaseの予約と`claiming`への遷移を同じ永続transactionで行い、その成功後にGitHub反映やworker起動へ進む。正本: [Resource admission契約](resource-admission.md)、[仕様書](specification.md)

### lease

Issueがclaimしたresourceとworker slotを特定の`(Issue number, run_id, generation)` ownerに結び付ける、永続的な単一host用の論理所有権。wall-clock TTLでは失効せず、再起動時は新規admissionより前にreconcileする。正本: [Resource admission契約](resource-admission.md)

### attention

監視taskがユーザーまたはoperatorへ提示すべき、`needs_input`、`blocked`、`stopped`などの状態の総称。attention状態はstickyであり、回答、明示的な復旧操作、または定義済みの状態遷移まで永続snapshotに残る。正本: [仕様書](specification.md)、[ADR-0003](adr/0003-event-notification.md)

### admission

固定したIssue・GitHub・設定・ローカル状態のsnapshotから、依存関係、resource競合、queue順、空きslotを決定論的に評価し、Issueを開始できるか判断する処理。判断からlease取得までをGoコードで行い、LLMには委ねない。正本: [Resource admission契約](resource-admission.md)

### resource claim

Issueが排他的に使用するresourceの集合で、GitHubの`area:<name>` labelから正規化される。入力が欠落または不正な場合は部分解釈せず、全resourceと競合する`repo:*`へ安全側に縮退する。正本: [Resource admission契約](resource-admission.md)

### park

通常workerが`needs_input`になったとき、元lease、run、request、ownerのprovenanceとcontinuationを保持したまま、claimをactive admissionから外すこと。回答後もresourceまたはslotが競合していればparkを維持し、安全に再取得できるまで待つ。正本: [Resource admission契約](resource-admission.md)

### fence（maintenance fence）

Release適用中に全repository schedulerの新規claim、retry/resume、conflict worker、PR maintenanceのdispatchを止める、host単位の永続ガード。実行中workerは強制終了せずdrainし、適用後のhealth確認またはrollbackが安全に完了するまで解除しない。正本: [アーキテクチャ概要](architecture.md)、[Release仕様](release.md)

### publication

worker完了後の差分とresource範囲を監査し、commit、push、draft Pull Requestの作成または再利用へ進める決定論的な公開処理。workerから分離され、repository単位で直列化される。正本: [仕様書](specification.md)、[ADR-0002](adr/0002-concurrency-and-multi-host.md)

### publisher

publicationを実行するsupervisor内の決定論的コンポーネント。GitHubへ公開できる論理writerはrepository単位で1つに限定され、workerはcommit、push、Pull Request作成を行わない。正本: [アーキテクチャ概要](architecture.md)、[仕様書](specification.md)

### provenance

workspace、session、run、lease owner generation、requestなどの由来と対応関係を、後続処理が再検証できるよう永続化した証拠。欠落や不一致があるexecution-required provenanceをevent履歴や推測で合成せず、該当処理をfail closedにする。正本: [仕様書](specification.md)、[互換性](compatibility.md)

### reconciliation

永続snapshotを正本として、GitHub、worker process、worktreeなどの観測結果と照合し、中断や通知欠落後も状態を定義済みの遷移へ収束させる処理。event通知は低遅延化のhintに留め、定期reconciliationを最終的な検出経路とする。正本: [アーキテクチャ概要](architecture.md)、[ADR-0003](adr/0003-event-notification.md)

### snapshot

supervisor、Issue、未回答request、leaseなどの現在状態を保持する永続的な状態像で、`state.json`に原子的に保存される。監視とreconciliationは通知payloadではなくsnapshotを読み直して判断する。正本: [仕様書](specification.md)、[ADR-0003](adr/0003-event-notification.md)

### event log

各状態更新に対応するeventを、連続する`sequence`付きの独立したJSON行として`events.jsonl`へ追記する永続記録。snapshotとの整合性はtransactionとrevisionで検証するが、event logだけから実行authorityや欠落したprovenanceを合成しない。正本: [仕様書](specification.md)

### state_revision

有効な状態更新ごとに単調増加するsnapshotの世代番号。最後のevent `sequence`と一致させ、watchの再接続やadmission中の競合更新を検出するために使う。正本: [仕様書](specification.md)

### needs_input

workerまたはlifecycleが、外部仕様、破壊的・公開操作、credentialや権限、リポジトリだけでは決められない事実などについてユーザー判断を必要とするIssue状態。errorではなく永続的な状態遷移として扱い、requestとattentionを回答まで保持する。正本: [仕様書](specification.md)、[アーキテクチャ概要](architecture.md)

### continuation

未完了のIssueを、保存済みsession、worktree、run、検証結果などを使って後続workerで継続すること。spawn直前にworkspace provenanceとlease ownerを再検証し、`extended`の自動継続とユーザー回答後の再開は別のbudgetとして扱う。正本: [仕様書](specification.md)

### preflight

初回worker prompt内で、受け入れ条件、変更範囲、依存関係、検証、リスク、反復見込みを整理する論理フェーズ。別processやユーザー確認で停止せず、profileを選択した後そのまま実装へ進む。正本: [仕様書](specification.md)、[アーキテクチャ概要](architecture.md)

### profile（standard / extended）

preflightで選ぶworkerの実行戦略で、範囲と完了条件が明確で単一run内の完了が見込める場合は`standard`、広範な調査・移行・長時間または段階的検証が必要な場合は`extended`とする。分類が曖昧な場合はユーザーへ質問せず`extended`を選び、必要なcontinuation budgetを確保する。正本: [仕様書](specification.md)

### worker backend

Issue workerを実行するruntimeの種別で、`worker.backend`には`codex`、`claude-code`、`opencode`のいずれかを指定し、省略時は`codex`を使う。任意のshell command templateは許可せず、session IDはbackend名と組にして保存する。正本: [仕様書](specification.md)、[互換性](compatibility.md)

### delivery controller

production Releaseの取得、checksum・provenance・version検証、host-wide drain、適用、health check、rollbackを担う、Macごとの短命LaunchAgent。repository supervisorとは別の設定・state・trust boundaryを持つ。正本: [アーキテクチャ概要](architecture.md)、[Release仕様](release.md)

### drain

maintenance fenceで新規dispatchを止めた後、実行中workerをkillせず、全workerのPID/PGID消失、snapshot flush、各supervisorの`maintenance`到達を待つこと。timeout時は適用を開始せず、fenceを解除して後のreconcileへ延期する。正本: [Release仕様](release.md)、[アーキテクチャ概要](architecture.md)

### recovery

process停止、状態更新の中断、外部状態との差異、または永続データ破損から、transaction replay、reconciliation、もしくは対象を限定したoperator commandで整合状態へ戻すこと。証拠が不足する状態を推測で修復せず、必要ならbackupを隔離して`recovery_blocked`としてfail closedにする。正本: [仕様書](specification.md)、[破損時の修復手順](break-glass-repair.md)

### blessed fixture

productionからread-onlyで取得・sanitizeしたrecovery fixtureのうち、完全性検証とmaintainer reviewを通過し、file SHA-256が`blessed-fixtures.sha256`に登録されたもの。fixtureを手作業で短縮・補完せず、保存されたrecordと参照関係をrecovery testへそのまま渡す。正本: [Production recovery fixture runbook](recovery-fixtures.md)
