# Release・install・update方針

## 対応範囲

- target: Apple Silicon macOS（`darwin/arm64`）
- build: Go 1.22以上、`CGO_ENABLED=0`、`-trimpath`、VCS情報はversion/commitとして明示的に埋め込む
- runtime support: macOS 13以降の最新2 major versionを通常サポートし、それ以前はbest effortとする
- release version: annotated Git tagの`vMAJOR.MINOR.PATCH`をbinary、Skill `VERSION`、install manifestで共通利用する

GitHub Releaseには次を公開する。

- `agent-loop_Darwin_arm64`
- `agent-loop_Darwin_arm64.spdx.json`（SPDX 2.3）
- `checksums.txt`（SHA-256）
- GitHub Actions artifact provenance attestation

release jobは同じtag、commit、`SOURCE_DATE_EPOCH`から2回buildし、binary、SBOM、checksumのbyte一致を確認してから公開する。repository固有の長期secretは使わず、GitHub Actionsの短命OIDC tokenと`GITHUB_TOKEN`だけを使う。

Pull Requestとmainの通常CIでも`scripts/check-release.sh`を実行し、固定test versionから2回作成したartifactのbyte一致と埋め込みversion/commitを確認する。

## Release作成

1. `main`のCIとIssue/milestoneを確認する。
2. releaseするcommitへannotated tagを作る。
3. tagをpushし、Release workflowの成功を確認する。

```sh
git tag -a v1.2.3 -m 'v1.2.3'
git push origin v1.2.3
gh run list --workflow Release --limit 1
```

lightweight tag、semantic versionでないtag、`main`に含まれないcommitはworkflowが拒否する。

## Artifact検証

```sh
gh release download v1.2.3 --repo ishii1648/codex-issue-loop --dir ./agent-loop-release
cd ./agent-loop-release
shasum -a 256 -c checksums.txt
gh attestation verify agent-loop_Darwin_arm64 --repo ishii1648/codex-issue-loop
chmod 0755 agent-loop_Darwin_arm64
./agent-loop_Darwin_arm64 version --json
```

checksum、attestation、version/commitのいずれかが一致しなければ実行・installしない。

## 新規install

loopが動いていないことを確認して、検証済みartifactから実行する。

```sh
./agent-loop_Darwin_arm64 install --json
agent-loop doctor --json
```

installはbinary、Skill、Skill `VERSION`、`install.json`を原子的なfile replacementで配置する。同じartifactからの再実行は`changed: false`となる。

## 安全なupdate

```sh
./agent-loop_Darwin_arm64 update --json
agent-loop doctor --json
```

`update`は次の順序で動く。

1. 同一version/checksumなら何も変更せず終了する。
2. 現在のbinary、Skill、Skill version、manifestを`~/Library/Application Support/codex-issue-loop/backups/`へ保存する。
3. 稼働中だったrepositoryのLaunchAgentだけを停止する。
4. 新しいartifactをinstallし、全登録repositoryのplistを現行形式で再生成する。
5. 元々稼働中だったLaunchAgentだけを再開する。
6. 途中で失敗した場合は旧install一式を自動復元し、元のLaunchAgentを再開する。

state、event、worker log、worktree、registryは通常updateの対象ではなく保持される。schema migrationを伴うversionでは全loopを先に停止する。新artifactの`update`はbinary/Skillだけを配置して自動再開せず、`schema_migration_required: true`を返す。その後、installed binaryで`migrate --apply`を実行してからdoctorとstartへ進む。詳細は[永続schema migration runbook](migration.md)を正本とする。

## rollback

`update`結果の`backup`絶対pathを指定する。

```sh
agent-loop rollback \
  --backup '/Users/name/Library/Application Support/codex-issue-loop/backups/<backup>' \
  --json
agent-loop doctor --json
```

CLIは管理対象backups配下だけを受け付け、manifestとbinary/Skill checksumを検証する。rollbackも元々稼働していたLaunchAgentだけを停止・再開し、state/worktreeを変更しない。旧binaryが現在のconfig/state versionを読めない場合は、先に対応するmigration backupを`migrate --rollback`で復元し、その後にinstall backupを`rollback`する。逆順はCLIが拒否する。

## Homebrew・Apple署名・notarization

初期配布はGitHub Release + checksum + provenance attestationを正本とし、Homebrew tapは採用しない。tap repositoryとformula更新の保守責任を増やさず、`update`/`rollback`のstate保持を先に一貫させるためである。

Developer ID署名とnotarizationは、不特定多数へGUI経由で直接配布する段階では必須とする。現段階ではApple Developer credentialをrepositoryへ導入せず、署名済みと表示しない。管理されたMac miniでは、`gh release download`で取得しchecksumとGitHub attestationを検証する。Gatekeeper警告を無効化する手順は提供しない。公開配布へ移る際は、Apple Developer ID、notary profile、credential rotation、失効時対応を別途実装してからsupport policyを更新する。
