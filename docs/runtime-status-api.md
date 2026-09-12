# Runtime status API v1

この文書を通信契約の正本とする。保存Snapshotのschema versionとは独立した、同一Mac・同一ユーザー用のHTTP/JSON APIである。monitorへの導入、配布保留の解除、本番配備は別作業。

## 発見と接続

repositoryの `owner/name` を小文字化したUTF-8 bytesのSHA-256を、64桁の小文字hexにする。socketは `/tmp/codex-loop-status-<effective UID>/<hash>.sock`。repo path、state directory、GitHubラベルを読む必要はない。directoryは所有ユーザーの `0700`、socketは `0600`。最長の32-bit UIDでもmacOSの104-byte `sun_path` 内にNUL終端を含めて収まる。`/tmp` はOS管理の一時directoryを前提とする。

```sh
repository=ishii1648/codex-issue-loop
socket_hash=$(printf '%s' "$repository" | shasum -a 256 | cut -d ' ' -f 1)
curl --max-time 5 --unix-socket "/tmp/codex-loop-status-$(id -u)/$socket_hash.sock" http://localhost/v1/status
```

runtimeはrepository supervisor lock取得・既存状態検証後、GitHub起動時同期より前にlistenerを開始する。schedulerとは独立したgoroutineで動く。起動・配信障害はruntime logへ記録し、API障害だけではworkerを停止しない。APIの自動再起動は行わない。

既存socketは所有UID・file typeを確認する。200msの接続probeが `ECONNREFUSED` の場合だけ、同じinodeであることを再確認して除去する。生きたlistener、symlink、通常file、他ユーザー所有物、不明な接続エラーはそのまま残しAPI起動を失敗させる。終了時は接続を閉じ、起動時に確保したsocketと同じinodeだけを除去する。private directoryは残す。同じユーザーによる意図的なfilesystem改変に対する認証境界ではない。

## GET /v1/status

成功は `200`、`Content-Type: application/json`、`Cache-Control: no-store`。公開fixtureは [status-v1.json](../internal/adapter/statusapi/testdata/status-v1.json)。保存用Snapshotを丸ごと返さない。

| フィールド | 意味 |
| --- | --- |
| `api_version` | 整数 `1`。保存schema versionではない |
| `repository` / `repo_id` | 設定されたGitHub repository / Snapshotのrepository識別子 |
| `runtime_id` | Run呼び出しごとに生成するランダム起動識別子。再起動で変わる。保存しない |
| `runtime_started_at` | 今回のAPI起動準備時刻 |
| `runtime_phase` | `starting`: 起動処理中（GitHub同期・rate limit待機を含む）。`serving`: 起動処理が完了しschedulerへ制御を渡した |
| `snapshot_revision` | 読み取った確定Snapshotの `state_revision`。repositoryの保存履歴内で比較する |
| `observed_at` | Snapshot読み取りとprojection完了後のサーバー時計 |
| `saved_supervisor` | 保存されたstate、PID、started_at、updated_at、failure_kind、retry_after |
| `saved_active_execution` | 保存された唯一の実行枠のissue_number、run_id、generation。所有者がなければ `null` |
| `issues` | issue_number昇順の配列。空は `[]`。隔離レコードも含む |

`issues[]` は `issue_number`、`run_id`、`generation`、`status`、`human_reason`、`reason_code`、`retry_after`、`updated_at` を持つ。statusは既存lifecycle語彙をそのまま返す。隔離aggregateだけは `quarantined` と表示する。run_idとgenerationを持つだけでは実行枠の所有者ではない。`resume_pending`、人間待ち、機械待ち、blocked等から実行枠を逆算してはならない。

`human_reason` はSnapshotの既存 `HumanNeeds(autoMerge)` から導出する。空文字は人間待ちの判定がないことを示し、正常進行の証拠ではない。`reason_code` はsuspensionのreason code、cancellationのsource、その他はfailure_kind。機械待ちの種類は既存status（awaiting_checks、retry_wait等）とretry_afterで表す。自由記述のエラー、質問・回答本文、worker結果、checkpoint、workspace、秘密情報は返さない。

すべての定義済みフィールドを出力する。時刻はRFC3339形式（小数秒は任意）。runtime_started_atとobserved_atはUTC、Snapshot由来の時刻は保存されたUTC offsetを保持する。未記録の非nullable時刻は `0001-01-01T00:00:00Z`、PID/generationは `0`、文字列は `""`。`retry_after: null` は保存されたretry期限がない。定義済み必須フィールドの欠落は不完全な応答として扱う。

成功応答は今回のruntimeへ接続できた証拠に限る。`saved_*` は前回起動の値を含み得る。特に `starting` 中の保存PID・状態を今回の稼働状態として表示しない。APIはworkerのprocess生存検証や実行権の再検証を行わず、保存された実行権を示すだけである。`serving` もworker進行を保証しない。revisionが停滞しても、人間待ちやqueue空等があり停止とは判定できない。時刻はwall clockであり単調時計ではない。

## 確定読み取りとエラー

要求ごとに既存 `state.lock` を作成せず開き、非ブロッキング共有lockを取得する。通常transactionとquarantine recovery transactionの不存在を確認して `ReadCanonicalSnapshot` と既存semantic validatorを使う。HTTP書き込み前にlockを解放する。イベント全履歴は参照せず、読み取りによる修復、migration、隔離、原本変更、別の保存・キャッシュは行わない。正式CLIが回答・cancel等をcommitすれば次回要求へ反映される。

エラーbodyは `{"api_version":1,"error_code":"..."}` のみ。内部pathや自由記述の例外を公開しない。

| HTTP | error_code | 意味 |
| --- | --- | --- |
| 404 | `unsupported_endpoint` | 未知のpathまたはAPI版 |
| 405 | `method_not_allowed` | GET以外。`Allow: GET` を返す |
| 503 | `state_busy` | writerがlockを保持中、または共有lock取得不能 |
| 503 | `state_unconfirmed` | 未確定transactionが存在 |
| 503 | `state_unavailable` | lockやtransaction metadataを読み取れない |
| 503 | `unsupported_snapshot_version` | 現行runtimeに非対応の保存版 |
| 503 | `invalid_state` | Snapshot欠損、破損、repository不一致、不正なlifecycle/semantic契約 |
| 503 | `state_recovery_required` | 保存されたrecovery markerが存在 |

取得不能を空queueや直前成功の継続とみなさない。後続要求で取得可能になれば業務進捗がなくても200へ戻る。API自体にretryはない。clientは接続失敗、timeout、HTTP取得不能と業務の停止理由を区別する。

serverはheader/read timeoutを2秒、write/idle timeoutを5秒、header上限を8KiBとする。終了時はgraceful drainを待たず接続を閉じる。clientにも全体timeoutを設ける。

v1では後方互換のフィールド追加を許容する。clientは未知フィールドを無視できるが、未知のapi_version、status、reasonを既知の正常状態へ置き換えない。破壊的な通信変更は別のpath/versionで提供する。保存versionの変更だけでは通信versionを変更しない。
