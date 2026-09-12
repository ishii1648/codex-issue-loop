# Release・install・update方針

## 対応範囲

- target: Apple Silicon macOS（`darwin/arm64`）
- build: Go 1.22以上、`CGO_ENABLED=0`、`-trimpath`、VCS情報はversion/commitとして明示的に埋め込む
- runtime support: macOS 13以降の最新2 major versionを通常サポートし、それ以前はbest effortとする
- release version: annotated Git tagの`vMAJOR.MINOR.PATCH`をbinary、Skill `VERSION`、install manifestで共通利用する

GitHub Releaseには次を公開する。

- `agent-loop_Darwin_arm64`
- `agent-loop-monitor_Darwin_arm64`
- `agent-loop_Darwin_arm64.spdx.json`（SPDX 2.3）
- `checksums.txt`（SHA-256）
- `release-manifest.json`（delivery protocol、tag/commit、target、artifact digest、schema互換範囲）
- GitHub Actions artifact provenance attestation

suffixなしのstable Releaseが唯一の正式releaseであり、GAは別の段階・channel・状態として定義しない。candidateは公開前検証の一時artifactで、repository assignmentの対象ではない。stable公開後のrepository別適用成否はrollout状態であり、releaseの成熟度を変更しない。

release jobは同じtag、commit、`SOURCE_DATE_EPOCH`から2回buildし、binary、SBOM、checksumのbyte一致を確認してから公開する。repository固有の長期secretは使わず、GitHub Actionsの短命OIDC tokenと`GITHUB_TOKEN`だけを使う。

GitHub Actionsのartifact downloadとGitHub Release downloadでは実行modeが保持されないため、isolated canaryとstable readbackはdownload後にbinaryを`0755`へ戻してから実行する。これはfile bytesを変更しない。mode復元後もmanifestのSHA-256とattestationを正本とし、不一致時はcandidate公開またはstable公開を停止する。

candidate integrityは待機を挟まず、candidate prereleaseから取得したbinaryとcanonical artifactのbyte一致およびGitHub attestationを即時検証する。通常releaseは本番snapshotの採取・比較を要求しない。Release workflowはstable公開後のartifact readbackで完了する。repository rolloutはproduction hostで検証し、5分間のhealth soakとして開始時・1分後・5分後に対象repositoryのassignment、doctor、statusを採取する。

通常CIはsourceの品質検証を行い、Release workflowがtagged sourceの品質検証と配布artifactの再現性・metadataを確認する。ローカルの`scripts/check-release.sh`はCIを使えない場合やrelease script変更時の検証に使い、同じcommitの成功済みCIとローカル全量検証を重複させない。

release artifact の `version --json` と manifest は snapshot の単一 version=6 を報告する。既存名 `state_schema_current` / `semantic_contract_current` は同じ契約値を参照する。既存 migration の移行先はまだ v5 であるため、#536/#537 統合まで [配布保留境界](release-gates.md) を維持する。release check は build の契約整合性を検証するが、通常配布の解除を意味しない。

## Release作成

1. `main`の対象commitのCIとIssue/milestoneを確認する。同じcommitのCI成功後にローカル全量検証を追加で待たない。
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

stable公開後のassignment、変更内容に応じたrollback drill、health reportはRelease workflowの完了条件ではない。production hostでのrollout検証が成功し、`production-health-report.json`をstable Releaseへ追加した時点でrollout完了とする。rollout失敗時は対象repositoryだけをpreviousへ戻し、artifact自体の修正が必要と確認できた場合に限って新しいpatch releaseを作る。

## Mac側pull型delivery

v0.9.0以降の通常経路はrepository別assignmentである。stable公開は全repositoryを更新せず、operatorがexact versionとpreview generationを指定して1 repositoryずつ適用する。設定、CLI、初回v1→v2 migration、rollback、隔離evidenceは[Repository別stable delivery](per-repository-delivery.md)を正本とする。設計判断は[ADR-0004](adr/0004-per-repository-stable-assignment.md)に記録する。

```sh
agent-loop delivery assignment migrate --json
agent-loop delivery assignment migrate --apply --json
agent-loop delivery assignment preview --repo /absolute/path/to/repository --version v1.2.3 --json
agent-loop delivery assignment apply --repo /absolute/path/to/repository --version v1.2.3 --expected-generation 1 --json
agent-loop delivery assignment verify --repo /absolute/path/to/repository --json
```

v2 configでは`auto_apply: never`とstable channelだけを許可し、host-wide `delivery apply`を拒否する。以下のhost-wide transaction説明はv0.8.5以前のbinaryによるv1 configのrollback/recovery互換境界に限る。v0.9.0以降のbinaryへv1 configを直接渡してhost-wide操作してはならない。

### v1 host-wide controller（legacy recoveryのみ）

初回installとdoctor完了後、Macごとに1つのcontrollerをpreviewしてから有効化する。

```sh
agent-loop delivery configure --json
agent-loop delivery configure --apply --json
agent-loop delivery check --json
agent-loop delivery status --json
```

設定は`$HOME/.agent-loop-delivery.yaml`だけに置き、regular file、現在userのowner、mode `0600`を必須とする。`--config`はtestまたは明示運用用のabsolute pathだけを受理する。credentialは保存せず既存の`gh`認証を使う。transaction、download cache、log、maintenance fenceは`$HOME/Library/Application Support/codex-issue-loop/delivery/`配下であり、設定fileや各repositoryへ展開しない。

v1 host-wideの`delivery status --json`では、`last_check_at`、`next_check_at`、`drain_started_at`、`drain_deadline`を常に出力する。`delivery/transaction.json`では、これらに加えて`started_at`と`updated_at`も常に出力する。未設定時刻は`"0001-01-01T00:00:00Z"`で表し、consumerはキーの有無ではなく時刻のゼロ値で未設定を判定する。

`check`/`reconcile`はdraft/prereleaseを除く最新production Releaseのannotated SemVer tagをcommitへpeelし、`release-manifest.json`、`checksums.txt`、binaryとmanifest双方のGitHub attestation、`darwin/arm64` target、binary埋め込みmetadataを照合する。checksumとtrusted release workflow attestationの完了前にcandidate binaryを実行しない。download中にRelease/tagが変化した場合はfresh candidateでやり直す。major、schema migration、downgrade、同一version異commit、未知manifest/protocolは自動適用しない。

`com.codex-issue-loop.delivery`は`RunAtLoad`と`StartInterval`で短命な`delivery reconcile`を実行する。host lockで手動`apply`との多重実行を拒否し、永続phaseと固定backup pathから再開する。drain timeoutではworkerをkillせずfenceを解除してdeferする。apply後はfenceを維持したまま全repositoryを含む`doctor --json`を二度実行してsoakし、失敗時は通常Issue処理の再開前にrollbackする。rollbackも失敗した場合はfenceとbackupを保持してfail closedする。

```sh
agent-loop delivery pause --json
agent-loop delivery resume --json
agent-loop delivery apply --version v1.2.3 --json
```

pause/resumeはactive maintenance transaction中には変更できない。schema migrationとmajor updateは従来どおり全loop停止、migration preview、paired rollbackを明示承認する手動runbookへ移す。

`rollback_failed`の再試行は通常reconcileから行わない。原因解消、exact managed backup、retained maintenance fenceをoperatorが確認した場合だけ、検証済みcandidateの`delivery retry-rollback --backup <exact-path> --confirm-retained-fence --json`を使用する。previous installへ既に戻っている場合はrestoreを重ねずhealthだけを再検証し、成功時だけfenceを解除する。

retry時のaggregate validationがlegacy completed merged identityだけを理由にmaintenance snapshotを隔離した場合は、検証済みcandidateの`recover-quarantined-snapshot`をdry-runし、exact backupと全GitHub PR identityを確認した後だけ専用confirmで復元する。これは一般の破損stateや追加invariant違反を許容するcompatibility bypassではない。

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

storage versionが同じでもsemantic contract migrationが必要なら自動再開しない。`migrate --json`の`non_migratable`が空であることを確認し、apply、doctor、repositoryごとのstartの順を守る。rollback時はmigration backupを先にrestoreし、安全な旧artifactへ戻す。

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
