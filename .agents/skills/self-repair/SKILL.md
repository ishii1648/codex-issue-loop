---
name: self-repair
description: Directly repair this codex-issue-loop repository from a normal Codex session when its own agent-loop runtime cannot safely process the repair. Use only when the user explicitly invokes $self-repair and directs Codex not to delegate the change to agent-loop. Never invoke this skill implicitly from a suspected or observed failure.
---

# 自己修復

ユーザーが`$self-repair`を明示的に呼び出し、agent-loopへ委譲せず直接実装するよう
依頼した場合だけ、このrepository scopeのbreak-glass workflowを使用する。この呼び出しを、
workflowに必要なCodex goalを作成する明示的な依頼として扱う。同じ依頼の実装Issueを
作成しない。

このskillを読んだ直後に修復goalを確立する。先にrepositoryを調査したり、commandを実行したり、
他のworkflow stepへ進んだりしない。

## Goalを確立する

1. 最初のactionとして`get_goal`を呼び出す。
2. 未完了のgoalがなければ、障害、許可されたdelivery scope、必要な検証、復旧後の状態を
   objectiveに含めて`create_goal`を呼び出す。ユーザーが明示的に指定しない限り
   `token_budget`を設定しない。
3. active goalがこの修復を表していれば、新しいgoalを作成せず継続する。無関係な未完了goalが
   ある場合は置き換えず、停止してユーザーに進め方を確認する。
4. goal toolを利用できない場合、または修復goalを確立できない場合は、repositoryや外部状態を
   変更しない。

このgateが成功するまでbreak-glass eligibilityの確認へ進まない。

再開または自動継続された各turnでは`get_goal`を呼び出し、処理を始める前にGit、process、queue、
deliveryの実状態を再取得する。goalは継続のanchorであり、運用状態のevidenceではない。

## Break-glass eligibilityを確認する

1. Gitでrepository rootを解決し、すべてのpathとcommandをそのrootに限定する。
2. 処理を始める前にrepository instructions、`.agent-loop.yaml`、
   `docs/break-glass-repair.md`を読む。
3. installed CLIを利用できる場合は、runbookで指定されたread-only evidenceを収集する。
   diagnostic codeとcommand failureを正確に保持する。
4. 障害によってcodex-issue-loopが同じ修復を受け付ける、scheduleする、実行する、publishする、
   updateする、または安全にrecoverする能力が損なわれている場合だけ続行する。通常のIssue loopが
   安全に処理できるとevidenceが示す場合は停止し、break-glassの対象外であることを報告する。

failureだけから直接実装の権限を推測しない。明示的な呼び出しと、条件を満たすself-hosting failureの
両方を必須とする。

## 対象runtimeをquiesceする

`docs/break-glass-repair.md`をrepository authorityとして従う。

- delivery、supervisor、worker、worktreeの初期状態を記録する。
- `active_workers=0`になるまで待つ。修復を始めるためにactive implementation workerを終了しない。
- controllerまたはsupervisorの停止が明示的に依頼済みでない限り、停止前に正確な影響を示して
  ユーザーへ確認する。
- typed CLIによる停止を優先する。installed CLIを利用できず、exact repository IDとLaunchAgentの
  検証に成功した場合だけ`scripts/break-glass-stop.sh`を使用する。
- durable state、registry entry、active execution、continuation、session、managed worktree、
  delivery configuration、backupを手作業で編集または削除しない。

## 修復を実装する

1. 編集前にworktreeを確認する。無関係なユーザー変更を保持し、安全に分離できないdirtyまたは
   ambiguousなcheckoutでは続行しない。
2. cleanな`codex/*` branchまたは専用worktreeで作業する。
3. failureを再現し、確認した原因によって失敗するtestを追加または特定する。
4. 原因を修正する最小の一貫した変更を行う。推測に基づくrecovery、compatibility、retry、fallback、
   persistence、configuration、無関係なrefactoringを追加しない。
5. focused testを先に実行し、その後、許可されたdelivery scopeについてrunbookが要求する
   repository checkを実行する。
6. 最終diffを一度確認し、修復に不要な変更を削除する。

ユーザーがそのscopeを依頼済み、または別途許可していない限り、commit、push、Pull Requestの
作成・merge、release tagの作成、assignmentのapply、serviceのrestartを行わない。

## Goalを完了またはblockする

- goalが検証済みpatchまたはPull Requestだけを対象とする場合は、そのdeliverableと必要な検証が
  完了した時点でgoalを完了する。
- goalが運用復旧までを対象とする場合は、要求されたreleaseとassignment stepが成功し、scoped
  doctor/status/assignment verificationを通過し、controllerを意図した状態へ戻した後だけ完了する。
- すべての完了条件を満たした場合だけ`update_goal`を`complete`で呼び出す。
- 同じblocking conditionが3回以上連続するgoal turnで発生し、安全なscope内の進行が残っていない
  場合だけ`update_goal`を`blocked`で呼び出す。
- blockedまたは許可待ちの場合は現状を保持し、正確なevidence、完了済みの作業、残りのaction、
  安全な再開地点を報告する。
