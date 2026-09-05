# Repository instructions

## Durable Issue lifecycle

- 状態語彙、遷移判定、不変条件は `internal/domain/issue` に置き、durable snapshotへのstatus commitは `internal/adapter/state/issue_transition.go` の境界を通す。`internal/application/app` と `internal/application/supervisor` に遷移の正当性判断や生のstatus代入を新設せず、domainからapplication、adapter、platformへの依存を導入しない。
- 新しい遷移は、domainの語彙・decision・不変条件、永続commit境界、呼び出し側の順に実装する。この順序または境界に例外を設ける場合は、実装前にADRで理由と代替する不変条件を記録する。
- 詳細な責務境界は [`docs/architecture.md` §3.1](docs/architecture.md#31-durable-issue-lifecycle境界)、公開状態の互換性は [`docs/architecture.md` §10](docs/architecture.md#10-issue-lifecycle-apiと互換性)を正本とし、このファイルへ詳細を重複させない。

## Comments

- コメントには、コードだけでは十分に表現できない設計意図、制約、不変条件、または呼び出し側が依存するAPI契約だけを書く。
- コードを読めば分かる処理内容や、名前を自然言語に言い換えただけのコメントは書かない。コードとコメントで同じ振る舞いを二重管理しない。
- APIの振る舞いは、副作用、エラー条件、並行実行上の保証、所有権、呼び出し順序、互換性など、呼び出し側が実装を読まずに依存する必要がある契約に限って記述する。
- コードを変更するときは周辺コメントも確認し、実装の説明になったコメントや古くなったコメントは削除または意図・契約の説明へ修正する。
- build tag、`go:embed`などのtool directive、license表記、外部toolが必須とするコメントはこの制限の対象外とする。

## Pull requests

- Pull request の説明本文は日本語で記述する。
- Pull request の作成・更新時は `.github/PULL_REQUEST_TEMPLATE.md` を参照し、説明本文をその形式に合わせること。
- 該当する内容がないセクションは削除せず、「なし」と明記する。
- コード、コマンド、ファイル名、API 名などは、正確さを優先して原文のまま記述してよい。
