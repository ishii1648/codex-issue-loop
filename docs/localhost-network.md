# localhost-only command network

## 適用範囲

既定のworker command networkは無効である。実HTTP serverやChrome CDPを必要とするrepositoryだけ、停止中に次を設定して再登録する。

```yaml
worker:
  backend: codex
  sandbox: workspace-write
  command_network:
    policy: localhost-only
    proxy: true
    allowed_hosts: [localhost, 127.0.0.1]
```

`allowed_hosts`は設定可能な一般allowlistではなく、レビュー済みpolicyの完全一致確認である。空、順序違い、重複、`*`、public hostname、RFC1918/LAN、link-local、IPv6、Unix socket、`dangerously_*` optionは拒否する。`approval_policy="never"`、`workspace-write`、worktree外書込み禁止、決定論的publisherは変わらない。

Codexの[Configuration Reference](https://learn.chatgpt.com/docs/config-file/config-reference)が明記するように、`sandbox_workspace_write.network_access`だけではdomain policyにならず、`features.network_proxy`が必要である。またproxyはWeb Search、apps、MCPなどhosted toolをfilterしない。このためadapterは`codex exec --ignore-user-config --strict-config`を使い、proxy設定とともにWeb Search、Browser/Computer Use、apps/plugins、MCP、remote plugin、skill由来MCP/tool suggestionを無効化する。

## fail-closed確認

設定変更後はworkerを開始する前に次を実行する。

```sh
agent-loop stop --repo /absolute/path/to/repository --json
agent-loop register --repo /absolute/path/to/repository --json
agent-loop doctor --repo /absolute/path/to/repository --json
```

`CODEX_LOCALHOST_NETWORK_PROXY_READY`が成功しなければ開始しない。`CODEX_LOCALHOST_NETWORK_PROXY_UNAVAILABLE`は、Codex CLIを更新して再度`register`と`doctor`を行う。proxy起動、strict config、unknown featureでCodex processが失敗した場合も通常workerへfallbackせず、run logのsecret-safeな末尾を調査する。

## 実機受け入れ

macOSの専用標準ユーザーで、workerがspawnする子processを含めて次を確認する。

1. network無効repositoryでは`Deno.listen({hostname: "127.0.0.1"})`と外部connectが拒否される。
2. `localhost-only` repositoryでは`127.0.0.1`へbindしたHTTP serverへ`localhost`と`127.0.0.1`で接続できる。
3. 同じworkerがspawnしたChromeのloopback CDP endpointへ接続し、実canvas screenshotを保存できる。
4. 親commandとspawnした子processの両方から`example.com`、public IP、RFC1918/LAN、`169.254.0.0/16`、許可外hostname、任意Unix socketへ接続できない。
5. Web Search、Browser/Computer Use、MCP/apps/pluginsがworker tool一覧に現れない。

通常unit/integration suiteはCodex認証やmodel利用を行わず、生成argv・capability・state machineをfake executableで検証する。実Codex/macOS sandbox/Chrome E2Eはrelease導入hostで明示的に行い、日時、Codex version、OS version、成功・拒否endpointを秘密値なしで記録する。

## 残余リスク

`localhost`と`127.0.0.1`はportを限定しないhost-wide許可である。同じmacOSユーザーまたはhost上の別serviceをport scanし、未認証loopback serviceへ接続するリスクはproxyだけでは除去できない。機密性の高いhostでは専用標準ユーザー、不要service停止、OS firewall、専用brokerまたは別host/VMを下位境界として必須にする。同一ユーザーの悪意あるprocessやOS侵害はこのpolicyの境界外である。
