# Architecture

`monitor/cmd/agent-loop-monitor`はsupervisorとは別のentrypointです。設定を読み、repositoryごとにread-only GitHub adapterを呼び、pureな状態判定結果をmonitor専用storeへcommitします。一つのrepositoryで失敗しても次のrepositoryをpollし、失敗したrepositoryだけを`UNKNOWN`にします。

可用性判定の依存方向は`cmd -> app/monitor -> github + model + store + config`です。`internal/application/supervisor`、`internal/domain/issue`、`internal/adapter/state`への依存はarchitecture testで拒否します。

GitHub adapterが実行できる外部操作は`gh api --method GET`だけです。open Issue一覧はpaginationし、現在actionableなIssueとbatchで変更されたIssueごとのevent履歴を現在snapshotから逆算し、phase開始・再入場時刻を取得します。repository Issue eventはnewest-firstのpage 1から永続cursorを含むpageまで一ページずつ取得し、そのpageで停止します。最大10ページ（1ページ100件）に制限し、上限までにcursorが見つからなければ現在snapshotの検証による再同期を行い、再生不能な過去区間を`UNKNOWN`として残します。通常pollでrepository event全履歴を`--paginate --slurp`しません。cursorを発見できない取得結果は完全な履歴としてstate machineへ渡しません。Issue、label、commentなどのmutation methodをinterfaceへ含めません。

state machineはbatch全体と現在snapshotの整合性を検証してから、永続queue snapshotを起点に、cursorより新しいeventとその間のdeadlineを時系列でreplayします。runningが存在する間はprocessing phaseを優先します。queue-level時計はIssue横断のready→runningまたはrunning→terminal/closeの進捗eventで更新し、個別runningの開始時刻・deadlineから集約しません。processing期限後はUNKNOWN、readyのみのadmission期限後はDOWNです。terminal後にreadyだけが残る場合はterminal event時刻からadmission windowを開始します。初回・再同期のrunningは進捗時計が不明なのでUNKNOWNとします。

永続rootは既定で`~/Library/Application Support/codex-issue-loop-monitor`です。repositoryごとに`repositories/<owner--repo>/current.json`と`intervals.jsonl`を持ちます。current stateにはevent cursorとqueue-level phase/deadline、判定versionと切替境界を保存します。intervalにも判定versionを保存し、旧履歴は保持したまま新契約の集計ではUNKNOWNへ分離します。migrationとreplayの境界は[specification](specification.md#判定契約の切替と旧履歴)を参照してください。確定intervalをatomicに更新してからcurrent stateをatomic renameし、同じtransition IDの再commitを除外します。event ID集合は累積しません。process再起動直後にpollすることで中断したcommitも同じ決定へ収束します。

LaunchAgentは`com.codex-issue-loop.monitor`であり、supervisorのrepository別LaunchAgentとは別に登録されます。

Dashboard の `/api/status` は、可用性判定と独立して `internal/platform/runtimemetadata` の専用公開メタデータを読み、応答の各 repository に `runtime`（`version`、`observed_at`、`expires_at`、照合不能時は null）を付加します。CLI status、monitor の snapshot・interval・report にこの情報は保存しません。`at` や期間選択は runtime の観測時刻へ影響しません。

公開先は既存 `AGENT_LOOP_HOME`（未設定時は `~/Library/Application Support/codex-issue-loop`）の `runtime-metadata/<小文字 owner/name の SHA-256>/<repo_id>.json` です。schema_version は独立した `1` で、repository、repo_id、runtime_version、pid、boot_session_id、process_started_at（seconds / microseconds）、written_at を持ちます。実行中 supervisor の ReleaseVersion を起動検証後・scheduler 開始前に、既存 fsutil の一時ファイル・fsync・rename で公開します。ディレクトリは0700、ファイルは0600です。正常終了時は supervisor lock 解放前に自身の記録だけを削除し、公開・削除失敗はログに記録します。

読み取りは同一 macOS ホスト・同一ユーザー・同一 AGENT_LOOP_HOME に限定します。OS の boot session、PID、起動時刻、所有ユーザーを照合し、終了済み・zombie を除外します。対応する生存 runtime が複数、メタデータの形式不正・欠損・権限不足・symlink、読取中の変更は不明とします。停止後の古い記録は生存確認で除外できる場合だけ無視します。registry、assignment、supervisor snapshot、ホスト共通 CLI の version による補完はしません。

version はプロセス存続中に不変なので起動時だけ記録し、written_at を heartbeat として失効させません。既存画面の15秒更新で OS を再照合し、観測結果の期限には既存 observation_timeout を使用します。runtime の観測失敗・失効はバッジだけを不明にし、GitHub による状態判定へ影響させません。追加設定・daemon・定期書き込みはありません。
