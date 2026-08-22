# Production recovery fixture runbook

## 目的と境界

`export-recovery-fixture`は、復旧判断に使われるproduction由来の証跡を、手作業で短縮・補完せず固定するためのread-only取得経路である。対象は1 repositoryの1 Issueに限定し、次を取得する。

- `state.json`内の対象Issueと、そのIssueに属するpending request record
- `events.jsonl`内で`issue_number`が対象と一致する全event
- 保存済みworktree/branchに対する1回のread-only inspection
- GitHub Issueのstate/labels/comment markerと、保存branchに対応するPR identity

他Issue、無関係event、worktree内容、worker prompt/log/result、Issue body、commentのmarker以外の本文はmanifestの`intentional_omissions`に理由付きで記録して取得しない。exportはsourceのstate/event、label/comment、PR、worktree/branchを変更しない。取得中に`state.json`または`events.jsonl`が変化した場合はfixtureを書かずfail closedになる。

## Export

まずrepository外のアクセス制限された一時directoryへ出力する。既存fileは上書きされない。

```sh
umask 077
agent-loop export-recovery-fixture \
  --repo /absolute/path/to/repository \
  --issue 442 \
  --output /private/tmp/issue-442-recovery-v1.json \
  --json

agent-loop verify-recovery-fixture \
  --fixture /private/tmp/issue-442-recovery-v1.json \
  --json
```

sanitizerは設定済みsecretと既知credential形式をredactし、absolute path、URL、SHA、run/resume/request/session等のIDを決定的な値へ置換する。同じ値は全recordで同じ値へ置換されるため、run/resume/lease/markerの参照関係は維持される。title、Issue body、question、answer、通常comment本文は決定的なopaque値へ置換する。failure commentは復旧predicateに必要なreasonをpath/secret sanitization後に保持し、failure marker digestを再計算する。

## 完全性とreplay

fixtureはraw JSON recordとして保存する。このためkey omission、明示的`null`、empty string、empty array/objectを区別する。manifestにはsource schema/version、sanitizer version、取得範囲、意図した省略、全内容のSHA-256を記録する。さらにevent件数・sequence・type、全recordのkey shape、scalar値、timestamp、参照graphを別々のdigestとして記録する。

`verify-recovery-fixture`と`recoveryfixture.Bundle.Replay`は次を満たさないfixtureを拒否する。

- eventの削除、追加、並べ替え、対象Issue外eventの混入
- omitted fieldの後世補完、`null`とemptyの正規化
- remote未publish値などをsynthetic success値へ変更
- completeness metadataまたは全体hashの不一致
- 未対応format/sanitizer、取得範囲や省略理由の欠落

`Replay`は検証済みrecordだけからlocal fake state store、event列、filesystem inspection、GitHub remote stateを返す。testはこの値をproduction recovery predicateと公式CLI testのfake境界へ直接渡す。replayは実state、GitHub、実worktreeを変更しない。

## Repository追加前のreview

production由来fixtureは、次をすべて実施してからrepositoryへ追加する。

1. 元snapshotをrepository内へ置かず、上記commandで新規exportする。fixtureをeditor、`jq`、scriptで手修正しない。
2. `verify-recovery-fixture`が成功し、manifestのIssue、source schema/version、取得範囲、event件数、first/last sequence、intentional omissionをproduction調査記録と照合する。
3. `git diff --no-index /dev/null <fixture>`等で全stringをsecurity reviewerと確認する。username/home path、repository access token、API key、private key、authorization header、顧客名、自由記述本文が残っていないことを確認する。疑わしい値があれば追加せず、sanitizerとtestを先に修正して再exportする。
4. run/resume/lease owner、request/resource park、failure/resume marker、PR URL/number/headの同一性とcardinalityが維持されていることをtestで明示する。後から判明したfieldを補完せず、新しいproduction captureとして取得する。
5. maintainer review後にfixtureのfile SHA-256を`internal/application/recoveryfixture/testdata/blessed-fixtures.sha256`へ追加する。lockだけ、またはfixtureとlockを検証なしで同時更新しない。
6. `go test ./...`、fault suite、race、vet、`make release-check`を実行する。release checkはreview済みbyte lockとfixture内部hash/completenessの両方を検証する。

## zeitreise #442

`internal/application/recoveryfixture/testdata/zeitreise-442-full-history-v1.json`は旧exact fixtureをv1形式へ移行したrelease fixtureである。27 event全件、sessionの明示的`null`、resume requestの約28ms timestamp差、6回のreconciliation、前半4回のremote key omission、後半2回の`RemoteHead=""` / `RemoteConsistent=false`、`pull_requests=null`、resume/failure marker cardinalityを一つのfixtureに保持する。移行手順は同directoryの`generate_zeitreise_442.go`に固定してあり、通常のfixture export代わりにsynthetic fixtureを作る用途には使用しない。
