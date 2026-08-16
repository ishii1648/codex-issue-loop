# 脅威モデル

## 目的と前提

`agent-loop`は、GitHub Issueを入力として、ログイン中のmacOSユーザー権限で`gh`、`git`、`codex`を長時間実行する。保護対象は、GitHub/Codexの認証情報、対象リポジトリ、Issueごとのworktree、永続状態・イベント・ログ、およびMac mini上の同一ユーザーのデータである。

次を信頼境界とする。

- **信頼する**: Mac miniのOSとログインユーザー、インストール済みCLIの実体、レビュー済みの`.agent-loop.yaml`と`AGENTS.md`、`agent-loop`本体
- **条件付きで信頼する**: GitHub/Codexサービス、対象リポジトリの履歴、依存モジュール
- **信頼しない**: Issueのタイトル・本文・コメント、workerのstdout/stderrと構造化結果、外部コマンドのエラー出力、質問への回答、Issueから導出する名前

管理者権限を奪取済みのプロセス、同一macOSユーザーとして動く悪意あるプロセス、GitHub/Codex自体の侵害は境界外とする。同一ユーザーの侵害に対してファイルモードだけで秘密を守ることはできない。

## データフローと制御

1. `gh`がIssueを取得する。タイトル512 bytes、本文64 KiB、最新20コメント・各8 KiBに制限し、NULを含む制御文字を除去する。
2. IssueはJSON化して明示的なuntrusted data境界内へ置く。Issue内の命令、権限拡張、資格情報要求、prompt境界の上書きを実行しないようworkerの優先順位を固定する。
3. worktree名はIssue番号と限定文字のslugから生成する。リポジトリID、Issue番号、Git ref、絶対worktree rootを検証し、root外への逸脱とworktreeパスのsymbolic linkを拒否する。
4. Codexは`workspace-write`または`read-only` sandbox、`approval_policy=never`相当で実行する。`danger-full-access`と承認による実行範囲拡張は設定で拒否する。
5. stdout/stderr、worker result、state snapshot、transaction、event payload、GitHubへ投稿する質問・失敗理由は、既知token形式と`security.redact_env`で指定した環境変数の値をマスクする。
6. stateディレクトリとrunディレクトリは0700、plist・registry・state・event・transaction・ログは0600とする。壊れた永続状態のquarantineも同じprivate tree内に保持する。

## 主な脅威

| 脅威 | 代表例 | 制御 | 残余リスク |
|---|---|---|---|
| Prompt injection | Issue本文が「以前の指示を無視」「tokenを投稿」と要求 | untrusted JSON境界、固定優先順位、権限拡張禁止、入力上限 | モデルがデータを誤解する可能性。外部変更はPRレビューで防御する |
| リソース枯渇 | 巨大本文、大量コメント、巨大回答 | 入力件数・bytes上限、回答16 KiB上限、worker timeout | `gh`がJSONを返すまでの一時メモリはCLI実装に依存する |
| 引数・path injection | `../`、悪意あるref、symlinkによるroot逸脱 | shell不使用、argv分離、ref検証、canonical path、root内判定、symlink拒否 | 検査後のTOCTOU。0700と専用macOSユーザーで他プロセスを制限する |
| 資格情報漏えい | tokenがログ、state、Issueコメントに混入 | 多層redaction、秘密を回答として拒否、0600/0700 | 未登録形式や4文字未満の値、redaction前に外部CLI自身が送るデータ |
| 過大権限 | GitHub tokenやmacOSユーザーが管理者 | 最小権限runbook、sandbox固定、承認なし | Codexはworktree内のソースを変更・pushできる。branch protectionとレビューが必要 |
| 永続データ漏えい | Time Machineやサポートbundleがログを収集 | private mode、秘密を保存前にredact、backup手順 | 既存のM2以前のstate/logには遡及redactionしない |
| 依存関係の脆弱性 | 到達可能な脆弱関数 | CIの`govulncheck`、`go.sum`、Dependabot等の更新PR | 未公開脆弱性と静的解析で到達性を判定できない呼び出し |

## セキュリティ変更時のレビュー項目

- 新しい外部入力がprompt・path・argv・永続状態・GitHub出力へ直接流れていないか
- 新しい文字列フィールドが`redact.Marshal`または境界redactorを通るか
- 新しいファイルがprivate modeで作られ、backupにも秘密が複製されないか
- subprocessはshell文字列ではなくargvで起動しているか
- sandbox、GitHub権限、macOS権限を拡大していないか
- 高リスクの負テストと`make vuln-check`が通るか
