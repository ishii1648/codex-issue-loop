# Architecture

`monitor/cmd/agent-loop-monitor`はsupervisorとは別のentrypointです。設定を読み、repositoryごとにread-only GitHub adapterを呼び、pureな状態判定結果をmonitor専用storeへcommitします。一つのrepositoryで失敗しても次のrepositoryをpollし、失敗したrepositoryだけを`UNKNOWN`にします。

依存方向は`cmd -> app/monitor -> github + model + store + config`です。`internal/application/supervisor`、`internal/domain/issue`、`internal/adapter/state`への依存はarchitecture testで拒否します。

GitHub adapterが実行できる外部操作は`gh api --method GET`だけです。open Issue一覧はpaginationし、初回bootstrapでは現在actionableなIssueごとのevent履歴からphase開始時刻を取得します。repository Issue eventはnewest-firstのpage 1から永続cursorを含むpageまで一ページずつ取得し、そのpageで停止します。一回のpollは最大10ページ（1ページ100件）に制限し、上限までにcursorが見つからなければ`UNKNOWN`にします。通常pollでrepository event全履歴を`--paginate --slurp`しません。cursorを発見できない取得結果は完全な履歴としてstate machineへ渡しません。Issue、label、commentなどのmutation methodをinterfaceへ含めません。

state machineは永続queue snapshotを起点に、cursorより新しいeventとその間のdeadlineを時系列でreplayします。runningが存在する間はprocessing phaseを優先し、terminal後にreadyが残る場合だけterminal event時刻からadmission windowを開始します。

永続rootは既定で`~/Library/Application Support/codex-issue-loop-monitor`です。repositoryごとに`repositories/<owner--repo>/current.json`と`intervals.jsonl`を持ちます。current stateにはevent cursorとqueue-level phase/deadlineを保存します。確定intervalをatomicに更新してからcurrent stateをatomic renameし、同じtransition IDの再commitを除外します。event ID集合は累積しません。process再起動直後にpollすることで中断したcommitも同じ決定へ収束します。

LaunchAgentは`com.codex-issue-loop.monitor`であり、supervisorのrepository別LaunchAgentとは別に登録されます。
