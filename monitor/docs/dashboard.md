# ローカルダッシュボード

独立monitorの既存stateを読むAPI、Prometheus、Grafanaを使用します。supervisorやGitHubへの書き込みは行いません。GitHubをpollする既存の`com.codex-issue-loop.monitor`は引き続き必要です。

- Dashboard（失効監視付き）: <http://127.0.0.1:19110/>
- Grafana詳細: <http://127.0.0.1:13000/d/independent-monitor>
- Prometheus: <http://127.0.0.1:19090>
- read-only API: <http://127.0.0.1:19110/api/status>

すべてloopbackのみです。Grafanaはanonymous Viewer、ログイン画面なし、alerting無効です。ローカルの他ユーザーも閲覧可能です。外部公開、reverse proxy、通知、Alertmanagerは構成しません。

## 準備・起動

Python 3、Grafana、Prometheusを使用します。既存のHomebrew binaryを再利用し、なければ`brew install grafana prometheus`で導入します。`brew services start`は別設定のサービスを起動するため使いません。対象環境はGrafana 13.2.1、Prometheus 3.14.0、Infinity 4.0.0、公式prometheus plugin 13.1.7です。Grafana 13.2ではPrometheus datasourceも独立pluginであり、`preinstall_disabled=true`のこの構成では明示的な導入が必要です。専用rootに導入済みなら再インストールは不要です。

repository rootで実行します。`--binary`にはこの変更を含むmonitor binaryを指定します。観測サービスのbinaryやstateを変更する必要はありません。

```sh
python3 monitor/dashboard/manage.py prepare --binary /absolute/path/to/agent-loop-monitor
DASHBOARD_ROOT="$HOME/Library/Application Support/codex-issue-loop-monitor-dashboard"
grafana cli --homepath "$(brew --prefix grafana)/share/grafana" \
  --pluginsDir "$DASHBOARD_ROOT/plugins" plugins install yesoreyeram-infinity-datasource 4.0.0
grafana cli --homepath "$(brew --prefix grafana)/share/grafana" \
  --pluginsDir "$DASHBOARD_ROOT/plugins" plugins install prometheus 13.1.7
python3 monitor/dashboard/manage.py start
python3 monitor/dashboard/manage.py status
```

`--config`でmonitor設定、`--binary`でmonitor binary、`--root`でdashboard保存先、`--grafana-home`でGrafanaの`conf/defaults.ini`を含むディレクトリを指定できます。標準の対象は`ishii1648/codex-issue-loop`と`ishii1648/zeitreise`です。両repositoryをmonitor設定に含めてください。別の対象は`dashboard.json`のrepository selectorも合わせます。

ログイン時の自動起動には、生成した3つのplistをLaunchAgentsへリンクします。既存の同名ファイルがあれば上書きせず、その配置を確認します。独自rootの場合は以下の変数も合わせます。

```sh
DASHBOARD_ROOT="$HOME/Library/Application Support/codex-issue-loop-monitor-dashboard"
mkdir -p "$HOME/Library/LaunchAgents"
for plist in "$DASHBOARD_ROOT"/com.codex-issue-loop.monitor-dashboard.*.plist; do
  ln -s "$plist" "$HOME/Library/LaunchAgents/"
done
```

API、Prometheus、Grafanaのサービス名はそれぞれ`com.codex-issue-loop.monitor-dashboard.api`、`.prometheus`、`.grafana`です。root内の`grafana/`にGrafana DB、`plugins/`に両plugin、`prometheus/`に30日保持のTSDB、`logs/`にログを保存します。履歴の正本はmonitor設定の`state_dir`内の`repositories/`です。

## 停止・再起動・更新

```sh
python3 monitor/dashboard/manage.py stop
python3 monitor/dashboard/manage.py start
python3 monitor/dashboard/manage.py restart
```

`start`は登録済みサービスを保持します。`restart`は登録済みに`kickstart -k`、未登録に`bootstrap`を使います。plistのProgramArgumentsを変更した場合は`stop`後に`start`して再登録します。`stop`はデータを保持します。ログイン自動起動も止める場合は、この構成の3つのLaunchAgentsリンクだけを外します。

更新時は`stop`、binaryの配置、`prepare`、必要なplugin更新、`start`の順です。以前のbinary・plugin・設定を保存し、Grafana DB互換性は使用版の更新手順に従います。`prepare`はDB・TSDB・観測stateを削除しません。

## 表示と正確性

失効監視付きURLはAPIから直接描画し、repositoryを左右に比較します。現在状態は正常 HEALTHY=緑、異常 DOWN=赤、待機 IDLE=青で大きく表示し、UNKNOWNは「状態を確認できません」と表示します。選択期間は需要時稼働率を数値で、正確なtimelineをバーで表示します。UNKNOWNと未観測の区間は斜線と「観測できない区間」の凡例で示し、hoverで時刻を確認できます。現在までの1h/24h/7d/30d集計は補助表、状態開始・最終観測・理由・観測エラー・現在queueのIssue番号と期限は折り畳み詳細です。選択期間の観測率と未観測時間も詳細内で確認でき、開閉状態は自動更新後も保持します。Issue番号からGitHubへ進めます。queue-level期限と各Issue期限を区別します。履歴に当時のqueue一覧はないため、過去の対象Issueは復元しません。

需要時稼働率は`HEALTHY秒 / (HEALTHY秒 + DOWN秒)`です。IDLEは分母に入らず、分母0は`N/A（需要なし）`です。画面では「観測できた需要時間に対する割合」と説明します。UNKNOWNだけでも既知の需要が0なのでN/Aですが、観測率は0であり、需要がなかったとは断定できません。観測率は`(HEALTHY + DOWN + IDLE)秒 / 全期間秒`です。選択期間にUNKNOWNや未観測時間がある場合のみ「この期間には未観測の時間があります」と添え、混在期間への単一の正常性判定は行いません。未観測は正常時間に加算しません。

`/metrics`はCLIと同じ`effectiveSnapshot`、`effectiveIntervals`、`BuildReport`を使用します。状態コードはUNKNOWN=0、HEALTHY=1、DOWN=2、IDLE=3、需要なしは-1、不明な日時はNaNです。Issue番号・理由・errorをlabelに含めず、counterやscrapeサンプル比率で稼働率を計算しません。基準時刻は秒境界に切り捨て、その同じ時刻で全期間を計算します。読み取り中のsnapshot変更・重複区間・破損はHTTP 503として拒否します。

scrapeとdashboardは15秒更新です。monitor停止は既存の`observation_timeout`でUNKNOWNになり、queue deadlineまで境界が遡る場合があります。API停止は`up=0`、scrape基準時刻が45秒以上古い場合も現在パネルを無効化します。

通常は失効監視付きURLを開きます。ブラウザは15秒ごとにAPI経由でGrafanaのPrometheus datasourceへ基準時刻とupを照会します。通信エラー・欠損は照会時に、45秒以上古い基準時刻は1秒タイマーで検出し、要約全体を隠してUNKNOWN / 表示失効を明示します。status・report・timelineの失敗や、同じ期間のreportとtimelineの秒数が一致しない更新途中も表示を失効させます。タブ復帰時も期限を確認します。Grafana詳細を単独で開くとこの失効監視はありません。追加pluginは使いません。

詳細表は`/api/details?repo=owner/name`から、既存status計算結果を平坦化した`rows`を読みます。queueが空でも状態・理由・観測エラーを1行返し、未設定のIssue番号と期限はnullです。理由と観測エラーは同じ列に表示します。

Infinityは`/api/timeline`から区間を直接読み、backendの列順に関係なくGrafanaのorganize変換で開始・終了・状態の順へ並べます。選択期間へclipし、未観測部分をUNKNOWNで埋め、開区間の終了を期間終端にします。replay後は次回取得で反映し、近似や間引きをしません。ピクセルより短い区間は、tableの「正確な区間JSON」リンクで開始・終了・理由を確認できます。Grafana詳細の時刻範囲はtimelineに適用します。失効監視付きURLでは1h/24h/7d/30d（既定は24h）または日時指定（入力・表示は端末のtimezoneによらずJST、Asia/Tokyo・UTC+09:00）を選び、UTC/RFC3339に変換した同じfrom/toを`/api/report`と`/api/timeline`へ渡します。reportの未観測時間をUNKNOWNへ補完して全長100%のバーと秒数凡例を表示します。細い区間に文字は重ねずhoverで状態と開始・終了を確認でき、ピクセル未満の区間はJSONから確認できます。現在カードは`/api/status`の最新取得、補助4期間集計は各更新時刻を終端とするreportであり、過去の選択期間とは独立です。replayは選択期間・補助集計とも次回取得で反映します。

## CLIとの照合

観測中のstateを直接変更せず、`repositories/`を隔離directoryにコピーし、`state_dir`だけコピー先にした設定を使います。commit途中の不整合が出たコピーは使わず、取り直します。APIを凍結コピーの設定で起動し、表示された選択期間の開始をF、終了をTとします。現在状態の照合には別途現在時刻を用います。`status --at`は最新観測より前を拒否します。

```sh
T=2026-09-06T12:00:00Z
F=2026-09-05T12:00:00Z
MONITOR_CONFIG=/absolute/path/to/frozen/config.yaml
agent-loop-monitor status --config "$MONITOR_CONFIG" --at "$T" --json > /tmp/monitor-cli-status.json
curl --fail --get --data-urlencode "at=$T" http://127.0.0.1:19110/api/status > /tmp/monitor-http-status.json
diff -u /tmp/monitor-cli-status.json /tmp/monitor-http-status.json
agent-loop-monitor history --config "$MONITOR_CONFIG" --from "$F" --to "$T" --json > /tmp/monitor-cli-history.json
curl --fail --get --data-urlencode "from=$F" --data-urlencode "to=$T" http://127.0.0.1:19110/api/history > /tmp/monitor-http-history.json
diff -u /tmp/monitor-cli-history.json /tmp/monitor-http-history.json
agent-loop-monitor report --config "$MONITOR_CONFIG" --from "$F" --to "$T" --json > /tmp/monitor-cli-report.json
curl --fail --get --data-urlencode "from=$F" --data-urlencode "to=$T" http://127.0.0.1:19110/api/report > /tmp/monitor-http-report.json
diff -u /tmp/monitor-cli-report.json /tmp/monitor-http-report.json
```

FをTの24時間、7日、30日前に変えて比較します。Grafana側も同じscrape基準時刻の値を控えます。Prometheusの過去scrape値はreplayで書き換わらないため、過去値との厳密な照合には当時の凍結stateが必要です。

## 隔離fixtureと検証

```sh
python3 monitor/dashboard/fixture.py /tmp/monitor-fixture --case healthy
python3 monitor/dashboard/manage.py prepare --root /tmp/monitor-dashboard-fixture \
  --config /tmp/monitor-fixture/config.yaml --binary /absolute/path/to/agent-loop-monitor
```

fixtureは新規directoryだけに生成します。`mixed`は4状態timelineとDOWN/UNKNOWN、ほかに`healthy`、`down`、`idle`、`unknown`、`stale`、`missing`、`corrupt`があります。実Issue操作や`run`は不要です。生成時刻で観測を固定し、3分後にstaleになります。

本番dashboardの3サービスだけを停止し、fixture rootへ両pluginを導入して同じ`--root`で`start`します。観測monitorやsupervisorは停止しません。fixtureのplistをログイン自動起動へ登録しません。

失効監視付きURLを1280x720と1760x761で開き、現在状態・選択期間の視覚要約がスクロールなしで見えること、timeline・補助4期間と折り畳み詳細を確認します。3状態の色と欠損区間の斜線・境界、queueのIssueリンクと期限、idleのN/A、missingの観測不能表示と詳細内の観測率0、staleの表示失効、corruptのエラーを確認します。

HEALTHY表示中にfixtureのAPI、Prometheus、Grafanaをそれぞれ`launchctl bootout gui/$(id -u)/com.codex-issue-loop.monitor-dashboard.<サービス名>`で停止し、表示が失効することを確認します。API停止は最大45秒、Grafana/Prometheus通信断は次回照会とtimeoutで失効します。観測を更新せず3分待ち、通信成功中でもUNKNOWNへ変わることも確認します。各試験の間に`start`で復旧します。終了後はfixture rootで`stop`、本番rootで`start`します。

```sh
go test ./monitor/...
python3 monitor/dashboard/test_dashboard.py
node --test monitor/dashboard/test_guard.cjs
promtool check config /tmp/monitor-dashboard-fixture/prometheus.yml
make ci
```

GoテストはCLI/API一致、4状態、3期間、stale、欠損・破損、履歴再読込、read-only/loopback境界、commit途中の拒否を検証します。PythonはPromQLをpromtoolで評価し、列順の全順列・起動済み/未登録サービスのrestartも検証します。Nodeはブラウザ用スクリプトを隔離し、4状態・未観測補完・微小区間・期間切替・再取得と、通信断・古い値・scrape欠損・休止後の失効と復旧を検証します。Goは同じfrom/toのCLI reportと補完済みtimelineの秒数・coverage・需要時稼働率も照合します。実Grafanaの描画・1280x720の可読性・ログイン起動は実機で別途確認します。
