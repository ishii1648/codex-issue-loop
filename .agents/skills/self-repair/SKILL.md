---
name: self-repair
description: Directly repair this codex-issue-loop repository from a normal Codex session when its own agent-loop runtime cannot safely process the repair. Use only when the user explicitly invokes $self-repair and directs Codex not to delegate the change to agent-loop. Never invoke this skill implicitly from a suspected or observed failure.
---

# 自己修復

ユーザーが`$self-repair`を明示し、agent-loopへ委譲せず直接実装するよう依頼した場合だけ使用する。
この呼び出しをCodex goal作成の明示的な依頼として扱い、同じ依頼の実装Issueは作成しない。

## Goal

repositoryの調査やcommand実行より先にgoalを確立する。

1. 最初に`get_goal`を呼び出す。
2. 未完了goalがなければ、障害、許可された作業範囲、検証、復旧後の状態を含むgoalを
   `create_goal`で作成する。明示されない限り`token_budget`は設定しない。
3. 同じ修復のgoalがあれば継続する。無関係な未完了goalは置き換えず、ユーザーに確認する。
4. goalを確立できなければ何も変更しない。

継続turnではgoalを確認し、Git、process、queue、deliveryの実状態を再取得してから再開する。

## 適用条件と停止

1. Gitでrepository rootを確定し、repository instructions、`.agent-loop.yaml`、
   `docs/break-glass-repair.md`を読む。
2. runbookのread-only evidenceを収集する。通常のIssue loopで安全に修復できる場合は停止する。
3. self-hosting障害によって同じ修復をloopで処理できない場合だけbreak-glassを続行する。
4. runtimeを停止する場合はrunbookに従い、`active_workers=0`を確認する。停止の影響が未承認なら
   先にユーザーへ確認する。

typed CLIを優先し、利用できない場合だけ検証済みの`scripts/break-glass-stop.sh`を使用する。
durable state、registry、execution、continuation、session、managed worktree、backupを手編集・削除しない。

## 直接実装

1. 無関係な変更を保持し、cleanな`codex/*` branchまたは専用worktreeで作業する。
2. 障害を再現し、原因を示すtestを追加または特定する。
3. 原因に対する最小の一貫した修正を行う。
4. focused testとrunbookが要求するcheckを実行する。
5. 最終diffを一度確認し、不要な変更を削除する。

依頼または別途許可されていないcommit、push、Pull Request、merge、release、assignment、restartは
行わない。

## Goalの終了

- patchまたはPull Requestが目的なら、deliverableと検証が完了した場合だけ`complete`にする。
- 運用復旧が目的なら、要求されたreleaseとassignmentを終え、scoped verificationを通し、
  controllerを意図した状態へ戻した場合だけ`complete`にする。
- 同じ阻害条件が3回以上連続し、安全に進められない場合だけ`blocked`にする。
- 許可待ちやblockedでは状態を保持し、evidence、完了済み作業、残作業、安全な再開地点を報告する。
