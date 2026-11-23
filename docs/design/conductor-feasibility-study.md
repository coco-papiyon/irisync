# Microsoft ConductorによるAI開発自動化 検討・設計方針

**版数:** 1.0
**改訂日:** 2026-11-23
**ステータス:** 技術方針確定・実装準備
**関連文書:** `docs/design/basic-design.md`

---

## 1. 目的

Microsoft Conductorをオーケストレーターとして採用し、GitHub ProjectsおよびCodex CLIを利用したAI開発自動化環境を構築する。

本システムは、Issueを起点として、設計、承認、実装、検証、PR作成、コードレビュー、修正までの一連の開発工程を管理する。

設計承認と最終マージは人間が実施する。

### 基本原則

1. AIは設計、実装、レビュー、修正など必要な工程でのみ利用する。
2. ワークフローの制御・条件分岐には原則としてAIを使用しない。
3. GitHub ProjectsのStatusを開発進捗の管理に利用する。
4. 人間による設計承認と最終マージを必須とする。
5. 自動検証を優先し、AIの利用コストを抑制する。
6. 実行履歴を永続化し、中断や障害から安全に復旧できるようにする。

## 2. 採用技術

| 項目                 | 採用技術・方式                 |
| ------------------ | ----------------------- |
| オーケストレーター          | Microsoft Conductor     |
| AIエージェント           | OpenAI Codex CLI        |
| AI連携               | Conductor Scriptステップ経由  |
| Polling Controller | Go                      |
| ポーリング方式            | Go常駐プロセス                |
| ポーリング間隔            | 5分（設定変更可能）              |
| GitHub認証           | gh CLI                  |
| GitHub API         | gh CLI / GraphQL / REST |
| 状態管理               | SQLite                  |
| 作業ディレクトリ           | Git worktree            |
| OS                 | Windows                 |
| 設定                 | YAML                    |
| 開発指示               | AGENTS.md               |
| 自動検証               | テスト・lint・ビルド・静的解析       |

Windowsタスクスケジューラは使用しない。

初期実装では通常のGoプロセスとして起動する。必要になった場合のみWindowsサービス化を検討する。

## 3. システム構成

```
GitHub Projects
        |
        | gh CLI / GitHub API
        v
Go Polling Controller（常駐）
        |
        +-- Polling Loop
        |      +-- 5分間隔の監視
        |      +-- Status取得
        |      +-- 起動条件判定
        |
        +-- Execution Manager
        |      +-- Conductor起動
        |      +-- プロセス監視
        |      +-- タイムアウト管理
        |
        +-- SQLite
        |      +-- Issue実行状態
        |      +-- 再試行回数
        |      +-- 承認状態
        |      +-- PR・レビュー履歴
        |      +-- 重複起動防止
        |
        +-- Microsoft Conductor
               |
               +-- Design Workflow
               |      +-- Codex CLI
               |
               +-- Implementation Workflow
               |      +-- Codex CLI
               |      +-- テスト・lint
               |      +-- 自己チェック
               |
               +-- PR Review Workflow
               |      +-- コンフリクト確認
               |      +-- Codex CLI
               |      +-- AIレビュー
               |      +-- 修正ループ
               |
               +-- Merge Check Workflow
                      +-- CI確認
                      +-- コンフリクト確認
                      +-- 人間のマージ待ち
```

### 3.1 コンポーネントの責務

**Go Polling Controller**

- 常駐プロセスの管理
- GitHub Projectsの定期監視
- Conductorの起動条件判定
- Conductorプロセスの起動・監視
- SQLiteによる状態永続化
- 同一Issueの二重起動防止
- 中断・異常終了からの復旧

**Microsoft Conductor**

- 各フェーズの処理順序制御
- スクリプトの実行
- 条件分岐
- 修正ループの制御
- ワークフロー結果の出力

**Codex Adapter**

- Codex CLIの非対話実行
- プロンプトと作業ディレクトリの指定
- 終了コード・出力内容の取得
- タイムアウト・エラー処理
- 実行結果の構造化

**GitHub**

- Issue・PRの管理
- Projectsの進捗管理
- 設計承認・レビューコメントの記録
- CIによる検証
- マージ権限の制御

## 4. Go Polling Controller

### 4.1 実行方式

Goの`time.Ticker`を使用し、5分間隔でGitHub Projectsを監視する。

起動直後には初回ポーリングを実行する。

ポーリング間隔は設定ファイルから変更可能とする。

### 4.2 ポーリング処理

1. GitHub ProjectsからIssue ID、Statusなどの判定に必要なメタデータを取得する。
2. SQLiteの実行状態と照合する。
3. `Pending` の前提Issueを照合して条件を満たした項目を `Ready` に更新し、取得済みのStatus・承認・PR状態からワークフローの起動条件を判定する。今回 `Ready` へ移したIssueは次回のポーリングで起動対象にする。
4. 起動が必要なワークフローを選択する。
5. 同一Issueの実行状態を排他的に確保する。
6. Execution Managerへ実行要求を渡す。
7. Conductorを非同期に起動する。
8. 実行状態・結果をSQLiteへ保存する。

### 4.3 Conductor実行との分離

Polling LoopとConductorの実行は分離する。

ConductorによるAI処理が長時間続いても、GitHub Projectsの監視を継続できるようにする。

Execution Managerは、起動したプロセスを追跡し、終了コード、実行時間、エラーを管理する。

同一Issueでは複数ワークフローを同時実行しない。

初期実装では、システム全体の同時実行数も設定可能とし、既定値を1とする。

### 4.4 起動・終了

- 通常のGo実行ファイルとして起動する。
- Windowsタスクスケジューラは使用しない。
- Ctrl+Cなどの終了要求を受け付ける。
- 終了要求後は新規ワークフローの起動を停止する。
- 実行中の処理は安全な停止または状態保存を行う。
- 次回起動時にSQLiteとGitHubの状態を照合する。

初期実装ではOS起動時の自動起動を必須としない。

将来、常時運用が必要になった場合はWindowsサービス化を検討する。

### 4.5 GitHub CLIの認証と起動前確認

Polling Controllerを実行するWindowsユーザーのPowerShellで、GitHub CLIの認証状態を確認する。

```powershell
gh auth status --hostname github.com
```

未認証の場合は、GitHub Projectsの読み取り・更新に使う `project` スコープを指定し、ブラウザーで認証する。

```powershell
gh auth login --hostname github.com --web --scopes project
```

すでにブラウザー認証済みで、保存済みの認証情報に `project` スコープが不足している場合は、権限を追加する。

```powershell
gh auth refresh --hostname github.com --scopes project
```

認証または更新後は `gh auth status --hostname github.com` を再実行し、[Projectの準備](#52-projectの準備)で対象へのアクセスを確認する。

`Access is denied` などで `gh` の設定ファイルを読めない場合は、実行ユーザーと設定ディレクトリのアクセス権を確認してから `gh auth status` を再実行する。認証・権限エラーが解消するまで新規ワークフローの起動を停止し、復旧コマンドをログに示す。ログに認証情報は含めない。 [gh auth status](https://cli.github.com/manual/gh_auth_status) · [gh auth login](https://cli.github.com/manual/gh_auth_login) · [gh auth refresh](https://cli.github.com/manual/gh_auth_refresh) · [gh project](https://cli.github.com/manual/gh_project)

## 5. GitHub ProjectsのStatus

### 5.1 Statusの定義

| Status        | 意味            |
| ------------- | ------------- |
| Backlog       | 未着手           |
| Pending       | 前提Issueの完了待ち   |
| Ready         | AI処理開始待ち      |
| Designing     | AI設計中         |
| Design Review | 人間の設計承認待ち     |
| Implementing  | AI実装・検証・自己修正  |
| PR Review     | AIによるPRレビュー   |
| PR Fixing     | レビュー指摘のAI修正   |
| Merge Ready   | 人間による最終確認待ち   |
| Blocked       | 自動処理停止・人間対応待ち |
| Done          | PRマージ完了       |

Statusは進捗管理に使用する。

実行ID、修正回数、承認済み設計書、レビュー済みコミットなどの内部状態はSQLiteで管理する。

Statusのみを根拠として同一ワークフローを繰り返し起動しない。

### 5.2 Projectの準備

運用予定の所有者のProjectを一覧表示し、Irisyncに利用できるものがあるか確認する。既存Projectの所有者がリポジトリの所有者と異なる場合は `$projectOwner` を置き換える。

```powershell
$projectOwner = 'coco-papiyon'
gh project list --owner $projectOwner
```

利用できるProjectがない場合は作成する。既存Projectを使う場合はその番号を控える。 [gh project list](https://cli.github.com/manual/gh_project_list) · [gh project create](https://cli.github.com/manual/gh_project_create)

```powershell
gh project create --owner $projectOwner --title Irisync --format json
```

既存または新規のProjectの番号を入力し、詳細とフィールドを確認する。Projectとリポジトリの所有者が同じで、リンクが必要な場合に `gh project link` を実行する。 [gh project view](https://cli.github.com/manual/gh_project_view) · [gh project field-list](https://cli.github.com/manual/gh_project_field-list) · [gh project link](https://cli.github.com/manual/gh_project_link)

```powershell
$projectNumber = Read-Host '対象Projectの番号'
gh project view $projectNumber --owner $projectOwner --format json
gh project field-list $projectNumber --owner $projectOwner --format json
# Projectとリポジトリの所有者が同じで、リンクが必要な場合
gh project link $projectNumber --owner $projectOwner --repo coco-papiyon/irisync
```

### 5.3 Statusの追加と修正

1. GitHubのProjectで **Settings → Fields → Status** を開き、[Statusの定義](#51-statusの定義)の11項目と照合する。既存の `Status` がある場合は不足する選択肢を追加し、名称を修正する場合は既存カード・ビュー・Workflowsへの影響を確認してから保存する。移行対象のカードは、意味が対応する選択肢へ移す。 [GitHub Docs の単一選択フィールド](https://docs.github.com/en/issues/planning-and-tracking-with-projects/understanding-fields/about-single-select-fields)
2. `Status` フィールド自体がない場合は、単一選択フィールドを作成する。 [gh project field-create](https://cli.github.com/manual/gh_project_field-create)

   ```powershell
   gh project field-create $projectNumber --owner $projectOwner --name Status --data-type SINGLE_SELECT --single-select-options 'Backlog,Pending,Ready,Designing,Design Review,Implementing,PR Review,PR Fixing,Merge Ready,Blocked,Done'
   ```

3. 設定後に `gh project field-list` を再実行し、`Status` フィールドと11選択肢のIDを取得する。Projectの既定値や組み込みWorkflowsも確認し、Issueの追加やクローズだけで `Ready` や `Done` に移行する設定があれば修正する。 [GitHub Docs の組み込み自動化](https://docs.github.com/en/issues/planning-and-tracking-with-projects/automating-your-project/using-the-built-in-automations)
4. 検証用IssueをProjectに追加し、Polling Controllerの実行アカウントから `Backlog` → `Ready` に更新できることを確認する。`$projectId` は `gh project view`、`$itemId` は `gh project item-list`、`$statusFieldId` と `$readyOptionId` は `gh project field-list` のJSON出力から取得する。 [gh project item-list](https://cli.github.com/manual/gh_project_item-list) · [gh project item-edit](https://cli.github.com/manual/gh_project_item-edit)

   ```powershell
   gh project item-edit --project-id $projectId --id $itemId --field-id $statusFieldId --single-select-option-id $readyOptionId
   ```

### 5.4 前提IssueとPendingの判定

Issueを起票・更新する人は、前提がある場合に本文の「先行Issue」欄へ前提Issueの番号またはURLをすべて記載し、同じIssueをGitHub標準の `Blocked by` に登録する。前提が揃い次第着手するIssueはProjectで `Pending` にする。 [GitHubのIssue依存関係](https://docs.github.com/en/issues/tracking-your-work-with-issues/using-issues/creating-issue-dependencies)

Go Polling Controllerは、起動時と定期ポーリング時に、対象リポジトリの未クローズかつ未アーカイブの `Pending` IssueだけについてGraphQLで `blockedBy` のIssue IDをまとめて取得する。Issue本文とAIを使わずに判定する。登録された前提が1件以上あり、その全件について同じProjectで `Done` を確認できた場合に `Ready` へ更新する。アーカイブされた前提は、同じProjectのStatusを個別に照合する。前提Issueのクローズ自体は完了条件に使わない。

前提が1件も登録されていない場合や、登録済みの前提にProject未登録、閲覧不可、取得エラー、未完了がある場合は `Pending` を維持する。`Backlog` と `Blocked` は運用者が扱う。`Ready` に移ったIssueのConductor起動には、既存の対象Issue設定を適用する。

## 6. 開発ワークフロー

### 6.1 設計フェーズ

**開始条件:** Status = Ready

1. 対象Issueを確保する。
2. Statusを`Designing`へ変更する。
3. Codex CLIでIssueと既存設計書を分析する。
4. AIが設計書・実装計画を作成する。
5. `docs/design/issues/issue-{number}.md`に保存する。
6. 設計結果をGitHubへ反映する。
7. Statusを`Design Review`へ変更する。
8. 人間の承認を待つ。

承認された設計書のコミットSHAを記録する。

設計が承認されるまで実装を開始しない。

承認待機中はConductorを終了し、Go Polling Controllerが承認状態を監視する。

### 6.2 実装フェーズ

**開始条件:** 設計承認済み

1. 作業ブランチとGit worktreeを作成する。
2. 承認済み設計書を読み込む。
3. Codex CLIでコードを実装する。
4. 必要なテストコードを追加する。
5. ビルド・lint・静的解析・テストを実行する。
6. 機械的な検証が成功した場合、AIによる自己チェックを行う。
7. 問題があればCodex CLIで修正する。
8. 修正後は検証を再実行する。
9. 成功時はPRを作成する。

**修正上限は初回実装を除き最大3回とする。**

3回の修正後も問題が解決しなければ`Blocked`へ移行する。

### 6.3 PRレビュー・修正フェーズ

**開始条件:** PR作成済み

1. PRのコンフリクト状態を確認する。
2. コンフリクトがあればCodex CLIで解消する。
3. コンフリクト解消後にテストを実行する。
4. 独立したCodexセッションでPRレビューを実施する。
5. レビュー指摘をGitHub PRへコメント投稿する。
6. 修正が必要な指摘をCodex CLIに渡す。
7. Codexがコードを修正する。
8. 同じPRへコミットを追加する。
9. 自動テストとAI再レビューを実行する。
10. 指摘がなくなれば`Merge Ready`へ変更する。

**PRレビュー後のAI修正も最大3回とする。**

実装フェーズの修正回数とは別に管理する。

レビューAIと実装AIは原則として別セッションとする。

コンフリクト解消についても独立した再試行・時間上限を設ける。

### 6.4 人間による最終レビュー

`Merge Ready`のPRを人間が確認する。

人間から修正指摘があった場合は、Codex CLIで修正する。

修正後はテスト・再レビューを実行し、再び人間の確認を受ける。

人間レビュー指摘への自動修正は原則最大3回とし、超過時は`Blocked`へ移行する。

PRのマージは人間のみが実施する。

AIによる自動マージは禁止する。

マージ済みであることをGitHub APIで確認してからStatusを`Done`に変更する。

## 7. Conductorワークフロー構成

以下の4つに分割する。

| ファイル             | 用途                 |
| ---------------- | ------------------ |
| design.yaml      | AI設計               |
| implement.yaml   | AI実装・テスト・自己修正      |
| pr-review\.yaml  | コンフリクト確認・AIレビュー・修正 |
| merge-check.yaml | CI・コンフリクト・マージ状態確認  |

Conductorの各Scriptステップから必要に応じてCodex Adapter、gh CLI、テストスクリプトを呼び出す。

外部コマンドの終了コードや構造化された検証結果によって分岐する。

長期間の人間承認待ちはConductorの常駐プロセスで保持しない。

## 8. Codex CLI連携

### 8.1 実行方式

ConductorのScriptステップからCodex Adapterを実行し、AdapterがCodex CLIを呼び出す。

概念例：

```
Conductor
    |
    v
Codex Adapter
    |
    +-- 入力ファイル作成
    +-- 実行権限・作業場所設定
    +-- codex exec
    +-- 終了コード取得
    +-- 結果JSON生成
    |
    v
Conductorへ結果返却
```

Codex CLIは既存の`codex login`認証を利用する。

ConductorのOpenAI APIプロバイダーは使用しない。

### 8.2 入出力

Codex Adapterは以下を受け取る。

- Issue番号
- 実行フェーズ
- 作業ディレクトリ
- プロンプト
- 承認済み設計書
- 必要なコンテキスト
- タイムアウト
- 実行権限制約

結果として以下を返す。

- 実行成功・失敗
- 終了コード
- 構造化された結果
- 変更ファイル一覧
- エラー概要
- 実行ログの保存先
- トークン使用量（取得可能な場合）

JSONなどの機械可読形式で結果を保存し、Conductor側で判定できるようにする。

Codex CLIが対応する実際のオプションは、実装時点のバージョンで確認する。

## 9. SQLite状態管理

最低限、以下の情報を管理する。

| データ                   | 内容                 |
| --------------------- | ------------------ |
| Issue ID              | 対象Issue            |
| Project Item ID       | Projectsアイテム識別子    |
| Workflow Run ID       | 実行識別子              |
| Phase                 | 現在の処理フェーズ          |
| Status                | GitHub Projectsの状態 |
| Branch                | 作業ブランチ             |
| PR Number             | 対象PR               |
| Commit SHA            | 対象コミット             |
| Approved Design SHA   | 承認済み設計書の版          |
| Implementation Retry  | 実装修正回数             |
| PR Review Retry       | PR修正回数             |
| Human Review Retry    | 人間指摘への修正回数         |
| Processed Comment IDs | 処理済みレビュー           |
| Execution Result      | 実行結果               |
| Last Error            | 最終エラー              |
| Started / Finished    | 実行時刻               |
| Token Usage           | 取得可能な利用量           |

### 二重実行防止

同一Issue・同一フェーズの重複実行を禁止する。

SQLiteのトランザクションと一意制約等を用いて、実行対象の確保を原子的に行う。

Conductorのプロセスが終了しても状態を失わないようにする。

実行状態の照合時には、GitHub上のPRやコミットの状況も確認する。

## 10. AI利用コストの制御

### AIを使用する処理

- 要件分析
- 設計作成
- コード実装
- 複雑な不具合修正
- AI自己チェック
- PRレビュー
- レビュー指摘修正
- 複雑なコンフリクト修正

### AIを使用しない処理

- GitHub Projectsの監視
- Status変更
- GitHub API操作
- ブランチ・worktree作成
- ビルド
- lint・静的解析
- テスト
- コンフリクトの有無の判定
- 再試行回数判定
- SQLite更新
- ログ収集
- ワークフローの条件分岐

### 費用削減方針

- AI自己チェックより先に機械的な検証を実行する。
- 必要な差分やログだけをAIに渡す。
- AIによる修正回数に上限を設ける。
- 同一エラーの反復を検出する。
- Issue単位で実行時間・予算上限を設定可能にする。
- 利用量を取得できない場合は未取得として扱う。
- 不要なAIレビューの再実行を防ぐ。

## 11. リポジトリ内の文書構成

```
repository/
├── AGENTS.md
├── docs/
│   ├── architecture.md
│   ├── development.md
│   ├── coding-standards.md
│   ├── review-guidelines.md
│   ├── security-policy.md
│   └── design/
│       ├── basic-design.md
│       ├── conductor-feasibility-study.md
│       ├── implementation-plan.md
│       └── issues/
│           └── issue-{number}.md
├── .conductor/
│   ├── workflows/
│   │   ├── design.yaml
│   │   ├── implement.yaml
│   │   ├── pr-review.yaml
│   │   └── merge-check.yaml
│   └── scripts/
├── .github/
│   ├── ISSUE_TEMPLATE/
│   ├── pull_request_template.md
│   └── workflows/
└── ...
```

`AGENTS.md`には共通指示と参照すべき文書を記載する。

詳細なアーキテクチャ、レビュー基準、セキュリティ規則は別文書で管理する。

AIは実行フェーズに必要な文書を明示的に参照する。

## 12. セキュリティ方針

- AIによる自動マージを禁止する。
- 保護ブランチへの直接pushを禁止する。
- Codexは専用worktree内で実行する。
- GitHub管理用認証とAI実行環境の権限を可能な限り分離する。
- Issue本文やPRコメントを信頼できない入力として扱う。
- シークレットをプロンプトやログに記録しない。
- 強制push、破壊的操作、外部公開操作を制限する。
- 各外部プロセスにタイムアウトを設定する。
- 承認済み設計の大幅変更は再承認を必要とする。
- GitHubのRulesetsやBranch protectionを利用してマージ条件を強制する。

AIへの禁止指示だけでなく、OS権限・サンドボックス・GitHub権限による実際の制御を実施する。

## 13. 障害・復旧

| 障害              | 対応                  |
| --------------- | ------------------- |
| GitHub API通信エラー | 次回ポーリングで再試行         |
| gh CLI認証・権限エラー | ログ記録・新規起動停止・[認証と起動前確認](#45-github-cliの認証と起動前確認)を案内 |
| Conductor実行失敗   | 結果保存・再試行判定          |
| Codex CLI実行失敗   | エラー分類・上限内再試行        |
| 修正上限到達          | Blocked             |
| Windows再起動      | SQLite・GitHubとの状態照合 |
| Goプロセス異常終了      | 次回起動時に復旧            |
| 承認待ち            | GitHub上で待機          |
| PRコンフリクト        | Codexで修正・再テスト       |
| PR更新            | SHA確認・必要な再レビュー      |
| Issue中止・クローズ    | 新規処理停止・後片付け         |

プロセスの異常終了後、過去の実行状態だけを根拠に処理を再開しない。

実際のGitHub状態やGit worktreeを照合し、安全な再開地点を決定する。

## 14. 実装計画

Irisync本体への着手条件と両工程の依存関係は、[Irisync 実装計画](implementation-plan.md)を参照する。

### Phase 1：実行基盤

- Go常駐プロセス
- 5分間隔のポーリング
- gh CLIによるProjects取得・更新
- SQLite状態管理
- 重複起動防止
- Conductor起動・監視
- AI不要のサンプルワークフロー

### Phase 2：Codex連携

- Codex Adapter
- `codex exec`非対話実行
- 結果JSON化
- タイムアウト・エラー処理
- 実行ログの保存

### Phase 3：設計承認

- AI設計書作成
- 設計書の版管理
- GitHub上での承認記録
- 承認後の実装起動

### Phase 4：実装・検証

- Git worktree作成
- AI実装
- 自動テスト
- AI自己チェック
- 最大3回の修正
- PR作成

### Phase 5：PRレビュー

- コンフリクト確認と修正
- AIレビュー
- レビューコメント投稿
- AIによるレビュー指摘修正
- 最大3回の再レビュー
- 人間レビュー・マージ

### Phase 6：運用品質

- 障害復旧
- 費用・トークン集計
- セキュリティ強化
- ログ・状態可視化
- Windowsサービス化の必要性評価

## 15. 今後の検討事項

以下は詳細設計・実装時に確定する。

1. GitHub上の設計承認方法と承認者の識別
2. Conductor Scriptステップの具体的な入出力仕様
3. Codex CLIの権限制御・サンドボックス設定
4. SQLiteテーブル構造
5. コンフリクト修正の再試行上限
6. PRレビュー結果の構造化フォーマット
7. human review後の修正再開条件
8. AI利用量・コストの取得方法
9. Goプロセスの異常終了時の自動再起動方式
10. GitHub APIレート制限時のバックオフ
11. 既存ベース設計書との整合性確認

## 16. 完了条件

初期実装は以下を満たすこと。

- Goプロセスが常駐し、5分間隔でProjectsを監視できる。
- ReadyのIssueからAI設計を開始できる。
- 設計承認前に実装されない。
- Codex CLIによる実装ができる。
- 自動テストと自己チェックが動作する。
- 実装修正回数が最大3回に制限される。
- 検証成功時にPRが作成される。
- コンフリクト修正が実行される。
- AIレビュー結果がGitHubに投稿される。
- PRレビュー指摘をAIが修正できる。
- PRレビュー修正回数が最大3回に制限される。
- 人間のレビュー指摘にもAIが対応できる。
- PRのマージは人間に限定される。
- 二重起動が防止される。
- プロセス再起動後に状態を復旧できる。
- ワークフロー制御そのものに追加のLLM呼び出しを必要としない。

---

## 17. 採用結論

Microsoft Conductorをワークフロー制御基盤として採用する。

GitHub Projectsの監視・実行管理にはGo製の常駐Polling Controllerを使用する。

Polling Controllerは起動直後および5分間隔でGitHub Projectsを確認し、必要なConductorワークフローを起動する。

AI処理はConductorのScriptステップからCodex CLIを呼び出す方式とする。

GitHub認証と操作はgh CLIを使用し、実行状態はSQLiteへ永続化する。

設計承認と最終マージは人間が実施する。

AI処理と機械的な処理を明確に分離し、コスト、安全性、再現性、保守性を重視した開発自動化基盤を構築する。
