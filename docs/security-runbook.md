# セキュリティ運用runbook

## 初回セットアップ

1. Mac miniには日常利用や管理者作業と分離した標準macOSユーザーを推奨する。FileVaultを有効化し、このユーザーにRemote Login、Full Disk Access、管理者権限を不要に付与しない。
2. `gh auth status`で利用中のアカウントとhostを確認する。token値は表示・記録しない。対象リポジトリに限定できるfine-grained tokenまたはGitHub Appを優先し、Metadata read、Contents read/write、Issues read/write、Pull requests read/writeだけを付与する。Administration、Actions、Packages、組織管理権限は通常不要である。
3. `codex login status`で認証済みであることだけを確認する。CodexのtokenやAPI keyを`.agent-loop.yaml`、plist、Issue、回答、shell履歴へ書かない。
   Claude Codeでは`claude auth status`、OpenCodeでは`opencode models`を使う。いずれもcredential値を出力・転記せず、runtimeの既存ユーザー認証領域を使う。
4. `.agent-loop.yaml`の`worker.sandbox`を`workspace-write`（調査専用なら`read-only`）にする。`danger-full-access`は設定検証で拒否される。workerは承認を要求できないため、worktree外への追加書き込みやnetwork権限が必要なIssueは自動キューへ入れない。
   Claude Code adapterはnative OS sandboxを有効化し、利用不能時をhard failureにし、unsandboxed escapeを拒否する。OpenCode adapterは`OPENCODE_CONFIG_CONTENT`で`external_directory`、質問待ち、`git commit`、`git push`、PR publishをdenyする。ただしOpenCodeの境界はapplication-levelでOS sandboxと同等ではないため、専用標準ユーザー、最小権限credential、branch protectionを併用する。
5. branch protectionでmainへの直接pushを禁止し、CIとレビューを必須にする。worker用資格情報にbranch protection bypass権限を与えない。

## 追加の秘密をマスクする

値ではなく環境変数名だけを設定する。

```yaml
security:
  redact_env:
    - INTERNAL_SERVICE_TOKEN
```

`INTERNAL_SERVICE_TOKEN`の値は4文字以上でなければならない。対話実行ではCLIを起動する環境へ設定する。LaunchAgentでは、値をplistへ埋め込まず、ログインセッションのlaunchd環境または組織のsecret injection機構から供給し、`agent-loop restart`後に反映を確認する。値そのものを診断出力へ表示しない。

Redactionは漏えい時の被害を減らす補助策であり、workerへ秘密を渡す許可ではない。質問への回答にcredentialらしい値または設定済みsecretを含めると`answer`は拒否する。

## 権限監査

次の確認はsecret値を表示しない。

```sh
gh auth status
codex login status
stat -f '%Sp %N' "$HOME/Library/Application Support/codex-issue-loop" \
  "$HOME/Library/Application Support/codex-issue-loop/registry.json"
find "$HOME/Library/Application Support/codex-issue-loop/repos" -type f \
  ! -perm 600 -print
find "$HOME/Library/Application Support/codex-issue-loop/repos" -type d \
  ! -perm 700 -print
stat -f '%Sp %N' "$HOME/Library/LaunchAgents"/com.codex-issue-loop.*.plist
```

期待値は、管理対象ディレクトリ0700、registry/state/event/log/plist 0600である。`~/Library/LaunchAgents`ディレクトリ自体のmodeはmacOS標準に従い、plistだけを0600にする。schema v3から移行した旧credential fileはrollback互換のため自動削除されない。[migration runbook](migration.md)に従って別途確認する。

## backupとインシデント対応

- stateとログはTime Machine等のユーザーbackup対象になり得る。backupの暗号化とアクセス制御を確認する。サポートbundleやIssueへraw log/stateを添付しない。
- M2導入前の既存state、event、recovery backup、ログは自動で遡及マスクされない。漏えいが疑われる場合はループを停止し、該当credentialを先に失効・再発行してから、保持要件に従って旧ファイルを隔離または削除する。
- tokenらしい文字列を検出したら、`agent-loop stop`、token失効、GitHub audit log/PR/Issueの確認、Codexセッションの無効化、原因修正、再登録の順で復旧する。
- backend、command path、runtime version、modelを変更した場合はloopを停止して`register`と`doctor`を再実行する。異なるbackendの保存sessionは再利用されずfresh sessionになることを`status --json`の`session.backend`と`worker_identity`で確認する。
- `govulncheck`の検出は到達可能性と影響を確認し、修正版へ更新する。例外を恒久的に黙殺せず、期限付きIssueとして記録する。

## リポジトリ外で管理する棚卸し

次はこのリポジトリへ値をコミットせず、端末台帳または組織のsecret managerで管理する。

- Mac mini名、macOSユーザー、管理者/標準ユーザー区分、FileVault、Remote Login、Full Disk Access、backup暗号化
- GitHub認証方式、token所有者、対象リポジトリ、付与permission、有効期限、最終rotation日
- Codex認証方式、利用組織、最終login確認日
- `security.redact_env`の変数名、注入元、rotation責任者（値は台帳にも平文記載しない）
- branch protection、required check、bypass可能なactor

本番導入前と認証・sandbox・外部連携を変更したときは、別担当者によるセキュリティレビューを実施する。
