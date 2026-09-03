# ADR-0004: stable Releaseとrepository別version assignment

- Status: Accepted
- Date: 2026-09-02

## Context

host全体を最新Releaseへ自動更新すると、`codex-issue-loop`自身の障害が全repositoryへ同時に伝播し、修正版を作る通常Codexセッションも同じ障害の影響を受ける。alpha、beta、LKGの名称を増やしても、実際の不具合混入率や復旧可能性は保証できない。

## Decision

配布channelはsuffixなしのannotated stable Releaseだけとし、stableとGAを同義にする。GAという別の昇格状態は持たない。各repositoryは次のexact tupleを独立して保持する。

```text
repository ID + version + commit + artifact SHA-256
+ immutable slot + generation + previous assignment
```

stable公開はassignmentを変更しない。operatorは`preview`で検証済みartifactと現在generationを取得し、そのgenerationを指定して対象repositoryだけへ`apply`する。切替はrepository fence、drain、immutable slot検証、exact LaunchAgent切替、scoped doctorの順で行う。失敗時は対象だけをpreviousへ戻し、rollbackにも失敗した場合はrepository fenceを保持する。

v2 configへのmigrationは全登録repositoryを現在installのexact tupleでgeneration 1として初期化する。migration自体はrepository plist、PID、stateを変更しない。assignment protocolを持たない旧runtimeからの初回切替では、対象stateのmutation lockでadmissionを凍結し、workerが0であることを同じlock内で再確認してからexact LaunchAgentだけをbootoutする。global maintenance fenceは使用しない。

最初の新version適用時にhostのdelivery controllerだけを検証済みimmutable slotへ移す。repository runtimeは引き続きassignmentが正本であり、global install pathへ戻さない。

## Consequences

- repositoryごとの段階展開と対象限定rollbackが可能になる。
- stable以外のLKG、GAなど意味が重なる版名・昇格状態を運用しない。
- rollout failureは対象repositoryのassignmentをrollbackするが、stable Releaseの状態は変更しない。
- downgradeは任意version指定ではなく、記録済みpreviousへのtyped rollbackだけを許可する。
- config、slot、tag、commit、digest、attestation、generationの不一致はfail closedになる。
- schemaやsemantic contractがrepository間で共有stateを非互換にする変更は、従来どおり別の全停止migrationを必要とする。
