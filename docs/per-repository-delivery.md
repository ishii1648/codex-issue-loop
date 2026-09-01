# Repository別stable delivery

## 不変条件

- 配布対象はstable Releaseだけであり、alpha、beta、LKG channelはない。
- stable Releaseの公開だけではassignmentを変更しない。
- assignmentの正本はowner-onlyのdelivery configであり、repository ID、version、commit、artifact SHA-256、immutable slot、generation、previousを保持する。
- applyとrollbackは対象repository以外のplist、PID、assignment、durable stateを変更しない。
- active workerは停止しない。drain期限では変更をdeferしてfenceを解除する。
- rollback失敗時は対象repositoryのfenceとtransactionを保持する。

## v1からv2への明示migration

検証済みv0.9以降のbinaryでpreviewし、全repositoryが現在installのexact tupleになることを確認してから適用する。

```sh
operator_binary=/absolute/path/to/verified/agent-loop_Darwin_arm64
"$operator_binary" delivery assignment migrate --json
"$operator_binary" delivery assignment migrate --apply --json
"$operator_binary" delivery assignment status --json
```

previewはfileを変更しない。applyは現在binaryをimmutable slotへcopyしconfigをv2へ更新するが、repository plist、LaunchAgent、PID、snapshotは変更しない。migrationから全repositoryの切替完了までは、global install pathが旧runtimeの起動元として残るため上書きしない。assignment操作には検証済みrelease artifactを`operator_binary`として直接使う。migration前にdelivery LaunchAgentを停止しておけば、旧controllerがv2 configを読む一時的なerrorを避けられる。

## Repository単位の適用

`--version`は既存のexact stable tagを指定する。previewが返したgenerationをapplyへ渡す。

```sh
"$operator_binary" delivery assignment preview \
  --repo /absolute/path/to/repository --version v0.9.0 --json

"$operator_binary" delivery assignment apply \
  --repo /absolute/path/to/repository --version v0.9.0 \
  --expected-generation 1 --json

"$operator_binary" delivery assignment verify \
  --repo /absolute/path/to/repository --json
```

artifactはchecksum、annotated tagのpeeled commit、release manifest、binary metadata、GitHub attestationを照合してからslotへstageする。tagが検証中に移動した場合、またはpreview後にgenerationが変わった場合は適用しない。

## Rollback

rollback先はconfigに記録されたpreviousだけである。

```sh
"$operator_binary" delivery assignment rollback \
  --repo /absolute/path/to/repository \
  --expected-generation 2 --json
```

rollbackもgenerationを1増やす。成功後のpreviousはrollback前versionになるため、同じartifactの再適用を新しいpreviewから行える。

## Evidence

段階展開では対象外repositoryについて、切替前後の次を保存する。

- assignment tupleとgeneration
- LaunchAgent PIDとprogram binary SHA-256
- state revision
- Issue、lease、pending request、managed worktreeの要約

対象repositoryは`assignment verify`、`doctor --repo ... --assignment-health --json`、`status --repo ... --json`を保存する。最終状態は全repositoryでworker limit 1、pending assignment transactionなし、repository fenceなしを必須とする。
