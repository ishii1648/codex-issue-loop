# Historical incident taxonomy v1

このdirectoryは、`codex-issue-loop`の過去運用データとGitHub履歴から作成した、sanitized incident corpus、排他的なprimary classification、決定的なoffline evaluator、観測不足の記録を保持する。runtimeはこの`rules.json`を正本としてsignal収集、episode生成、scheduled AI、重複防止付きGitHub Issue生成へ利用する。外部Prometheus/OpenTelemetry exporterは含まない。

## 再実行

repository rootで次の1 commandを実行する。

```bash
go run ./cmd/incident-eval
```

commandは次を順番に行い、保存済みgoldenとbyte単位で一致しなければ失敗する。

1. `corpus.json`と`rules.json`をunknown field拒否でdecodeする。
2. corpus、classification/error-state定義、rule priority、unknownのmissing evidenceを検証する。
3. `source-inventory.json`、`coverage.json`、`observability-gaps.json`を検証する。
4. `schemas/incident-*.schema.json`のdialectと構造を確認する。
5. 全incidentへpriority順にrulesを適用する。
6. `evaluation.golden.json`とcanonical JSONを比較する。

回帰testは次で実行する。

```bash
go test ./internal/application/incidentanalysis
```

testは同一入力の2回評価が一致すること、priority conflictを拒否すること、unknownに不足証拠があること、raw path/credential markerを拒否することも確認する。

## 成果物

| file | role |
| --- | --- |
| `source-inventory.json` | 取得時刻、期間、件数、query、欠落、retention/権限制約 |
| `coverage.json` | 取得時点の全34 `bug` Issueとmerged fix ground truthの対応 |
| `corpus.json` | 16件の層化sanitized incident corpus |
| `rules.json` | 6分類、9 error states、priority付きmachine-readable rules |
| `evaluation.golden.json` | 再現可能な保存済み評価結果 |
| `observability-gaps.json` | incident/invariantへtraceした追加signal候補 |
| `schemas/incident-*.schema.json` | corpus、rules、inventory、coverage、evaluation、observabilityのschema |
| `cmd/incident-eval` | networkへ接続しないoffline evaluator |

raw `state.json`、raw `events.jsonl`、supervisor/launchd log、worker transcriptは保存していない。local sourceはevent sequence、timestamp、typed field、件数、hash化可能な参照だけへ縮約した。GitHub参照は公開Issue/PR/merge commitに限定した。

## Source inventoryの要点

取得時刻は`2026-09-02T01:29:50Z`。詳細なqueryと制約は`source-inventory.json`を正本とする。

| source | period | count | principal gap |
| --- | --- | ---: | --- |
| active events | 2026-08-18T14:38:30Z–2026-09-01T19:55:20Z | 1,048 events | archive checkpoint 1件と重複 |
| gzip events | 2026-08-16T10:15:07Z–2026-08-18T14:33:16Z | 44,023 events | これ以前はretention外 |
| state snapshot | revision 45,070 | 53 Issues | snapshot単体に開始時刻なし |
| supervisor archive | 2026-08-16T13:20:15Z–2026-08-18T11:02:34Z | 27,990 lines | event全期間を覆わない |
| launchd stderr | timestampなし | 58 lines | incident windowへjoin不能 |
| worker runs | 2026-08-16T10:25:24Z–2026-08-19T17:39:23Z | 60 runs | active events後半を覆わない |
| GitHub Issues | 2026-08-15T11:13:12Z–2026-09-01T05:25:56Z | 90 | closure reasonが一貫しない |
| Pull Requests | 2026-08-15T09:47:28Z–2026-09-02T01:25:20Z | 123 | 過去check rerunの欠落可能性 |
| PR Reviews | 同上 | 0 | review commentは別集計 |
| git history | 2026-08-15T08:24:22Z–2026-09-02T01:29:23Z | 291 commits | local refsから到達可能な範囲 |

GitHub母集団では90 Issues中34件に`bug` labelがあった。32件はmerged PRと40桁merge commitへ対応し、#168と#171はclosing fix/supersedeを確認できないためunknownとした。`bug` labelがない#102は、local eventで隣接retry 27,603組中26,999組が先行`retry_at`より早いtimestamp候補になり、原因を説明するIssueとmerged PR #120もあるためconfirmed ground truthへ追加した。途中の`supervisor_recovered`が実API成功かを示すattempt IDはないので、26,999組すべてを確定deadline違反とは数えない。

corpusは全34件を複製せず、failure familyとsignal sourceを層化した16件を詳細recordにした。非抽出のcorroborated Issueも`coverage.json`でPR/commitまでreadbackできるため、選択漏れとground-truth不足を区別できる。

## 分類規則

runtimeの`failure_kind`（`transient` / `issue` / `supervisor`）と、製品incidentのprimary classificationは別の軸である。`failure_kind=transient`でも#102のように製品bugになり得る。

優先順位は次のとおりで、最初に一致した1分類だけをprimaryにする。後続一致はsecondary evidenceとして結果へ残す。

1. `confirmed_bug`: documented invariant violationとcorroborated merged product fix
2. `suspected_bug`: invariant violationが独立runで再現し、merged fixは未確認
3. `operator_attention`: sticky stateを解除するhuman actionが必要
4. `degraded`: request amplification、persistence threshold超過、progress停止
5. `expected_transient`: typed transient、deadline遵守、4回以下、successful domain event、増幅/停止なし
6. `unknown`: 上記の必須証拠が不足

`blocked`、`failed`、retry回数、error文字列、`bug` labelのいずれか単独では`confirmed_bug`にならない。priorityは全ruleで一意であり、同priorityを追加するとvalidationが失敗する。

各分類とerror stateのentry/exit、persistence threshold、severity、required evidence、exclusion、retry、operator action、AI handoff、Issue condition、fingerprint、代表incident、現在signalでの決定可能性は`rules.json`にある。

## Offline evaluation

保存済み結果は次のとおり。

| primary classification | count |
| --- | ---: |
| `confirmed_bug` | 9 |
| `expected_transient` | 1 |
| `degraded` | 1 |
| `operator_attention` | 2 |
| `suspected_bug` | 0 |
| `unknown` | 3 |
| total | 16 |

- classified: 13
- unknown: 3
- rule conflict: 0
- ground-truth mismatch: 0
- confirmed bug: TP 9、FP 0、FN 0、ground-truth不足で評価対象外3
- expected transient: TP 1、FP 0、FN 0、ground-truth不足で評価対象外3

これは16件の層化corpus内の再現結果であり、将来データへの精度を主張しない。特にexpected transientはpositive sampleが1件だけでvarianceが大きい。#168、#171、PR #98は、closure/fix provenanceまたはCI detailが不足するためunknownを維持した。

## 観測不足

`observability-gaps.json`の各候補はincident IDまたは明示したinvariantへtraceされる。優先度が高いものは次のとおり。

- scheduler cycle ID、wake trigger、scheduled deadline: #102のdeadline bypassと#90のlate retryを区別する。
- external API attempt/result: `supervisor_recovered`が実成功を伴うか立証する。
- progress marker: idle、timer starvation、blocked blast radiusを区別する。
- retry episode ID: retry/recoveredを同一operationへjoinする。
- normalized failure code: raw error文字列なしでfingerprintとrunbookを安定化する。
- build/schema identity: known buggy version windowと再発を区別する。
- monotonic duration: wall clock変更に依存しないpersistence判定を行う。
- timestamped/redacted host log: launchd logをevent sequenceへjoinする。
- retention manifest: 証拠の欠落と事象の不存在を区別する。

各signalにはsecurity、metric label cardinality、retention制約を記録している。cycle ID、episode ID、commit SHA、path、Issue番号など高cardinality/機微情報はmetric labelにしない。

## Runtime実装

signal contract、restart/rotation安全なepisode builder、scheduled read-only AI分析、fingerprint dedup、dry-run/live Issue producer、bounded JSON metricsを依存順に実装した。運用設定、保存先、権限、停止・復旧方法は[`docs/incident-automation.md`](../../docs/incident-automation.md)を正本とする。外部metrics exporterは将来拡張であり、episode payloadや高cardinality IDをmetric labelとして公開してはならない。

## 未解決事項

- #168、#171のclosure rationaleとsupersede/fix参照
- PR #98の`Quality gates` failure detailと最終supersede判断
- launchd logの時系列join不能
- worker run retentionがactive event後半を覆わない理由
- expected transient ground truthが1件しかなく、複数release/versionのnegative controlが不足
- historical `supervisor_recovered`が実API成功かを直接示すattempt IDがない

不足情報を推測で補わず、corpusの`missing_evidence`と`would_resolve_with`へ保存している。
