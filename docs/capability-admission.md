# Issue capability admission契約

この文書は、Issueが要求する実行能力とworker profileが提供する能力を、claimより前に照合する正本仕様である。resource claimは編集範囲の排他制御だけを表し、`repo:*`を含め、network、browser、download、外部時刻前提の許可にはならない。

## Version 1 contract

ready Issueは本文に、次のblockをちょうど1つ持たなければならない。

```yaml
<!-- agent-loop:capabilities
version: 1
profile: extended
network: localhost
browser_cdp: true
download: true
external_time_gate: false
-->
```

fieldはすべて必須である。

| field | v1の値 | 意味 |
| --- | --- | --- |
| `version` | unquoted integer `1` | contract version |
| `profile` | `standard` / `extended` | claim前に選択するworker profile |
| `network` | `none` / `localhost` / `public` | 必要なcommand network scope |
| `browser_cdp` | boolean | browserまたはCDPが必要 |
| `download` | boolean | download経路が必要 |
| `external_time_gate` | boolean | supervisor外の時刻前提を満たせるprofileが必要 |

unknown key、unknown enum、unknown version、field欠損、duplicate block/key、YAML alias/custom tag、quoted version、型不一致はfail-closedである。v1 readerは将来versionを部分解釈しない。producerが新versionを出すのは、全consumer（通常queue、`RunOnce` manual admission、startup後queue）がそのversionを扱えるようになった後だけとする。

metadataがない既存ready Issueも`capability_metadata_missing`でskipする。migration時はready labelを外し、contractを追加してからready labelを最後に戻す。すでにdurable claimを持つlegacy runはその保存provenanceを維持し、本文から能力を推測し直さない。

## Providerの導出

`.agent-loop.yaml`の`worker.profiles.<name>.capabilities`はprofileごとの非secret allowlistである。supervisorはこれをそのまま信用せず、built-in adapterが実際に組み立てる`worker.command_network`、sandbox、backend経路との共通部分だけを提供能力とする。

- `disabled` routeのnetworkは`none`である。
- `localhost-only` routeのnetworkは`localhost`であり、このrouteだけbrowser/CDPとdownloadを提供可能とする。
- 現在のbuilt-in routeは`public` command networkを提供しない。profileが`public`を宣言してもeffective providerは安全側へ縮退し、doctorは`WORKER_PROFILE_LAUNCH_MISMATCH`を返す。
- `external_time_gate`はoperatorが用意した外部orchestrationの非secret propertyとしてprofileへ明示する。timestamp、token、credential値をmetadata、state、status、doctorへ保存しない。

profileは実起動経路より能力を少なく宣言してよい。実起動経路を超える宣言はdriftである。`status --json`は`capability_admission.contract_version`とeffective `profiles`を返し、claimed Issueのstateには保存済み`capability_requirements`と`worker_capabilities`が含まれる。

## Admission predicateと副作用境界

判定はIssue snapshotとprofile mapだけを入力とする決定的なpure functionで、clock、filesystem、environment、network、credentialを読まない。不一致は`capability_mismatch`でskipし、`mismatches[].code`に次のstable codeを返す。

- `capability_metadata_missing`
- `capability_metadata_invalid`
- `capability_profile_unknown`
- `capability_network_mismatch`
- `capability_browser_cdp_mismatch`
- `capability_download_mismatch`
- `capability_external_time_gate_mismatch`

queue sort後、各candidateのcapability predicateを評価する。不一致の先頭candidateで探索を止めず、capacityとresourceが許す後続compatible candidateを選択する。選択後もauthoritative Issueを再取得して同じpredicateを再評価し、次の副作用より前に不一致を検出する。

1. durable Issue/state作成
2. resource lease予約
3. GitHub ready/running label変更とclaim comment
4. worktree作成
5. worker spawn

不一致では上記を一切行わない。resource fallback、dependency、retry budget、needs-input parkはcapability許可へ昇格しない。新contractで開始したrunはrequirements/providerをclaim時に保存し、answer後またはenvironment block後にpark済みleaseを再取得する前にも、保存requirementsを現在のeffective profileへ同じvalidatorで照合する。secret、token、credentialの名前や値はcontract modelとmismatch modelに存在しない。
