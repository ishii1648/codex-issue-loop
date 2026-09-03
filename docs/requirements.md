# codex-issue-loop 要件定義

## 1. 文書の目的

本書は、GitHub Issue を継続的に取得し、Codex CLI で実装する汎用的な Issue ループの要件を定義する。

対象は、常時稼働させる Mac mini を実行ホストとし、ユーザーが ChatGPT モバイルアプリの Codex Remote から操作・監視できる構成である。仕事の投入元はMac mini上のCodexに限定せず、GitHub Issueを共通のキュー境界とする。Claude Code および Claude Remote Control には依存しない。

## 2. 背景

Codex の単一 task や goal は、一つの具体的な目的を継続的に追う用途には適するが、以下を含む永続的な Issue キュー処理そのものではない。

1. GitHub から着手可能な Issue を選ぶ
2. Issue ごとに実行環境を分離する
3. Codex ワーカーに実装させる
4. 結果を GitHub に反映する
5. 次の Issue を選び、キューが空になるまで繰り返す
6. 新しい Issue が追加されたら処理を再開する

したがって、ループの実行主体を Codex task から分離し、決定論的な常駐プログラムとして実装する。

## 3. ゴール

### G-1: スマートフォンからの運用

ユーザーは Mac mini の前に戻ることなく、次の操作を行えること。

- ループを起動・停止・再起動する
- 現在の Issue、状態、直近の結果を確認する
- ループが要求した質問に回答する
- 新しい GitHub Issue を会話から作成する

### G-2: Codex task から独立した継続実行

監視用 Codex task が終了、切断、アーカイブされた場合も、`launchd` 配下のループは継続すること。新しい監視用 task から同じループへ再接続できること。

### G-3: Issue がある限り処理する

設定された条件に一致する着手可能な Issue を決定論的に選び、完了・入力待ち・失敗のいずれかになるまで実行し、その後に次の Issue へ進むこと。

### G-4: 質問による停止を可視化する

自律実行中の質問は最小限に抑える一方、破壊的操作、曖昧なプロダクト判断、権限追加など、ユーザー判断が不可欠な場面では質問を許容する。質問と途中状態を失わず、スマートフォンから検知・回答できること。

### G-5: 複数リポジトリで再利用できる

ループ実装を対象リポジトリから分離し、設定ファイルとインストール手順によって任意の GitHub リポジトリへ導入できること。

### G-6: 停止させない実行経路選択

各Issueのpreflightで通常実行と長時間実行を自動選択し、分類が難しいこと自体を理由にユーザー確認を要求しないこと。安全性を保ったまま判断できない場合は、より強い長時間実行profileへ倒すこと。

### G-7: 低コストで取りこぼしのない監視

監視は永続状態を正本とし、event通知による低遅延な起床と低頻度reconciliation pollingを併用すること。待機中の確認をGoプロセス内で完結させ、Codexに定期的なstatus確認をさせないこと。

## 4. 非ゴール

初期リリースでは次を対象外とする。

- Claude Code、Claude Remote Control との連携
- Codex Desktop 内への専用ダッシュボード追加
- GitHub 以外の Issue 管理サービス
- 複数ホストによる同一リポジトリの分散処理
- 複数 Issue の並列実行
- ユーザー判断をモデルが代行する仕組み
- Mac の自動ログインやスリープ設定を無断で変更すること
- Codex の認証情報や GitHub トークンの独自保管

## 5. 利用者と主要ユースケース

### 5.1 利用者

- Mac mini を常時稼働ホストとして管理する開発者
- ChatGPT モバイルアプリから Codex Remote を利用する同一ユーザー
- GitHub UI、CLI/API、automation、別ホストのCodex等からIssueを作成するproducer

### 5.2 主な操作入口と投入経路

同じローカルプロジェクト配下には、監視taskを作成してピン留めする。Issue作成用taskは会話で要望を整理したい場合の任意の入口であり、常設の必須コンポーネントではない。

| 入口 | 役割 | 通常時の見え方 |
| --- | --- | --- |
| `[LOOP] <repo> — monitor` | ループの起動、再接続、状態監視、質問への回答 | `Running`、入力が必要なら `Needs input` |
| `[INTAKE] <repo> — new issue`（任意） | 要望の整理、コード調査、Issue 本文作成、GitHub Issue 登録 | 会話待ち |
| GitHub UI、CLI/API、automation等 | GitHub Issue作成と着手可能ラベル付与 | Codex taskを必要としない |

Issue ごとの `codex exec` ワーカーを Codex アプリ上の個別 task として作成することは要件としない。Issue 単位の進捗は監視用 task、GitHub Issue、Pull Request、ローカルログで確認する。

複雑な Issue の検討では一時的なdraft taskを作成してよい。どのproducerを使っても、Issueの作成主体・作成場所・作成手段は着手可能性の判定条件にしない。

### 5.3 主要フロー

#### UC-1: ループを開始する

1. ユーザーがスマートフォンで監視用 task を開く
2. ユーザーが `agent-loop` の開始を指示する
3. Codex が Skill の手順に従って `agent-loop start` を実行する
4. CLI が対象の LaunchAgent を起動する
5. Codex が `agent-loop watch --until-attention` で監視を開始する

#### UC-2: 新しい仕事を投入する

1. 任意のproducerがGitHub Issueを作成する
2. producerがIssue本文へ要望と完了条件を記載し、着手可能ラベルを付ける
3. CodexのIssue作成用taskを使う場合は、必要に応じて対象リポジトリと既存Issueを事前に調査する
4. ループが Issue を取得して実装を開始する

#### UC-3: ユーザーの回答を待つ

1. ワーカーが自律的に決められない事項を検出する
2. ワーカーが構造化された `needs_input` 結果を返す
3. ループが質問、選択肢、作業状態を永続化する
4. 監視コマンドが終了し、監視用 Codex task がrequest ID、推奨案、選択肢を保持してユーザーへ質問する
5. Codex Desktopの質問通知とActivityの回答待ちにより、ユーザーが検知する
6. ユーザー回答を `agent-loop answer` で保存する
7. 新しい Codex ワーカーが、既存 worktree と回答を引き継いで処理を再開する

#### UC-4: 監視 task を再接続する

1. ユーザーが既存または新規の監視用 task を開く
2. Codex が `agent-loop status` で対象を特定する
3. 未回答の質問があれば直ちに表示する
4. なければ `agent-loop watch --until-attention` に接続する

## 6. 機能要件

### 6.1 セットアップと登録

- **FR-001**: 単一の Go バイナリとして配布できること。
- **FR-002**: ユーザー単位でバイナリ、Codex Skill、LaunchAgent をセットアップできること。
- **FR-003**: 対象リポジトリを登録・解除できること。
- **FR-004**: 対象リポジトリの `.agent-loop.yaml` から設定を読み込めること。
- **FR-005**: セットアップ処理は冪等であり、再実行しても重複登録や状態破壊を起こさないこと。
- **FR-006**: 必要な外部コマンド、認証、設定、macOS の電源条件を `doctor` で検査できること。
- **FR-007**: 必須GitHubラベルの変更計画を事前表示し、不足分だけを冪等に作成できること。既存ラベルのmetadataを上書きせず、ラベルを削除しないこと。

### 6.2 launchdによるプロセス管理

- **FR-010**: Codex task から実行される短命な CLI が `launchctl` を介して LaunchAgent を開始できること。
- **FR-011**: 常駐ループの親プロセスは Codex task ではなく `launchd` であること。
- **FR-012**: 異常終了時は、過剰な再起動を避ける間隔を設けて自動再起動できること。
- **FR-013**: start、stop、restart、status が冪等であること。
- **FR-014**: 登録リポジトリごとにプロセスと状態を分離できること。

### 6.3 Issue選択

- **FR-020**: GitHub から open Issue を取得し、設定された ready ラベル、除外ラベル、assignee、milestone等で絞り込めること。
- **FR-021**: Issue番号昇順、作成日時昇順、priority label・作成日時順を明示設定でき、同じ入力に対して同じIssueを選ぶ決定論的な順序を持つこと。
- **FR-022**: 選択した Issue を GitHub ラベルとローカル状態の両方で claim し、重複実行を防ぐこと。
- **FR-023**: キューが空の間は低負荷で待機し、定期的に再取得すること。
- **FR-024**: Issue が入力待ちまたは恒久的失敗になっても、設定に応じて他の着手可能な Issue へ進めること。
- **FR-025**: Issueの作成主体、作成場所、作成手段を着手可能性の条件にせず、GitHub上の状態、ラベル、設定されたassignee・milestone、ローカル処理状態だけで選択すること。
- **FR-026**: queue orderingは全pageの候補取得後に適用し、作成日時とIssue番号で安定したtie-breakを行うこと。
- **FR-027**: priority labelの順位は設定配列で定義し、labelなしを最低順位、複数該当を最上位一致として扱うこと。不正設定は起動前に拒否すること。
- **FR-028**: 同一repository内並列化では、configに定義したresourceとGitHubの`area:` label、Issue本文のversion付き`depends_on` metadataだけからeffective claimと依存関係を決定すること。自然言語やLLMによる補完を行わないこと。
- **FR-029**: resourceまたはmetadataが未指定・未知・不正なIssueは`repo:*`相当へ縮退し、同じrepositoryの他Issueと並列実行しないこと。

### 6.4 Codexワーカー

- **FR-030**: Issue ごとに独立した Git worktree と `codex/issue-<number>-<slug>` ブランチを作成すること。
- **FR-031**: 各試行を非対話の `codex exec` として起動すること。
- **FR-032**: ワーカーの最終結果を JSON Schema で検証可能な構造化データとして受け取ること。
- **FR-033**: ワーカーの終了状態として最低限 `completed`、`needs_input`、`retryable_failure`、`blocked` を扱うこと。
- **FR-034**: ワーカーは対象リポジトリの `AGENTS.md`、Issue 本文、既存コメント、ループ設定を入力として利用できること。
- **FR-035**: ユーザー回答後は、保存済み worktree、Issue コンテキスト、過去の質問と回答を与えた新しいワーカーで再開できること。
- **FR-036**: 承認とsandboxを無効化する危険な Codex オプションを既定で使用しないこと。
- **FR-037**: 各Issueの初回workerは、実装前にpreflightを行い、`standard` または `extended` execution profileを構造化結果として記録すること。
- **FR-038**: preflightは初回worker内の論理フェーズとして実行し、そのまま実装へ進めること。profile判定だけを目的とする別workerを必須にしないこと。
- **FR-039**: profile判定が曖昧な場合は、ユーザーへ質問せず `extended` を選択すること。`extended` は必要に応じてsupervisor管理のcontinuationを許可すること。
- **FR-039-A**: worker timeout時はprocess groupへ穏当な終了要求を送り、設定可能なgrace periodを超えた場合だけ強制終了すること。子processを残さず、既存worktreeと有効な作業を保持すること。
- **FR-039-B**: command networkは既定無効とし、Codex localhost-only opt-inではcommand/child processのnetworkを必須proxy経由のexact `localhost` / `127.0.0.1`へ限定し、capability・設定・proxy初期化失敗時はworker開始前にfail closedすること。
- **FR-039-C**: command proxyが保護しないWeb Search、Browser/Computer Use、MCP、apps/plugins等をlocalhost-only workerで無効化し、proxyの保証範囲を越えて保護済みと扱わないこと。

### 6.5 GitHubへの反映

- **FR-040**: claim、入力待ち、完了、失敗を GitHub のラベルまたはコメントへ反映できること。
- **FR-041**: 実装完了時にコミット、push、draft Pull Request の作成を既定の公開経路とすること。
- **FR-042**: Issue、ブランチ、Pull Request の対応関係を永続化すること。
- **FR-043**: 同じ試行を再実行しても、Pull Request やコメントを不必要に重複作成しないこと。
- **FR-044**: draft Pull RequestのCI結果をモデル呼び出しなしで監視し、すべて成功した場合だけReady for reviewへ移すこと。CI失敗時は同じworktreeと失敗理由をworkerへ渡して再試行すること。
- **FR-045**: 対象リポジトリのmanifestでauto mergeを選択でき、既定は無効とすること。有効時はbase branchへの追随とCI再確認を行い、conflict時は既存worktree・branch・Pull Requestを維持した永続的な自動復旧を開始すること。
- **FR-045-A**: conflict recoveryはimmutableなbase SHA、競合file、試行履歴を永続化し、workerへIssue・元PR差分・base追加commit・競合内容・検証要件を渡すこと。workerはGit公開操作を行わず、supervisorが未解消entry、marker、base SHA、path scope、検証結果を確認して通常pushすること。
- **FR-045-B**: terminal `blocked` / `failed`はhard leaseを保持せず、`issue plan`と`issue resolve --action retry-stage`がcanonical snapshotと現在のprocess/git/GitHubを再検証してdurable stateとGitHubを監査付きで同期できること。
- **FR-045-C**: 継続可能なworkerまたはlifecycle stageはgeneric checkpointへ同一成果物とauthorityを保存し、operator選択後に`issue resolve --action resume|retry-stage|adopt-pr|cancel`で解決できること。ambiguous、manual/security、active worker、inconsistent worktree/PRは副作用なく拒否すること。
- **FR-046**: Issueを完了扱いにし、設定に応じてcloseするのは対応Pull Requestのmergeを確認した後とすること。

### 6.6 監視と質問

- **FR-050**: 現在の supervisor、Issue、試行回数、状態、更新時刻、未回答質問を CLI で表示できること。
- **FR-051**: 機械可読な JSON 出力を提供すること。
- **FR-052**: `watch --until-attention` は、ユーザー入力、恒久的障害、明示停止、設定された完了条件のいずれかまで待機できること。
- **FR-053**: 監視クライアントが0件でも supervisor は処理を継続すること。
- **FR-054**: 複数の監視クライアントが接続しても処理状態を変更しないこと。
- **FR-055**: 質問には安定した request ID、Issue番号、質問文、理由、選択肢、自由記述可否、作成時刻を含めること。
- **FR-056**: 回答は request ID に対して一度だけ受理し、重複送信を安全に扱うこと。
- **FR-057**: 未回答質問は監視 task が切断しても失われないこと。
- **FR-058**: `watch` は永続snapshotを正本とし、event通知を状態変化のヒントとして扱うこと。event payloadだけでattention状態を確定しないこと。
- **FR-059**: `watch` はeventを取りこぼしても検出できるよう、既定60秒間隔の内部reconciliationを行うこと。reconciliation中はCodexへheartbeatや途中結果を返さないこと。
- **FR-059-D**: macOSのevent wakeはfsnotify/kqueueでstate directoryを監視し、watcher作成・登録失敗またはchannel終了時はpolling-onlyへ降格すること。
- **FR-059-E**: 接続中のCodex Desktop監視taskは`needs_input`をユーザー回答待ちの質問として表示し、OSの質問通知を閉じてもActivityから再発見できること。
- **FR-059-F**: Codex Desktop監視taskが切断中の新規Activity投入は保証せず、再接続時に永続snapshotの未回答requestを即時再表示すること。

### 6.7 Codex Skill

- **FR-060**: Skill は自然言語の依頼を安全な `agent-loop` CLI コマンドへ対応づけること。
- **FR-061**: Skill 自身が Issue 取得ループや長期状態を保持しないこと。
- **FR-062**: Skill は開始後に監視へ接続し、入力待ちになったらユーザーへ質問し、回答後に監視へ戻ること。
- **FR-063**: stop、reset、claim解除など影響の大きい操作では、対象と影響を明示すること。
- **FR-064**: 既存の未回答質問がある場合は、新規 watch より先にその質問を表示すること。
- **FR-065**: Desktopではrepositoryごとに専用監視taskを命名・pinし、対象path、request ID、回答先をrepository間で混在させないこと。
- **FR-066**: 質問表示はrequest ID、Issue番号、質問、理由、推奨案、全選択肢、自由記述可否を欠落させないこと。

### 6.8 Worktreeライフサイクル

- **FR-070**: completed、failed、blocked、needs-inputごとにworktree保持期間を設定でき、既定値を文書化すること。
- **FR-071**: cleanupは既定でread-only planを返し、dirty、未push commit、open PR、未回答requestを自動削除しないこと。
- **FR-072**: cleanup/purge適用時はloop停止を要求し、`git worktree prune`と整合させ、削除前後を監査eventへ記録すること。
- **FR-073**: purgeは通常cleanupと分離し、Issue単位の完全一致確認tokenと復元可能性の表示を必須にすること。

### 6.9 将来の並列化と複数host冗長化

- **FR-080**: 単一hostのworker並列化と、複数hostの冗長化を独立したmode・migrationとして扱うこと。
- **FR-081**: 単一host並列化では1つのsupervisorがIssue claim、state更新、GitHub公開、rate limitを直列化し、worker slotだけを並列化すること。
- **FR-082**: 複数host modeはGitHub外の線形化可能なcoordinator、単調増加epoch、期限付きlease、条件付き更新を必要とし、coordinator喪失時はfail closedすること。
- **FR-083**: 複数hostのworkerはGitHubへ直接公開せず、durable publication intentを介してfenced publication gatewayだけがbranch、comment、Pull Requestを更新すること。
- **FR-084**: status/watchは複数hostのownership、Issue状態、attentionをcoordinatorから集約し、event取りこぼしをreconciliationで修復すること。
- **FR-085**: distributed modeの有効化前にbackend conformance、credential、backup、partition、publication takeoverをdoctorまたは運用検証で確認すること。
- **FR-086**: 単一hostのresource claimは`claiming`からPR merge確認まで永続化すること。retry、CI待ち、open PRはactive leaseを保持するが、発生箇所を問わず`needs_input`はcontinuation provenanceを保ったcheckpointへleaseを退避し、後続Issueのadmissionから外すこと。
- **FR-087**: admissionは固定snapshot、正規化済み集合、queueの全順序、Issue番号tie-breakから決定し、同じsnapshotとscheduler versionに対して同じ選択結果と待機理由を返すこと。
- **FR-088**: resource/依存metadataの導入はconfig/state schema v3への停止・backup・preview・明示applyを伴うmigrationとし、v2 Issueを自動書換えまたは暗黙に並列化しないこと。
- **FR-089**: Webhook modeのrepository schedulerはfsnotifyをhintとして扱い、60秒のlocal reconciliationでcanonical snapshotとmailboxを再評価すること。このtimerはGitHub ready collectionを直接取得しないこと。
- **FR-090**: statusとdoctorはsafety sweepのready collection、mailbox、canonical snapshotを照合し、ready Issueが2 local reconciliation intervalを超えて未claimの場合とmailboxが同一targetの重複で非有界化した場合を失敗として表示すること。

## 7. 非機能要件

### 7.1 信頼性

- **NFR-001**: supervisor、Codex task、ネットワークのいずれが終了しても、確定済み状態を復元できること。
- **NFR-002**: 状態更新は原子的に行い、途中書き込みを有効状態として読み込まないこと。
- **NFR-003**: Codex、GitHub、ネットワークの一時障害には、上限付き exponential backoff で再試行すること。
- **NFR-004**: 再起動後に GitHub とローカル状態を照合し、安全に処理を再開すること。
- **NFR-005**: attention状態はユーザーの回答または明示的な取消までstickyに保持し、一過性eventの欠落で解除されないこと。
- **NFR-006**: 永続状態に単調増加するrevisionを持たせ、監視の再接続とrace検出に利用できること。
- **NFR-007**: network partitionではavailabilityよりsafetyを優先し、古いepochのhostによるclaim、state更新、GitHub公開を拒否すること。
- **NFR-008**: GitHub API応答を失った場合も、同じpublication intent、branch、冪等markerを照合して再開し、別PRを作って回避しないこと。

### 7.2 セキュリティ

- **NFR-010**: Codex と GitHub の既存認証機構を利用し、トークンを設定ファイルやログに保存しないこと。
- **NFR-011**: 対象リポジトリ外への書き込みを既定で許可しないこと。
- **NFR-012**: `--dangerously-bypass-approvals-and-sandbox` 相当を既定で使用しないこと。
- **NFR-013**: ログ出力時に既知のcredential形式と設定されたsecretをマスクすること。
- **NFR-014**: force push、履歴破壊、Issue削除、リポジトリ削除を通常フローに含めないこと。

### 7.3 可観測性

- **NFR-020**: 人間向けログと機械可読イベントを分離して提供すること。
- **NFR-021**: すべての状態遷移にリポジトリID、Issue番号、run ID、時刻、理由を含めること。
- **NFR-022**: ログローテーションまたは保持上限を持つこと。
- **NFR-023**: `doctor` で停止理由と復旧手順を提示できること。
- **NFR-024**: 待機中の監視はモデル呼び出しを発生させないこと。モデルを使うのはattention発生後の要約、質問、回答反映に限定すること。

### 7.4 移植性と保守性

- **NFR-030**: 初期対象は Apple Silicon macOS とする。
- **NFR-031**: コアロジックは `launchd`、GitHub、Codex のアダプターから分離すること。
- **NFR-032**: 設定ファイルとCLIの後方互換性を管理するため、schema versionを持つこと。
- **NFR-033**: ユニットテストでは GitHub や Codex の実サービスを必要としないこと。
- **NFR-034**: 現行v2の設定・stateは暗黙に並列・distributed modeへ移行せず、concurrency 1の動作を維持すること。

## 8. 運用上の前提

### 8.1 Mac mini

- Codex Remote を利用するには、ChatGPT desktop app が起動し、対象Macがオンラインかつ接続済みである必要がある。
- ループ本体だけを動かす場合、desktop app は実行主体ではない。ただしスマートフォンからCodex Remoteで監視・回答する経路には必要である。
- macOS の「ディスプレイがオフのときに自動でスリープさせない」設定を有効にすることを推奨する。これは Codex ではなく macOS の設定である。
- LaunchAgent はユーザーのログインセッションで動く。再起動直後から使うには、少なくとも対象ユーザーがログインしている必要がある。
- ディスプレイを常時点灯させる必要はない。
- LaunchDaemonと自動ログインは採用しない。FileVault unlockとlogin前の無人復旧は要件外とし、logout・再起動は発生時または計画保守時の運用確認とする。判断根拠は[ADR-0001](adr/0001-macos-execution-model.md)を正本とする。

### 8.2 認証

- `codex login` が完了していること。
- `gh auth status` が成功し、対象リポジトリへの Issue、branch、Pull Request 操作権限があること。
- Git の user name と email が設定されていること。

## 9. 受け入れ条件

### AC-1: taskから独立した継続

監視用 task からループを開始した後、その task を閉じても、ready Issue が処理され続ける。

### AC-2: 再接続

別の監視用 task で status/watch を実行すると、現在の Issue と状態を確認できる。

### AC-3: 入力待ち

ワーカーが `needs_input` を返すと質問が永続化され、監視用 task に表示される。回答後、同じ worktree で処理が再開する。

### AC-4: 再起動復旧

Codex ワーカーまたは supervisor を強制終了しても、LaunchAgent の再起動後に二重PRを作らず復旧する。

### AC-5: キュー処理

3件の ready Issue を登録すると、設定された順序で1件ずつ処理され、それぞれに結果が反映される。

### AC-6: スマートフォン運用

Mac mini に物理アクセスせず、Codex Remote から起動、状態確認、質問への回答、停止ができる。

### AC-7: event取りこぼしからの復旧

`needs_input` 保存後のevent通知を意図的に破棄しても、watchがreconciliation間隔内に永続状態から質問を検出する。

### AC-8: preflightで停止しない

実行profileを一意に判定できないIssueでもユーザー確認を要求せず `extended` を選び、初回worker内で実装へ進む。

### AC-9: 外部producerからの投入

GitHub UI、CLI/API、automation、または別ホストのCodexから作成したIssueでも、同じ着手可能条件を満たせばMac mini上のsupervisorが取得して処理を開始する。

### AC-10: 監視task未接続時の永続化

監視taskを閉じた状態で`needs_input`へ遷移しても、pending requestは永続snapshotに残る。再接続した監視taskはstatus-first手順で同じrequestを即時再表示し、supervisor再起動でも失われない。

## 10. 実装フェーズ

### Phase 1: ローカルMVP

- Go CLI の骨格
- 設定読み込みと検証
- 単一リポジトリ、単一ワーカー
- ローカルfixtureを使った状態機械

### Phase 2: GitHub/Codex統合

- `gh` によるIssue選択とclaim
- worktree作成
- `codex exec` と構造化結果
- preflightと`standard` / `extended` execution profile
- supervisor管理のcontinuation
- draft PR 作成

### Phase 3: 常駐化

- LaunchAgent の生成、登録、起動、停止
- 永続状態、再起動復旧、ログ
- doctor

### Phase 4: スマートフォン監視

- Codex Skill
- watch/answerフロー
- event通知と60秒reconciliationを組み合わせたwatch
- 監視task切断と再接続
- 通知を含むE2E確認

### Phase 5: 配布品質

- インストール・更新・アンインストール
- 設定migration
- 障害注入テスト
- 運用runbook

## 11. OpenAI公式仕様への依存

本要件は、2026-08-17時点の以下の公式OpenAIドキュメントを前提とする。利用可否の詳細とlocal schema確認は[Codex公式仕様確認](codex-capability-review.md)を正本とする。

- [Remote connections](https://learn.chatgpt.com/docs/remote-connections): 接続済みコンピューター上のchatをスマートフォンから開始・監視・指示・承認できる
- [Projects and chats](https://learn.chatgpt.com/docs/projects): 同一ローカルプロジェクトで複数taskを整理し、頻繁に使うtaskをピン留めできる
- [Notifications](https://learn.chatgpt.com/docs/notifications): Desktopでpermission/question notificationsを設定でき、Activityからunread、running、回答待ちのchatを確認できる
- [Long-running work](https://learn.chatgpt.com/docs/long-running-work): Goalは明確な成果、制約、完了条件を持つ長時間作業に使う
- [Codex App Server](https://learn.chatgpt.com/docs/app-server): 将来の再評価候補となるthread Goal、resume、turn start、token usageのprogrammatic interface。現行runtimeは使用しない
- [Integrated terminal](https://learn.chatgpt.com/docs/integrated-terminal): Codex taskから実行中のterminal出力を確認できる
- [Developer commands](https://learn.chatgpt.com/docs/developer-commands?surface=cli): `codex exec`、session resume、`--json`、`--output-schema`、sandbox指定

外部製品の未公開APIや、外部プロセスからCodex taskの表示状態を直接変更する機能には依存しない。Codex taskが `watch` を実行し、そのコマンドが入力待ちイベントを返すことで、Codexがユーザーへ質問する。

Goalは外側のIssueキューsupervisorの代替には使わない。現行のheadless workerはsupervisor管理のexecution profileと`codex exec` / `codex exec resume`だけを使う。App Server方式は中核品質の安定後に別Issueで再評価する。待機中のtool callに対する製品全体の厳密なゼロトークン保証は公式文書にないため要件に含めず、Go側の監視がモデル呼び出しを行わないことを保証範囲とする。
