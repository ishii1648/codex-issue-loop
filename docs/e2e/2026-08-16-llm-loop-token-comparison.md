# LLM内ループと外部supervisorのtoken消費比較（2026-08-16）

## 1. 目的

`zeitreise` repositoryで実運用した次の2方式について、LLMが処理したtokenを
比較する。

1. **LLM内ループ**: 1つのClaude Code sessionが`agent-loop` Skillを読み、
   Issue選択、subagent起動、GitHub操作、次Issueへの遷移を会話履歴の中で繰り返す。
2. **外部supervisor**: `codex-issue-loop`のGo supervisorがIssue選択と状態遷移を
   所有し、Issueごとに独立した`codex exec` workerを起動する。

確認したいのは、Issue境界をLLMの会話履歴から外へ出すことで、前のIssueの
contextを次のIssueの全model callで再処理する増幅をどの程度抑えられたかである。
API料金やsubscription利用枠の比較ではない。

## 2. 結論

同じ9件完了のcohortでは、記録から追跡できるagent全体の処理tokenは
**101.041Mから16.568Mへ83.6%減少し、6.10分の1**になった。

| 比較範囲 | LLM内ループ | 外部supervisor | 削減率 | 比率 |
| --- | ---: | ---: | ---: | ---: |
| loop main transcriptとIssue worker | 60.081M | 16.568M | 72.4% | 3.63分の1 |
| subagentを含む記録上のend-to-end | 101.041M | 16.568M | 83.6% | 6.10分の1 |
| end-to-endの非cached相当 | 2.877M | 1.109M | 61.5% | 2.59分の1 |

外部supervisor側の最初の10件は17.323Mであり、移行前の調査が予測した約17Mと
1.9%の差だった。Issue境界でcontextを破棄する効果は、事前試算どおり確認できた。

ただし、backend、model、task内容が同一のrandomized A/B testではない。
この結果は実運用上の強い観測証拠だが、83.6%の全差分をsupervisorだけの因果効果と
断定しない。

## 3. 比較対象

### 3.1 LLM内ループ

| 項目 | 値 |
| --- | --- |
| 実行日 | 2026-08-11 |
| backend | Claude Code 2.1.227 |
| model / effort | `claude-fable-5` / `high` |
| main session | `fa9787eb-89b5-434d-867e-734ab30ae878` |
| 完了数 | 9 Issues相当（merged PR #227、#233、#234、#235、#239、#240、#241、#242、#243） |
| main model request | 228 |
| 完了までに開始したsubagent log | 15 files、512 model requests |

旧調査ではmain sessionについて、1 taskあたり25.3 request、taskごとにcontextへ残る
増分を40.9K tokensと実測した。contextはtask境界で破棄されず、後続taskの各requestへ
引き継がれていた。調査本文は`zeitreise`の
[agent-loopのcontext境界方式調査](https://github.com/ishii1648/zeitreise/blob/3f594bd32ffd20f4e460879e7a6ceb7720285433/docs/research/2026-08-11-agent-loop-context-boundary.md)
にcommitされている。

### 3.2 外部supervisor

| 項目 | 値 |
| --- | --- |
| 実行日 | 2026-08-16 |
| supervisor | `codex-issue-loop` schema v2 |
| backend | Codex CLI 0.147.0、`codex exec` |
| model / effort | `gpt-5.6-sol` / `low` |
| 完了数 | 最初の9 Issue worker（#362、#341、#353、#354、#355、#356、#368、#403、#404） |
| model request | 232 |
| subagent call | 0 |

設定は2026-08-16 02:57 JSTに追加され、Issue admission labelの移行は03:24、
最初のworkerは03:25:56に開始した。各workerの`cwd`は
`codex-issue-loop/worktrees/.../issue-<N>`で、session IDもIssueごとに分かれている。
切替のrepository上の証跡は`zeitreise`の
[設定追加 #408](https://github.com/ishii1648/zeitreise/commit/348ed6028d800767ff507259ff37edea2a35b206)と
[label運用移行 #409](https://github.com/ishii1648/zeitreise/commit/2984f0da5f47db3f247b5a2d562efcbc8f1299ef)である。

model requestは旧228回、新232回で1.8%しか違わない。このcohortの72.4%減は、
単にworkerのmodel callが少なかったことでは説明できない。

## 4. 計測定義

### 4.1 処理token

providerをまたいで、各model requestが報告した次のtokenを単純合算した。

- Claude Code:
  `input_tokens + cache_creation_input_tokens + cache_read_input_tokens + output_tokens`
- Codex:
  `total_token_usage.total_tokens`。これは`input_tokens + output_tokens`であり、
  `input_tokens`には`cached_input_tokens`を含む。`reasoning_output_tokens`は
  `output_tokens`の内数なので重ねて加算しない。

同じ`requestId`が複数のcontent blockへ分かれて記録される場合は最初の1件だけを
数えた。Codexは各sessionの最後の累積`total_token_usage`を使った。

### 4.2 非cached相当

料金換算ではなく、cache readを除いた観測量として次を併記する。

- Claude Code: `input_tokens + cache_creation_input_tokens + output_tokens`
- Codex: `input_tokens - cached_input_tokens + output_tokens`

providerごとのcache価格、subscriptionのrate limit、model別weightは適用していない。

## 5. 同数cohortの結果

### 5.1 LLM内ループ

| 内訳 | model request | 処理token | 非cached相当 |
| --- | ---: | ---: | ---: |
| main sessionの最初の228 request | 228 | 60,081,033 | 568,452 |
| 完了時点までの15 subagent | 512 | 40,959,871 | 2,308,477 |
| 合計 | 740 | 101,040,904 | 2,876,929 |

main sessionだけを比較すると新方式は72.4%減である。旧方式は実装をsubagentへ
委譲していたため、end-to-endではsubagent logも加える必要がある。9件完了時点より
後に開始したIssue #232の調査subagentや次iterationのsubagentは含めていない。

### 5.2 外部supervisor

| Issue | model request | 処理token |
| ---: | ---: | ---: |
| #362 | 21 | 1,369,189 |
| #341 | 47 | 5,103,340 |
| #353 | 18 | 1,034,263 |
| #354 | 17 | 955,698 |
| #355 | 21 | 1,013,960 |
| #356 | 24 | 1,409,321 |
| #368 | 20 | 1,185,971 |
| #403 | 25 | 1,667,704 |
| #404 | 39 | 2,828,462 |
| 合計 | 232 | 16,567,908 |

Codex側の内訳は`input_tokens` 16,488,912、`cached_input_tokens` 15,459,072、
`output_tokens` 78,996で、非cached相当は1,108,836だった。

## 6. 移行後全期間の観測

2026-08-16 22:46 JST時点で、`zeitreise`では15のmerged Issueに対応する
19個の完了worker sessionが記録されていた。再試行も含む合計は次のとおりである。

| 指標 | 値 |
| --- | ---: |
| unique merged Issues | 15 |
| 完了worker sessions | 19 |
| 処理token | 84,635,641 |
| 1 Issueあたり | 5,642,376 |
| 非cached相当 | 3,173,241 |
| cached input比率 | 96.59% |

#369、#407、#423だけで51,833,836 tokens、全体の61.2%を占めた。これらは
初回だけでも87、107、132 model requestsを要している。外部supervisorは
**Issue間**の二次増幅を除去するが、1 Issue内で長大になったsessionの増幅は
除去しない。大型Issueの分割、continuation上限、Issue内checkpointは別の改善軸である。

集計時に進行中だった#427はterminal eventがなかったため除外した。

## 7. 解釈と限界

### 確認できたこと

- model request数が同程度の最初の9件で、main/workerの処理tokenは72.4%減った。
- 旧方式のsubagentまで含めると、記録上のend-to-end処理tokenは83.6%減った。
- 事前予測の10件約17Mに対して実測17.323Mで、context境界のモデルは再現した。
- Issueごとにsessionが分かれ、前Issueのcontextが後続workerへ単調に蓄積しない。

### この計測だけでは断定できないこと

- API料金、Codex subscription利用枠、rate limitが同じ割合で減ったか。
- model差（`claude-fable-5/high`対`gpt-5.6-sol/low`）を除いた純粋な方式差。
- task難易度とtool出力サイズを揃えた場合の削減率。
- provider内部のcache weightや、待機中processの課金有無。

したがって、このreportの主張は「実運用の観測tokenでは大幅に減り、Issue境界を
外へ出す事前モデルと一致した」である。「任意のmodel・料金体系で83.6%安い」ではない。

## 8. 再現方法

source transcriptはlocalの認証済みagent実行履歴であり、repositoryへcommitしない。
再集計時は秘密値やIssue本文を出力せず、usage fieldだけを読む。

旧main sessionは、transcript順に重複`requestId`を除いた最初の228 requestを
集計する。subagentはmain session配下の`subagents/*.jsonl`から、9件目の完了時刻
`2026-08-11T10:29:07.726Z`より前に開始し、同時刻までに完了した15 fileを集計する。

新方式は`~/.codex/sessions/2026/08/16/*.jsonl`から次を満たすsessionを抽出する。

- `session_meta.payload.originator == "codex_exec"`
- `cwd`が`codex-issue-loop/worktrees/zeitreise-*/issue-*`
- `task_complete` eventが存在する
- 最初のunique 9 Issuesについて、最初の完了sessionだけを採用する

各Codex sessionでは最後の`token_count.info.total_token_usage`と、
`last_token_usage`を持つ`token_count` event数を取得する。集計結果は上記のIssue別表と
一致することを確認する。

## 9. 再確認条件

次のいずれかを変更した場合は、新しいcohortを別日付のreportとして追加する。

- worker backend、model、reasoning effort、base prompt
- `session_mode`、`max_continuations`、Issue内checkpoint方式
- workerによるsubagent利用方針
- token usage eventのschemaまたはcache fieldの意味

同一Issueを両方式で実行できるfixture repositoryを用意できた場合は、本reportより
優先度の高いcontrolled benchmarkとして扱う。
