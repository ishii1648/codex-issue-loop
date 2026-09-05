# Architecture

`monitor/cmd/agent-loop-monitor`はsupervisorとは別のentrypointです。設定を読み、repositoryごとにread-only GitHub adapterを呼び、pureな状態判定結果をmonitor専用storeへcommitします。一つのrepositoryで失敗しても次のrepositoryをpollし、失敗したrepositoryだけを`UNKNOWN`にします。

依存方向は`cmd -> app/monitor -> github + model + store + config`です。`internal/application/supervisor`、`internal/domain/issue`、`internal/adapter/state`への依存はarchitecture testで拒否します。

GitHub adapterが実行できる外部操作は`gh api --method GET --paginate --slurp`だけです。Issue、label、commentなどのmutation methodをinterfaceへ含めません。

永続rootは既定で`~/Library/Application Support/codex-issue-loop-monitor`です。repositoryごとに`repositories/<owner--repo>/current.json`と`intervals.jsonl`を持ちます。確定intervalをatomicに更新してからcurrent stateをatomic renameし、同じinterval IDの再commitを除外します。process再起動直後にpollすることで中断したcommitも同じ決定へ収束します。

LaunchAgentは`com.codex-issue-loop.monitor`であり、supervisorのrepository別LaunchAgentとは別に登録されます。
