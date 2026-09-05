# ADR-0001: GitHub外形監視を独立processにする

## Status

Accepted

## Decision

可用性判定はGitHubのIssue、label、Issue event時刻だけを入力とするread-only processに置く。supervisorとbinary、LaunchAgent、config、state、logを分離し、同じreleaseで配布する。

synthetic/canary Issueは使わない。queueが空の期間を成功として扱わず`IDLE`とする。GitHubまたはmonitor履歴が不足する期間を`UNKNOWN`とし、`HEALTHY`へ補完しない。

## Consequences

supervisor自身が停止・破損していても外形判定の入力境界は変わらない。一方、実需要がない期間はavailabilityの証拠にならず、GitHub観測不能時の原因をsupervisor内部情報で推測することもできない。
