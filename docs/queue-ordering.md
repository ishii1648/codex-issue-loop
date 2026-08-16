# Queue ordering

`agent-loop`はGitHub APIの返却順を使わず、eligibleなIssueを全件取得してからlocalで決定論的に並べ替える。既定strategyは従来どおり`issue_number_asc`であり、既存の`.agent-loop.yaml`を変更する必要はない。

将来の並列schedulerはこの全順序をresource競合のないIssueへ順番に適用する。dependency評価、競合Issueのskip、同じsnapshotから同じadmission結果を得る規則は[Resource admission契約](resource-admission.md)を正本とする。

## Strategies

| `queue.order` | Primary key | Tie-break |
| --- | --- | --- |
| `issue_number_asc` | Issue番号の昇順 | Issue番号自体が一意 |
| `created_at_asc` | GitHub `createdAt`の古い順 | Issue番号の昇順 |
| `priority_then_created_at` | `priority_labels`で最初に一致する順位 | 作成日時、Issue番号の昇順 |

priority順を使う例を示す。配列の先頭ほど優先度が高い。

```yaml
queue:
  order: priority_then_created_at
  priority_labels:
    - priority:critical
    - priority:high
    - priority:normal
    - priority:low
```

- configured priority labelがないIssueは、priority付きIssueの後へ並ぶ。
- 複数のconfigured priority labelが付いている場合は、配列内で最も上位のlabelを採用する。
- label名の比較は大文字小文字を区別しない。
- 同じpriority・作成日時でもIssue番号で順序を確定する。
- GitHub応答に作成日時が欠ける異常なfixtureは、日時があるIssueの後に置き、Issue番号で確定する。

`priority_then_created_at`では`priority_labels`が1件以上必要である。空文字、前後の空白、大文字小文字を無視した重複、未知の`order`はconfig errorとしてsupervisor開始前に拒否する。

## Paginationと状態変更

GitHub CLIが内部で複数pageを取得した後の一つの候補集合に対してsortする。各pageの返却順やpage境界は選択結果に影響しない。現在の取得上限は1 pollあたり1000件であり、eligible Issueが上限を超える運用ではキュー分割または上限拡張を別途検討する。

`queue.order`を変更しても、`claiming`、`claimed`、`running`、`needs_input`、`awaiting_checks`、`awaiting_merge`、`completed`、`blocked`のIssueを取消・再配置しない。次回pollで、まだclaimされていない候補だけに新しいstrategyを適用する。設定変更時は通常どおりloopをrestartし、`doctor`で設定とpriority labelを確認する。

## Priority labelの準備

`priority_labels`に指定したlabelは対象GitHub repositoryに必要である。`bootstrap-labels`のpreviewには不足priority labelも含まれ、`--apply`は不足分だけを作成する。既存labelの色・説明は変更しない。

```sh
agent-loop bootstrap-labels --repo /absolute/path/to/repository --json
agent-loop bootstrap-labels --repo /absolute/path/to/repository --apply --json
agent-loop doctor --repo /absolute/path/to/repository --json
```

組織の既存priority体系を使う場合は、そのlabel名を高い順に設定する。priorityの意味、付与権限、自動付与ruleは対象repository側で管理する。
