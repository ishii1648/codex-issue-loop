# Release・install・update方針

## 対応範囲

- target: Apple Silicon macOS（`darwin/arm64`）
- build: Go 1.22以上、`CGO_ENABLED=0`、`-trimpath`、VCS情報はversion/commitとして明示的に埋め込む
- runtime support: macOS 13以降の最新2 major versionを通常サポートし、それ以前はbest effortとする
- release version: annotated Git tagの`vMAJOR.MINOR.PATCH`をbinary、Skill `VERSION`、install manifestで共通利用する

GitHub Releaseには次を公開する。

- `agent-loopctl_Darwin_arm64`（ホスト共通 CLI）
- `host-release-manifest.json`（ホスト binary の version、commit、digest、委譲 protocol）
- `agent-loop_Darwin_arm64`（repository runtime）
- `agent-loop-monitor_Darwin_arm64`
- `agent-loop_Darwin_arm64.spdx.json`（SPDX 2.3）
- `checksums.txt`（SHA-256）
- `release-manifest.json`（delivery protocol、tag/commit、target、artifact digest、schema互換範囲）
- GitHub Actions artifact provenance attestation

suffixなしのstable Releaseが唯一の正式releaseであり、GAは別の段階・channel・状態として定義しない。candidateは公開前検証の一時artifactで、repository assignmentの対象ではない。stable公開後のrepository別適用成否はrollout状態であり、releaseの成熟度を変更しない。

release jobは同じtag、commit、`SOURCE_DATE_EPOCH`から2回buildし、binary、SBOM、checksumのbyte一致を確認してから公開する。repository固有の長期secretは使わず、GitHub Actionsの短命OIDC tokenと`GITHUB_TOKEN`だけを使う。

GitHub Actionsのartifact downloadとGitHub Release downloadでは実行modeが保持されないため、isolated canaryとstable readbackはdownload後にbinaryを`0755`へ戻してから実行する。これはfile bytesを変更しない。mode復元後もmanifestのSHA-256とattestationを正本とし、不一致時はcandidate公開またはstable公開を停止する。

candidate integrityは待機を挟まず、candidate prereleaseから取得したbinaryとcanonical artifactのbyte一致およびGitHub attestationを即時検証する。production hostのraw snapshotはowner-onlyのprivate evidenceに限定し、公開Releaseにはcandidate digest、非変更結果、安全条件、private evidence digestだけのredacted summaryを添付する。Release workflowはstable公開後のartifact readbackで完了する。repository rolloutは独立workflowで検証し、5分間のhealth soakとして開始時・1分後・5分後に対象repositoryのassignment、doctor、statusを採取する。

Pull Requestとmainの通常CIでも`scripts/check-release.sh`を実行し、固定test versionから2回作成したartifactのbyte一致と埋め込みversion/commitを確認する。

release artifactの`version --json`とinstall manifestはstorage schemaのcurrent/migration-from、およびsemantic contractのcurrent/minimumを明示する。release checkはこの範囲とversioned state contractを検査し、execution-required provenance追加にmigration/compatibility ruleがないbuildを拒否する。release前にはsupported旧versionのactive、blocked、needs-input、retry、publication recovery fixtureへcurrent validatorを適用する。

## Release作成

1. `main`のCIとIssue/milestoneを確認する。
2. releaseするcommitへannotated tagを作る。
3. tagをpushし、`verify-stable-release`までのRelease workflow成功を確認する。

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

stable公開後のassignment、rollback drill、health reportはRelease workflowの完了条件ではない。rollout成功後に`production-health-report.json`をstable Releaseへ追加し、独立した`Repository rollout health` workflowを起動する。rollout失敗時は対象repositoryだけをpreviousへ戻し、artifact自体の修正が必要と確認できた場合に限って新しいpatch releaseを作る。

## Mac側pull型delivery

v0.9.0以降の通常経路はrepository別assignmentである。stable公開は全repositoryを更新せず、operatorがexact versionとpreview generationを指定して1 repositoryずつ適用する。設定、CLI、初回v1→v2 migration、rollback、隔離evidenceは[Repository別stable delivery](per-repository-delivery.md)を正本とする。設計判断は[ADR-0004](adr/0004-per-repository-stable-assignment.md)に記録する。

```sh
agent-loop delivery assignment migrate --json
agent-loop delivery assignment migrate --apply --json
agent-loopctl delivery assignment preview --repo /absolute/path/to/repository --version v1.2.3 --json
agent-loopctl delivery assignment apply --repo /absolute/path/to/repository --version v1.2.3 --expected-generation 1 --json
agent-loopctl delivery assignment verify --repo /absolute/path/to/repository --json
```

v2 configでは`auto_apply: never`とstable channelだけを許可し、host-wide `delivery apply`を拒否する。以下のhost-wide transaction説明はv0.8.5以前のbinaryによるv1 configのrollback/recovery互換境界に限る。v0.9.0以降のbinaryへv1 configを直接渡してhost-wide操作してはならない。

### v1 host-wide controller（legacy recoveryのみ）

初回installとdoctor完了後、Macごとに1つのcontrollerをpreviewしてから有効化する。

```sh
agent-loopctl delivery configure --json
agent-loopctl delivery configure --apply --json
agent-loopctl delivery check --json
agent-loopctl delivery status --json
```

設定は`$HOME/.agent-loop-delivery.yaml`だけに置き、regular file、現在userのowner、mode `0600`を必須とする。`--config`はtestまたは明示運用用のabsolute pathだけを受理する。credentialは保存せず既存の`gh`認証を使う。transaction、download cache、log、maintenance fenceは`$HOME/Library/Application Support/codex-issue-loop/delivery/`配下であり、設定fileや各repositoryへ展開しない。

`check`/`reconcile`はdraft/prereleaseを除く最新production Releaseのannotated SemVer tagをcommitへpeelし、`release-manifest.json`、`checksums.txt`、binaryとmanifest双方のGitHub attestation、`darwin/arm64` target、binary埋め込みmetadataを照合する。checksumとtrusted release workflow attestationの完了前にcandidate binaryを実行しない。download中にRelease/tagが変化した場合はfresh candidateでやり直す。major、schema migration、downgrade、同一version異commit、未知manifest/protocolは自動適用しない。

`com.codex-issue-loop.delivery`は`RunAtLoad`と`StartInterval`で短命な`delivery reconcile`を実行する。host lockで手動`apply`との多重実行を拒否し、永続phaseと固定backup pathから再開する。drain timeoutではworkerをkillせずfenceを解除してdeferする。apply後はfenceを維持したまま全repositoryを含む`doctor --json`を二度実行してsoakし、失敗時は通常Issue処理の再開前にrollbackする。rollbackも失敗した場合はfenceとbackupを保持してfail closedする。

```sh
agent-loopctl delivery pause --json
agent-loopctl delivery resume --json
agent-loop delivery apply --version v1.2.3 --json
```

pause/resumeはactive maintenance transaction中には変更できない。schema migrationとmajor updateは従来どおり全loop停止、migration preview、paired rollbackを明示承認する手動runbookへ移す。

`rollback_failed`の再試行は通常reconcileから行わない。原因解消、exact managed backup、retained maintenance fenceをoperatorが確認した場合だけ、検証済みcandidateの`delivery retry-rollback --backup <exact-path> --confirm-retained-fence --json`を使用する。previous installへ既に戻っている場合はrestoreを重ねずhealthだけを再検証し、成功時だけfenceを解除する。

retry時のaggregate validationがlegacy completed merged identityだけを理由にmaintenance snapshotを隔離した場合は、検証済みcandidateの`recover-quarantined-snapshot`をdry-runし、exact backupと全GitHub PR identityを確認した後だけ専用confirmで復元する。これは一般の破損stateや追加invariant違反を許容するcompatibility bypassではない。

## ホスト CLI の導入・切替

ユーザー向けの入口は `agent-loopctl`、repository LaunchAgent の実行対象は immutable slot の `agent-loop` である。ホスト binary と `host-release-manifest.json` の checksum、GitHub attestation、version/commit を検証したうえで実行する。

```sh
gh attestation verify agent-loopctl_Darwin_arm64 --repo ishii1648/codex-issue-loop
gh attestation verify host-release-manifest.json --repo ishii1648/codex-issue-loop
chmod 0755 agent-loopctl_Darwin_arm64
./agent-loopctl_Darwin_arm64 version --json
./agent-loopctl_Darwin_arm64 install --json
```

初回 install は同じ stable version/commit の runtime を既存の release verifier で取得・検証して immutable slot に配置する。新規登録の bootstrap と remote answer にこの runtime を使う。ホストの記録は `host-install.json`、binary は管理 root の `bin/agent-loopctl` に置く。既存の `install.json` と repository assignment は上書きしない。

既存環境では、先に delivery config の v2 migration と repository LaunchAgent の immutable slot 化を済ませる。global binary を参照する repository が残る場合は install を拒否する。切替前に各 runtime が `version --json` の `repository_command_protocol: 1` に対応していることを確認する。非対応 runtime は通常操作の委譲を拒否するため、必要な runtime 更新はホスト切替とは別の明示操作として行う。

管理対象の旧 `bin/agent-loop` は `agent-loopctl` を案内して終了するスクリプトになる。既配布のコピーや管理外 PATH にある旧 binary は遡及修正できない。`type -a agent-loop agent-loopctl` で確認し、旧共通 binary の PATH 登録・alias を除き、管理 root の `bin` を PATH に登録する。slot 内の `agent-loop` は削除・置換しない。既存の共有 broker/delivery plist はホスト CLI を起動するように切り替えるが、repository plist は変更しない。

## ホストだけの update・rollback

```sh
./agent-loopctl_Darwin_arm64 update --json
agent-loopctl rollback --json
```

検証済みの新しいホスト artifact から update する。旧ホスト binary、Skill、manifest は `host-backups/<digest>/` に保存し、rollback は記録された直前のバックアップの digest を検証して復元する。初回切替より前の旧共通 CLI への rollback は提供しない。ホスト更新・rollback は repository の assignment、LaunchAgent、snapshot/event、migration を変更しない。bootstrap runtime も保持する。

repository runtime の変更は `agent-loopctl delivery assignment preview/apply/rollback --repo <path>`、snapshot migration は `agent-loopctl migrate --repo <path>` を使用する。migration の判定と実行は選択された runtime が所有し、別 repository のファイルを読まない。共通 registry 自体の旧 schema migration はホスト切替前に従来の正式手順で完了させる。

uninstall は登録済み repository が残っている間は拒否する。全 repository の unregister 後に `agent-loopctl uninstall --json` を実行すると、共有 LaunchAgent とホスト binary/Skill を除去する。immutable slots、snapshot、バックアップは削除しない。

## Homebrew・Apple署名・notarization

初期配布はGitHub Release + checksum + provenance attestationを正本とし、Homebrew tapは採用しない。tap repositoryとformula更新の保守責任を増やさず、`update`/`rollback`のstate保持を先に一貫させるためである。

Developer ID署名とnotarizationは、不特定多数へGUI経由で直接配布する段階では必須とする。現段階ではApple Developer credentialをrepositoryへ導入せず、署名済みと表示しない。管理されたMac miniでは、`gh release download`で取得しchecksumとGitHub attestationを検証する。Gatekeeper警告を無効化する手順は提供しない。公開配布へ移る際は、Apple Developer ID、notary profile、credential rotation、失効時対応を別途実装してからsupport policyを更新する。
