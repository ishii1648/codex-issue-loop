# codex-issue-loop

GitHub Issue をキューとして、着手可能な Issue が存在する限り Codex CLI のワーカーを繰り返し実行する、macOS 向けの常駐ループです。

このリポジトリは現在、要件定義・仕様策定の段階です。実装はまだ含みません。

## Documents

- [要件定義](docs/requirements.md)
- [システム仕様](docs/specification.md)

## 設計の要点

- ループ本体は Codex の task や goal ではなく、独立した `agent-loop` CLI が担う
- macOS の `launchd` がループの生存を管理する
- Issue ごとに独立した `codex exec` ワーカーを起動する
- Codex Skill は起動・停止・監視・回答を CLI に橋渡しする薄い操作層とする
- スマートフォンでは、監視用 task と Issue 作成用 task の2つを主な入口にする
- ユーザーへの質問が必要になった場合は状態を永続化し、監視用 task を通して回答できるようにする

