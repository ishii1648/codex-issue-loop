# agent-loop-monitor

`agent-loop-monitor`はGitHub上のIssue labelとIssue event時刻だけから実装キューの可用性を記録する独立サブシステムです。supervisorのstate、metrics、logs、process状態を正常性判定に使わず、GitHubを変更しません。

## クイックスタート

`config.example.yaml`を`~/.agent-loop-monitor.yaml`へコピーし、`state_dir`と監視対象repositoryを実環境に合わせます。releaseに同梱されたbinaryを使います。

```sh
./agent-loop-monitor_Darwin_arm64 install --config ~/.agent-loop-monitor.yaml --json
~/Library/Application\ Support/codex-issue-loop-monitor/bin/agent-loop-monitor service register --config ~/.agent-loop-monitor.yaml --json
~/Library/Application\ Support/codex-issue-loop-monitor/bin/agent-loop-monitor service start --config ~/.agent-loop-monitor.yaml --json
```

確認コマンドは次のとおりです。

```sh
agent-loop-monitor run --config ~/.agent-loop-monitor.yaml --once --json
agent-loop-monitor status --config ~/.agent-loop-monitor.yaml --json
agent-loop-monitor history --config ~/.agent-loop-monitor.yaml --repo ishii1648/codex-issue-loop --from 2026-09-01T00:00:00Z --json
agent-loop-monitor report --config ~/.agent-loop-monitor.yaml --from 2026-09-01T00:00:00Z --json
agent-loop-monitor service status --config ~/.agent-loop-monitor.yaml --json
```

状態と計算の正本は[specification](docs/specification.md)、境界は[architecture](docs/architecture.md)、導入・復旧は[runbook](docs/runbook.md)を参照してください。

- [requirements](docs/requirements.md)
- [architecture](docs/architecture.md)
- [specification](docs/specification.md)
- [runbook](docs/runbook.md)
- [ADR-0001: GitHub外形監視を独立processにする](docs/adr/0001-independent-github-black-box-monitor.md)
