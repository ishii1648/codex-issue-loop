# codex-issue-loop システム仕様

## 1. 位置づけ

`codex-issue-loop` は配布リポジトリ名、`agent-loop` はユーザーが操作するCLI名とする。

ループの制御は決定論的なGoプログラムが担当し、実装上の判断とコード変更のみをCodexワーカーへ委譲する。

## 2. 全体構成

```text
ChatGPT mobile app
        │ Codex Remote
        ▼
ChatGPT desktop app on Mac mini
        │
        ├─ [LOOP] monitor task
        │      │ Skill
        │      ▼
        │   agent-loop start/status/watch/answer
        │      │
        │      ▼
        │   launchctl ──► launchd LaunchAgent
        │                         │
        │                         ▼
        │                  agent-loop run
        │                    supervisor
        │                    │        │
        │                    │        └─ persistent state/events
        │                    ▼
        │                 GitHub Issues
        │                    │ pick/claim
        │                    ▼
        │              git worktree + codex exec
        │                    │
        │                    ▼
        │              branch / draft PR
        │
        └─ [INTAKE] new issue task ──► GitHub Issue
```

### 2.1 責務境界

| コンポーネント | 責務 | 保持しないもの |
| --- | --- | --- |
| Codex監視task | ユーザーとの会話、CLI操作、質問表示 | ループの正本状態 |
| agent-loop Skill | 自然言語からCLIへの安全な操作手順 | 常駐プロセス、Issue選択ロジック |
| agent-loop CLI | 設定、プロセス制御、監視、回答登録 | 実装判断 |
| launchd | supervisorの起動と再起動 | Issue状態機械 |
| supervisor | Issue選択、状態遷移、Codex起動、復旧 | プロダクト判断 |
| Codex worker | 1 Issueの調査、実装、検証、結果報告 | 次Issueの選択、無限ループ |
| GitHub | Issue/PRの共有状態 | ローカル実行詳細 |

Codex Skill の実行主体はCodex taskである。Skillは実行可能プロセスではなく、Codexが読む操作手順である。長時間動き続ける実行主体は `agent-loop run` supervisor である。

## 3. 技術選定

### 3.1 言語

Goを採用する。

理由:

- 単一バイナリで配布しやすい
- 常駐プロセス、signal、subprocess、ファイルロックを標準機能中心で実装できる
- macOS/arm64向けのビルドとテストが容易
- 対象リポジトリの言語やruntimeに依存しない

初期実装ではCGOを必須にしない。永続状態は原子的JSON snapshotとappend-only JSONL event logを用いる。要件が増えた場合のみ組み込みDBへの移行を検討する。

### 3.2 外部コマンド

- `git`: worktree、branch、commit状態の操作
- `gh`: GitHub Issue、label、comment、Pull Requestの操作
- `codex`: 非対話ワーカー
- `launchctl`: LaunchAgentの登録と制御

各コマンドは絶対パスを登録時に解決し、LaunchAgentの限定されたPATHに依存しない。

## 4. ディレクトリ構成

### 4.1 配布リポジトリ

```text
codex-issue-loop/
├─ cmd/agent-loop/
├─ internal/
│  ├─ config/
│  ├─ supervisor/
│  ├─ state/
│  ├─ github/
│  ├─ codex/
│  ├─ gitworktree/
│  ├─ launchd/
│  └─ observe/
├─ schemas/
│  └─ worker-result.schema.json
├─ skill/
│  └─ agent-loop/SKILL.md
├─ launchd/
│  └─ agent-loop.plist.tmpl
├─ scripts/
├─ docs/
├─ go.mod
└─ README.md
```

### 4.2 対象リポジトリ

```text
target-repository/
├─ .agent-loop.yaml
├─ AGENTS.md                 # 任意だが推奨
└─ ...
```

### 4.3 ユーザー領域

```text
~/Library/Application Support/codex-issue-loop/
├─ registry.json
└─ repos/<repo-id>/
   ├─ state.json
   ├─ events.jsonl
   ├─ supervisor.log
   ├─ supervisor.err.log
   ├─ lock
   └─ prompts/

~/Library/LaunchAgents/
└─ com.codex-issue-loop.<repo-id>.plist

~/.codex/skills/agent-loop/
└─ SKILL.md
```

`repo-id` は GitHub の `owner/repo` とcanonicalなローカルパスから生成した、人間が識別可能なprefix付きstable hashとする。リポジトリ移動時は再登録を必要とする。

## 5. 設定仕様

設定ファイル名は対象リポジトリ直下の `.agent-loop.yaml` とする。

```yaml
version: 1

github:
  repo: ishii1648/example
  ready_labels: [codex-loop:ready]
  exclude_labels: [blocked, do-not-automate]
  running_label: codex-loop:running
  needs_input_label: codex-loop:needs-input
  failed_label: codex-loop:failed
  done_label: codex-loop:done

queue:
  poll_interval: 60s
  concurrency: 1
  order: issue_number_asc
  max_attempts: 3
  continue_after_needs_input: true

worker:
  command: codex
  model: null
  sandbox: workspace-write
  ephemeral: true
  timeout: 2h

git:
  branch_prefix: codex/issue-
  worktree_root: null
  base_branch: main

completion:
  create_draft_pr: true
  close_issue: false
```

### 5.1 設定規則

- `version` は必須。未知のmajor versionはエラーとする。
- `github.repo` は `owner/name` 形式で必須。
- `queue.concurrency` はMVPでは `1` のみ許可する。
- `worker.model: null` はユーザーのCodex既定値を使う。
- `worker.sandbox` の既定値は `workspace-write` とする。
- worktree root未指定時はユーザー状態領域配下を使う。
- durationはGo duration形式とする。
- 未知キーは設定ミスを検出するため既定でエラーとする。
- secretsを設定ファイルに記述しない。

## 6. CLI仕様

### 6.1 共通規則

```text
agent-loop <command> [options]
```

- 対象は `--repo <path>`、現在ディレクトリ、registryの順で解決する。
- 対象が一意に決まらなければ終了コード `2` と候補を返す。
- `--json` 指定時はstdoutにJSONだけを出し、診断ログはstderrへ出す。
- 破壊的・復旧用操作は通常コマンドから分離する。

### 6.2 コマンド一覧

| コマンド | 目的 |
| --- | --- |
| `install` | バイナリに対応するSkillと共通ディレクトリをセットアップする |
| `uninstall` | 実行中プロセスを確認してインストール物を削除する |
| `register --repo PATH` | 対象リポジトリを検証し、registryとplistを生成する |
| `unregister --repo PATH` | 停止確認後に登録を解除する |
| `start` | LaunchAgentをbootstrap/kickstartする |
| `stop` | LaunchAgentを停止する。Issue状態は保持する |
| `restart` | 停止後に再起動する |
| `status` | snapshot、launchd状態、GitHub状態の要約を返す |
| `watch` | イベントを追跡する |
| `answer` | 未回答requestへ回答を登録する |
| `logs` | supervisorまたはIssue別ログを表示する |
| `doctor` | 依存関係、認証、設定、電源条件、状態整合性を検査する |
| `run` | launchd専用の内部supervisorエントリーポイント |

### 6.3 start

```text
agent-loop start --repo /path/to/repo [--json]
```

処理:

1. 登録と設定を検証する
2. 既に実行中なら成功として現在状態を返す
3. plistを `launchctl bootstrap gui/<uid>` で登録する
4. 必要に応じて `launchctl kickstart` する
5. 起動確認を行う

`start` 自身は常駐しない。

### 6.4 watch

```text
agent-loop watch --repo /path/to/repo --until-attention [--json]
```

`--until-attention` の終了条件:

- 未回答の `needs_input`
- supervisorが `blocked`
- 明示的な停止
- `--until-idle` も指定された場合のキュー空

未回答質問が既に存在する場合、待機せず即時返却する。watchはsnapshotとevent logを読み取るだけで、supervisorの親プロセスにはならない。

### 6.5 answer

自由記述:

```text
agent-loop answer --request-id req_... --message "回答"
```

stdin:

```text
agent-loop answer --request-id req_... --message-file -
```

処理:

1. request IDと未回答状態を検証する
2. 回答を原子的に保存する
3. `answer_recorded` イベントを追記する
4. supervisorをwakeする
5. 既に同一回答が登録済みなら成功、異なる回答ならconflictとする

### 6.6 終了コード

| code | 意味 |
| --- | --- |
| `0` | 成功、または期待されたwatch終了 |
| `1` | 実行時エラー |
| `2` | 引数・設定エラー |
| `3` | 対象が未登録または曖昧 |
| `4` | 競合、古いrequest、二重claim |
| `5` | 認証・権限不足 |

## 7. launchd仕様

登録単位は1対象リポジトリにつき1 LaunchAgentとする。

主要plist設定:

- `Label`: `com.codex-issue-loop.<repo-id>`
- `ProgramArguments`: 絶対パスの `agent-loop run --repo <absolute-path>`
- `RunAtLoad`: true
- `KeepAlive`: 異常終了時に再起動する条件
- `ThrottleInterval`: 再起動stormを防ぐ値
- `WorkingDirectory`: 対象リポジトリ
- `StandardOutPath` / `StandardErrorPath`: repo別状態ディレクトリ
- `EnvironmentVariables`: 必要最小限のPATHとHOME。tokenは含めない

LaunchAgentなので、ユーザーがログアウトしている間は動作保証しない。システムwideなLaunchDaemonは、ユーザーcredential、HOME、Codex認証との境界が複雑になるためMVPでは採用しない。

## 8. supervisor状態機械

### 8.1 リポジトリ状態

```text
stopped
   │ start
   ▼
starting ──► polling ◄──────────────┐
                 │ issue found      │
                 ▼                  │
              claiming              │
                 │                  │
                 ▼                  │
              running               │
        ┌────────┼─────────┐        │
        ▼        ▼         ▼        │
   completed needs_input retry_wait  │
        │        │         │         │
        │        │ answer  └─────────┘
        │        ▼
        │      running
        └───────────────────────────► polling

running ──fatal/nonrecoverable──► blocked
```

`needs_input` はIssue単位の状態であり、`continue_after_needs_input: true` の場合、supervisor全体は別Issueのpollingを続けてよい。ただし同一worktreeは回答まで変更しない。

### 8.2 Issue状態

- `claimed`
- `running`
- `needs_input`
- `retry_wait`
- `completed`
- `failed`
- `blocked`

すべての遷移はsnapshot更新前にevent logへ記録し、再起動時にsnapshotとGitHubを照合する。

## 9. Issue選択とclaim

### 9.1 着手可能条件

Issueは以下をすべて満たす場合に着手可能とする。

- stateがopen
- `ready_labels` をすべて持つ
- `exclude_labels` を一つも持たない
- running、needs-input、done、failedラベルを持たない
- Pull Requestではない
- ローカル状態で処理中ではない

### 9.2 並び順

MVPの既定はIssue番号昇順とする。将来、priorityラベルと作成日時を追加できるが、GitHub APIの返却順には依存しない。

### 9.3 claim手順

1. 候補一覧を取得してローカルでsortする
2. 先頭Issueの最新状態を再取得する
3. runningラベルを追加し、readyラベルを外す
4. run IDを含む開始コメントを冪等キー付きで作成する
5. ローカル状態を `claimed` にする

GitHub APIには汎用的なcompare-and-swapがないため、MVPは「同一リポジトリを処理するsupervisorは1つ」という運用制約を置く。複数ホスト対応時はGitHub外の分散lockが必要になる。

## 10. worktreeとGit仕様

- branch: `codex/issue-<number>-<slug>`
- worktree: `<worktree-root>/<repo-id>/issue-<number>`
- base branchは処理開始時にfetchした設定値
- 既存branchまたはPRがある場合は対応関係を検証して再利用する
- ユーザーの通常working treeは変更しない
- 未コミット変更があるworktreeを自動削除しない
- force pushを行わない
- 完了後もPRがopenの間はworktreeを保持できる設定を用意する

## 11. Codexワーカー仕様

### 11.1 起動

概念上、次の形式で実行する。

```text
codex exec \
  --cd <issue-worktree> \
  --sandbox workspace-write \
  --ephemeral \
  --json \
  --output-schema <worker-result.schema.json> \
  <generated-prompt>
```

実際の引数は起動前に `codex exec --help` とversion capabilityを検査する。非対応versionでは推測で継続せず `blocked` とする。

### 11.2 ワーカープロンプト

次を含む。

- repository、base branch、worktree
- Issue番号、title、body、関連コメント
- 現在の試行番号
- 過去の質問とユーザー回答
- 実行可能な範囲と禁止事項
- 対象リポジトリのAGENTS.mdに従う指示
- 実装、テスト、commit、push、draft PRに関する完了条件
- 構造化結果の意味
- 質問すべき条件と、推測して進めてよい条件

### 11.3 質問ポリシー

質問する:

- 要件の選択肢が複数あり、外部仕様やUI動作が大きく変わる
- データ削除、公開、課金、credential、権限拡大が必要
- リポジトリ内の情報だけでは安全に決められない
- 相反する受け入れ条件がある

質問せず合理的な仮定で進める:

- 命名や局所的実装詳細
- 既存規約から一意に推測できる事項
- 容易に戻せる内部構造
- テスト追加やformatなど通常の実装作業

質問時は、調査済み事実、判断が必要な理由、推奨案、2〜3個までの選択肢を返す。単なる進捗報告を質問にしない。

### 11.4 構造化結果

```json
{
  "version": 1,
  "status": "completed",
  "summary": "Implemented ...",
  "question": null,
  "tests": [
    {"command": "go test ./...", "result": "passed"}
  ],
  "git": {
    "branch": "codex/issue-123-example",
    "commit": "abc1234",
    "pull_request_url": "https://github.com/owner/repo/pull/456"
  },
  "retry": null
}
```

`needs_input` の例:

```json
{
  "version": 1,
  "status": "needs_input",
  "summary": "Both APIs are compatible with the current code.",
  "question": {
    "text": "Which compatibility policy should be used?",
    "reason": "The choice changes the public API.",
    "recommended_option": "preserve-v1",
    "options": [
      {"id": "preserve-v1", "label": "Preserve v1"},
      {"id": "breaking-v2", "label": "Adopt v2"}
    ],
    "allow_free_text": true
  },
  "tests": [],
  "git": null,
  "retry": null
}
```

ワーカーのstdout JSONLは実行ログとして保存し、最終メッセージのみをschema検証済み結果として採用する。schema不一致は `retryable_failure` とする。

## 12. 永続状態とイベント

### 12.1 state.json

最低限次を保持する。

```json
{
  "version": 1,
  "repo_id": "example-a1b2c3d4",
  "supervisor": {
    "state": "polling",
    "pid": 12345,
    "started_at": "2026-08-15T08:00:00Z",
    "updated_at": "2026-08-15T08:10:00Z"
  },
  "issues": {},
  "pending_requests": {}
}
```

一時ファイルへのwrite、fsync、renameにより原子的に更新する。ファイルpermissionはユーザーのみ読み書き可能とする。

### 12.2 events.jsonl

各行を独立したJSONイベントとする。

共通フィールド:

- `version`
- `event_id`
- `sequence`
- `timestamp`
- `repo_id`
- `issue_number`（該当時）
- `run_id`（該当時）
- `type`
- `payload`

主要イベント:

- `supervisor_started`
- `issue_claimed`
- `worker_started`
- `worker_completed`
- `input_requested`
- `answer_recorded`
- `retry_scheduled`
- `issue_completed`
- `issue_failed`
- `supervisor_blocked`
- `supervisor_stopped`

## 13. 監視とCodex task連携

### 13.1 Skillの標準フロー

監視開始:

1. `agent-loop doctor --json`
2. `agent-loop start --json`
3. `agent-loop status --json`
4. 未回答質問がなければ `agent-loop watch --until-attention --json`

入力待ち:

1. watchの結果を人間向けに要約する
2. questionをほぼそのままユーザーへ提示する
3. 回答後に `agent-loop answer` を実行する
4. statusを確認する
5. watchへ戻る

停止:

1. 現在Issueと未コミット状態を表示する
2. ユーザーの停止意図を確認する
3. `agent-loop stop` を実行する
4. 最終statusを表示する

### 13.2 Codex app上の制約

外部supervisorからCodexアプリ内taskへ直接メッセージを挿入したり、task状態を変更したりする非公開機能には依存しない。

`Needs input` の表示とpush通知は、監視task内で実行中のwatchが戻り、Codex自身がユーザーへ質問することで成立する。監視taskが接続されていない間も質問は永続状態に残るが、Codex由来の即時push通知は保証しない。再接続時には未回答質問を即時表示する。

将来、公式に利用可能なtask wakeupまたは通知APIが提供された場合は、optional adapterとして追加できる。

## 14. エラー処理と再試行

### 14.1 分類

| 種類 | 例 | 動作 |
| --- | --- | --- |
| 一時障害 | network timeout、GitHub 5xx、Codex一時失敗 | backoff後に再試行 |
| Issue固有の恒久障害 | test不能、競合要件、許可されない操作 | Issueをblocked/failedにして次へ |
| supervisor全体の障害 | auth失効、config不正、binary消失 | supervisorをblockedにして通知対象 |
| ユーザー入力待ち | product判断、権限承認 | requestを保存し、設定に応じて次へ |

### 14.2 backoff

- 初回: 5秒
- 倍率: 2
- 上限: 5分
- jitter: ±20%
- Issueワーカーの既定上限: 3回
- polling失敗はsupervisorを終了させず、連続失敗閾値でblockedにする

## 15. 再起動時のreconciliation

supervisor起動時に次を行う。

1. lockを獲得し、二重supervisorを拒否する
2. state snapshotとevent logの整合性を検証する
3. GitHub Issue、branch、PRの現況を取得する
4. 保存されたworker PIDが生存しているか確認する
5. worktreeとGit状態を検証する
6. completed PRがあれば完了へ収束させる
7. 実行途中でworkerが消えていればretryへ移す
8. 未回答requestはneeds_inputのまま保持する
9. reconciliationイベントを記録してpollingを開始する

## 16. セキュリティ仕様

- subprocess引数はshell文字列として連結せず、argv配列で起動する
- Issue本文をshellとして評価しない
- worktree pathはcanonical化し、許可root配下であることを確認する
- plistと状態ファイルをユーザー所有、最小permissionで作成する
- credentialをpromptへ明示的に埋め込まない
- Codex sandboxは既定で `workspace-write`
- dangerous bypassは設定schemaでもMVPでは許可しない
- GitHub Issueは信頼済み入力とはみなさず、prompt injectionの可能性をworkerへ明示する
- stopはプロセスを終了するが、worktreeや未コミット変更を削除しない
- reset/purgeを実装する場合は別コマンドとし、明示確認を必須にする

## 17. テスト仕様

### 17.1 ユニットテスト

- configの正常・異常系
- Issue filterと決定論的sort
- 全状態遷移
- answerの冪等性とconflict
- structured result validation
- retry/backoff
- secret masking
- repo-id生成

### 17.2 統合テスト

- fake GitHub adapter + fake Codex process
- worktree作成、再利用、異常終了
- supervisor二重起動防止
- snapshot途中書き込みからの復旧
- worker kill後のreconciliation
- watchの接続、切断、複数接続

### 17.3 macOS E2E

- install/register/start/stop/uninstall
- `launchctl`による自動再起動
- Macの画面off中の継続
- Codex Remoteからの監視開始
- `needs_input`のスマートフォン通知、回答、再開
- ChatGPT desktop taskを閉じた後のsupervisor継続

## 18. 実装時に確定する項目

以下は要件を変えずに実装検証で確定する。

- Goの最低version
- event logのrotation閾値
- Codex CLIの最低対応version
- `gh` JSON fieldとlabel更新の具体コマンド
- worker timeout時のgrace period
- worktreeの既定保持期間
- desktop app更新によるRemote/通知表示差異のE2E手順

