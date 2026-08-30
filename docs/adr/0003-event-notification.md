# ADR-0003: fsnotify/kqueueとreconciliation pollingを採用する

- Status: Accepted
- Date: 2026-08-16
- Decision owners: codex-issue-loop maintainers

## Context

`agent-loop watch`は、永続snapshotが`needs_input`や`blocked`へ変わったことを低遅延で検知する必要がある。一方、event通知だけで完全配送を保証すると、監視clientの切断、supervisor再起動、OSのevent欠落、file descriptor不足、state fileのatomic renameをすべて独自protocolで解決する必要がある。

現在のstate更新はtemporary fileを同じdirectory内でrenameして`state.json`を置換し、`events.jsonl`へappendする。個別fileを監視するとatomic replacementでwatch対象inodeを失うため、state directoryを監視して対象basenameだけをwake hintとして扱う必要がある。fsnotifyの公式READMEも、atomic updateされるfile自身ではなくparent directoryを監視することを推奨している。

## Decision

macOSのevent wakeには`github.com/fsnotify/fsnotify`のdirectory watchを正式採用する。fsnotifyはmacOSでkqueue backendを使う。通知は配送保証を持つevent busではなく、永続snapshotを再読込するための**hint**と定義する。

watch algorithmは次の契約を守る。

1. subscription前にsnapshotを読む。
2. state directoryをfsnotifyへ登録する。
3. subscription後にsnapshotをもう一度読み、read-subscribe間のraceを閉じる。
4. `state.json`または`events.jsonl`のeventを受けたらpayloadを解釈せずsnapshotを読む。
5. eventがなくても内部のjitter付きreconciliation間隔でsnapshotを読む。
6. watcher生成・directory登録が失敗した場合、またはevent/error channelが閉じた場合はwatchを終了せずpolling-onlyへ降格する。
7. 各watch clientは独立したwatcherを持ち、client間でeventを消費し合わない。

既定reconciliation間隔は60秒である。eventを全欠落させても次のreconciliationまでにattention状態へ収束する。event errorはattentionの根拠にせず、state loadが成功する限り利用者への追加操作を要求しない。

## Options considered

| Option | Atomic rename | 複数client・再接続 | Resource/complexity | Failure fallback | Decision |
| --- | --- | --- | --- | --- | --- |
| fsnotifyでstate directoryを監視 | directory eventから置換後fileを再読込できる | clientごとに独立watcher。server不要 | macOS kqueueはwatch対象ごとにFDを使うが、本設計はrepositoryごとに1 directoryだけ | watcher作成・購読・channel終了時にpolling-only | 採用 |
| Unix domain socket | application protocolで通知可能 | serverがclient registry、再接続、backpressureを管理 | socket path清掃、permission、framing、fan-out、server lifecycleが必要 | 切断中eventは別途snapshot reconciliationが必要 | 不採用 |
| Named pipe/FIFO | byte streamとして送れる | 複数readerへのbroadcastではなく、reader間でdataを消費し得る | open/write blocking、framing、stale FIFOの処理が必要 | writer不在・reader停止を避けてもpollingが必要 | 不採用 |
| process内channel | supervisor内では低コスト | 別processの`watch`へ配送できず、再起動で消える | 最小 | cross-process要件を満たさない | 不採用 |
| pollingのみ | rename・再接続の影響なし | clientごとに独立 | 最小だが、反応時間を短くするとI/O wakeが増える | 方式自体が最終保証 | 単独採用せず、保証層として併用 |

fsnotify v1.7.0の[README](https://github.com/fsnotify/fsnotify/blob/v1.7.0/README.md)は、macOS/BSDでkqueueをsupported backendとし、個別fileのatomic replacement、network filesystem非対応、kqueueのfile descriptor消費を明記している。Appleの[`kqueue(2)`](https://developer.apple.com/library/archive/documentation/System/Conceptual/ManPages_iPhoneOS/man2/kqueue.2.html)も、登録したeventの通知queueとfile descriptor lifecycleを定義している。Unix socket案の追加contractはGoの[`net.ListenUnix`](https://pkg.go.dev/net#ListenUnix)相当のserver lifecycleを必要とする。

## Failure model

| Failure | Behavior |
| --- | --- |
| state fileのatomic rename | directory eventをhintとしてsnapshotを再読込する |
| event coalescing・全欠落 | 60秒reconciliationで収束する |
| readとsubscribeの間で更新 | subscribe後の2回目のreadで検出する |
| watcher作成・Add失敗、FD不足 | polling-onlyへ降格する |
| event/error channel終了 | channelを無効化し、timer pollingを継続する |
| 複数watch | 各clientが独立して同じrevisionを読む |
| watch終了後の再接続 | 新しいwatcherと最初のsnapshot readから開始する |
| supervisor再起動 | state directoryとsnapshotはprocess外に残り、watchは同じdirectory eventまたはpollingで検出する |
| system sleep/wake | sleep中の即時性は保証せず、wake後のreconciliationで現在snapshotへ収束する |
| state directory自体の外部削除・置換 | event方式の障害ではなくdurable stateの破壊として扱う。自動復元や空stateへの切替を保証せず、backup/restoreとdoctorの対象にする |

監視directoryをnetwork filesystemへ置く構成はサポートしない。state rootはMac miniのlocal filesystemを使う。

## Verification

自動testは次を固定する。

- event channelを一度も起こさなくても短いreconciliation intervalで`needs_input`へ収束する。
- watcher subscriptionがFD不足相当のerrorを返してもpollingで収束する。
- read-subscribe-readの間に更新しても取りこぼさない。
- 複数の実fsnotify watcherが同じstate revisionを受け取り、終了後に新しいwatcherで再接続できる。
- event/error channelが閉じてもtimer pollingを継続する。

2026-08-16にApple Silicon macOS上で`go test ./internal/application/observe -count=20`を実行し、実fsnotifyを使う複数watch・再接続を含むsuiteが20回、8.486秒で完了した。この反復には合計60回のwatcher subscription lifecycleが含まれ、hang、FD error、取りこぼしは発生しなかった。これは実装regressionの基準であり、event到達時間のSLOではない。resource modelはwatch clientごとに1 watcher・1 state directoryであり、repository配下を再帰監視しない。

macOS sleep/wakeは端末状態に依存するためmilestone blockerにせず、実際の発生時または計画保守時の運用確認とする。異常があった場合だけmacOS version、fsnotify version、`doctor`、state revision、時刻を記録してIssue化する。

## Consequences

### Positive

- 新しいdaemon、socket protocol、message brokerを追加せず低遅延wakeを得られる。
- event配送の完全性を要求せず、既存のdurable stateとrevisionを正本にできる。
- supervisor、watch clientの再起動と複数clientを同じ契約で扱える。
- FD不足やbackend障害がattention監視の停止へ直結しない。

### Negative

- 通知欠落時は最大でreconciliation interval分だけ検知が遅れる。
- 各watch clientがkqueueとdirectory watch用resourceを使う。
- polling-onlyへの降格中はevent wakeよりI/Oとlatencyが増える。
- sleep中の通知到達時間は保証しない。

## Revisit triggers

- 1 hostで多数のwatch clientを常時接続し、FD数が実運用上の制約になる。
- stateをlocal filesystem以外へ移す。
- 複数host coordinatorがglobal watch streamとdurable cursorを提供する。
- 60秒より厳しい通知SLOが必要になり、fsnotify欠落時にも満たす必要がある。
