# ADR-0001: GitHub外形監視を独立processにする

## Status

Accepted

## Decision

可用性判定はGitHubのIssue、label、Issue event時刻だけを入力とするread-only processに置く。supervisorとbinary、LaunchAgent、config、state、logを分離し、独立した `monitor-v*` releaseで配布する。

当初は本体と同じreleaseで配布していたが、monitorだけの更新・切戻しを可能にするため #576 で配布系列を分離した。repository・Go moduleは共通のまま、labelの意味とruntime metadata schemaを接点とし、リリース番号一致は要求しない。

synthetic/canary Issueは使わない。queueが空の期間を成功として扱わず`IDLE`とする。GitHubまたはmonitor履歴が不足する期間を`UNKNOWN`とし、`HEALTHY`へ補完しない。

## Consequences

supervisor自身が停止・破損していても外形判定の入力境界は変わらない。一方、実需要がない期間はavailabilityの証拠にならず、GitHub観測不能時の原因をsupervisor内部情報で推測することもできない。
