# ADR-0007: 外形監視とDOWNの詳細分析のためblockedラベルを維持する

- Status: Accepted
- Date: 2026-09-07
- Decision owners: codex-issue-loop maintainers

## Context

`needs-human` は、人の回答・復旧操作・レビュー・手動マージなどが必要であることの共通表示である。`blocked` は、Issueの処理が阻害されて停止している状態を示す。人の復旧が必要な停止では両方を併記できる。この区別は [HumanWait.Reason](../../internal/domain/issue/attention.go) と [GitHub projection](../../internal/adapter/github/projection.go) に対応する。

GitHubの表示を簡素化するため、`blocked` ラベルを廃止し、人の対応要否を `needs-human`、停止理由や必要操作をコメントで表す案を検討した。この案でも内部状態の `blocked` は維持する想定だった。

現行monitorはGitHubのopen Issue、ラベル、Issue event時刻から外形監視を行う。[要件](../../monitor/docs/requirements.md)では `running -> done|needs-human|failed|blocked` を処理進捗として扱い、[既定のterminalラベル](../../monitor/internal/config/config.go)は `codex-loop:done`、`needs-human`、`codex-loop:failed`、`blocked` である。ここでのterminalはmonitorの処理進捗イベントであり、Issueの作業完了を意味しない。[現行metrics](../../monitor/internal/app/http.go)は監視状態・時刻・継続時間・需要時稼働率・観測カバレッジなどを提供し、blocked専用の件数・時間・率は提供していない。

## Decision

- GitHubの `blocked` ラベルを維持する。通常の回答・レビュー待ちと、処理を阻害する停止を機械可読な形で区別し、GitHubだけから行う外形監視でDOWNの前後に生じた停止を詳細分析する手掛かりを残す。
- コメントは具体的な理由と必要な操作を説明し、ラベルは分類と集計の手掛かりを提供する。自由文コメントの解析に依存せず、ラベルとイベント時刻から停止の発生・滞留・解消を分析できる情報を保持する。
- [`.agent-loop.yaml`](../../.agent-loop.yaml) の `github.exclude_labels` にある `blocked` は、新規受付を防ぐ用途も維持する。
- この決定は既存ラベルの維持に限定する。runtime、ラベル同期・受付方針、monitorの判定、metrics、設定、永続状態を変更しない。

## Consequences

### Positive

- 人の対応待ちという共通表示を保ちながら、停止をラベルで抽出・分類できる。
- DOWN前後の停止を調べる際に、自由文の表現差に左右されない時系列分析の手掛かりを保持できる。
- ブロック発生率、滞留時間、未解消件数などを将来集計する余地を残す。これらは集計候補であり、今回実装する指標でも確定済みの指標定義でもない。

### Negative

- `needs-human` と `blocked` の意味を区別し、併記を含む既存のラベル管理を続ける必要がある。
- `blocked` はmonitorの `DOWN` と同義ではない。[ADR-0005](0005-single-execution-boundary.md)の実行枠解放により、Issueがblockedでも後続Issueの処理は継続できる。ラベルと時刻の相関だけでDOWNの原因を断定しない。
- 手動の除外用途やラベル同期の遅延もあるため、ラベルの全付与をそのままruntime障害件数とみなせず、イベント時刻も内部停止・復旧時刻と一致するとは限らない。分析ではコメントなどの具体的な理由と照合する必要がある。

## Alternatives considered

- **GitHubのblockedラベルを廃止する**: 内部状態の `blocked` は残し、`needs-human` とコメントに集約すれば表示は簡素になる。一方、通常の回答・レビュー待ちと停止の区別を自由文コメントの解析に委ねることになり、GitHubだけからの分類・集計やDOWN前後の詳細分析の手掛かりが失われるため採用しない。
