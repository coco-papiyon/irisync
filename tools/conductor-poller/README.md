# Irisync Conductor Poller の使い方

Conductor Poller は、Windows 上で [Irisync Project #4](https://github.com/users/coco-papiyon/projects/4) を監視し、Issue の設計、実装、実装 PR のレビューを進めるツールです。

## 設計承認と PR マージ

| 場面 | 人間が行う操作 |
| --- | --- |
| Poller が Issue を `Design Review` にした | Issue に投稿された設計書を読み、**同じ Issue に `/approve-design` を新規コメントとして投稿**する。設計ブランチのマージは不要。 |
| Poller が実装 PR を作成し、Issue が `Merge Ready` になった | **実装 PR** の差分と CI を確認し、GitHub でマージする。Poller が次回の確認で Issue を `Done` に進める。 |
| 人間が別途作成した設計 PR を使う | 設計 PR をレビューしてマージし、その設計コミットを承認済みとして手動実装に進む。PR のマージは Poller の `/approve-design` 承認としては記録されない。 |

たとえば [#29 の設計 PR #49](https://github.com/coco-papiyon/irisync/pull/49) は、手動実装に進むための設計 PR です。自動運用なら、[Issue #29](https://github.com/coco-papiyon/irisync/issues/29) に Poller が投稿した最新の承認依頼を使います。

## 起動する

Windows の PowerShell 7 で、リポジトリのルートから実行します。Go、Git、GitHub CLI、Conductor CLI、Codex CLI を用意してください。 [開発環境のセットアップ](../../docs/development.md)

```powershell
gh api user --jq '.login'
gh project view 4 --owner coco-papiyon --format json --jq '.title'
codex login status

if (-not (Test-Path .conductor/poller.local.yaml)) {
    Copy-Item .conductor/poller.example.yaml .conductor/poller.local.yaml
}
.\tools\conductor-poller\build.ps1
```

ビルドスクリプトは `.conductor/.local/poller.exe` を作成します。再ビルドする場合は、実行中の Poller を `Ctrl+C` で停止してから実行します。

### GitHub 認証を選ぶ

Project は URL の `/projects/` に続く番号を `project_number` に指定します。Irisync Project #4 なら `project_number: 4` です。Project の所有者は既定で `repository` の所有者です。別のユーザーや組織が所有する場合は `project_owner` にそのログイン名を指定します。既存設定で `project_id` を使っている場合は、そのまま使用するか `project_number` に置き換えます。 [Irisync Project #4](https://github.com/users/coco-papiyon/projects/4)

| `github_auth_mode` | 設定 |
| --- | --- |
| `gh`（既定） | `github_login` に `gh api user --jq '.login'` の結果を設定する。その認証でリポジトリと Project の両方を操作する。 |
| `app` | `github_app_id` と `github_app_key_path` を指定し、Project 用の `github_project_login` を設定する。Poller が対象リポジトリに限定した App トークンを発行・更新する。 |

App モードでは、ソースの fetch・push、Issue の取得とコメント、PR の作成とレビュー操作を bot が行います。Project の一覧取得、先行 Issue の Project ステータス参照、ステータス更新には、`github_project_login` に指定した `gh` アカウントの保存済み認証を使います。

```yaml
project_number: 4
github_auth_mode: app
github_login: coco-papiyon-developper
github_app_id: 'YOUR_APP_ID_OR_CLIENT_ID'
github_app_key_path: 'C:\keys\irisync-app.private-key.pem'
github_project_login: coco-papiyon
```

`github_app_id` には App ID または Client ID を指定します。RSA 秘密鍵の PEM ファイルはリポジトリの外に置き、`github_app_key_path` にパスを指定します。相対パスは設定ファイルの位置を基準に解決されます。App を対象リポジトリにインストールし、**Contents、Issues、Pull requests を Read and write** にします。非公開リポジトリのチェック結果を読む場合は **Checks を Read** にします。実装で `.github/workflows/` を更新する場合は **Workflows を Read and write** にします。 [GitHub App の権限](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/choosing-permissions-for-a-github-app)

Project を `gh` で操作するアカウントには `project` スコープと Project への書き込み権限が必要です。対象アカウントを有効にして `gh auth refresh --hostname github.com --scopes project` を実行します。認証モードを変更したら Poller を再起動します。Bot に切り替える前に承認済みだった設計は、bot が Issue に投稿する承認依頼に対して承認し直してください。

設定を確認してから起動します。`-once` を付けると1回だけ確認します。常駐時は `-once` を外し、停止は `Ctrl+C` です。設定を変更したら再起動します。

```powershell
.\.conductor\.local\poller.exe -config .conductor/poller.local.yaml -once
```

ローカルのスモークテストだけを実行するには、Poller の起動の代わりに `.\.conductor\scripts\test-smoke.ps1` を実行します。Poller は対象 Issue のリストが空でも、先行 Issue がそろった `Pending` を `Ready` に更新します。

## Web ダッシュボード

ダッシュボードだけを起動するには、リポジトリのルートで次を実行し、起動時に表示される URL を開きます。既定は [http://127.0.0.1:8765](http://127.0.0.1:8765) です。

```powershell
.\.conductor\.local\poller.exe -config .conductor/poller.local.yaml -dashboard-only
```

`-dashboard-only` は SQLite を読み取り専用で開きます。初回は通常の Poller 起動で状態ファイルが作成された後に使えます。ダッシュボードから投稿した設計承認を処理して実装に進むには、通常の Poller が稼働している必要があります。

Poller を動かしながら画面も開く場合は `poller.local.yaml` に `dashboard_addr: '127.0.0.1:8765'` を追加し、通常どおり起動します。一度に使うビューアーは1つとし、同じポートを使う場合は先に別のビューアーを `Ctrl+C` で停止します。ダッシュボードはループバック IP だけで待ち受けます。

- Issue 一覧にはオープンな Issue の Project ステータスを表示し、番号と件名で検索できます。
- PR 一覧にはオープンな PR を更新順に最大50件表示します。CI・レビュー状態も確認できます。
- Issue や PR の件名をクリックすると、GitHub 上のコメント、PR のレビュー、関連する実行結果を表示します。設計書やコメントの原文はリンクから GitHub で開けます。
- Issue が `Design Review` の場合は **設計を承認** で、表示中の設計版を指定した承認コメントを Issue に投稿します。
- オープンでレビュー可能な PR の **PR を承認** は、表示中のコミットに GitHub の承認レビューを投稿します。マージは GitHub で行います。
- 実行結果にはスモークテスト、設計や実装の記録、レビュー結果を表示します。ログがある行の **開く** から、ログの末尾を最大128 KiB 読み取れます。

承認者の数値 ID を `design_approver_ids` に登録します。ダッシュボードは、Poller を起動する Windows ユーザーで保存済みの `gh` 認証を使います。未保存の場合は、承認者のアカウントで `gh auth login --hostname github.com --web` を実行します。承認には、設計承認依頼の投稿者、または PR の作成者とは別の人間のアカウントを使います。

- `app` モードで `dashboard_approver_login` が空欄の場合は、`github_project_login` の保存済み認証を承認に使います。別の承認者を使う場合は `dashboard_approver_login` にログイン名を指定します。
- `gh` モードで `dashboard_approver_login` が空欄の場合は、`github_login` の保存済み認証を承認に使います。そのアカウントで設計承認依頼や PR を作成した場合は、`dashboard_approver_login` に別の承認者のログイン名を指定します。

ダッシュボード右上の **設定** から、承認者の数値 ID とログイン名、ポーリング間隔、Ready の自動処理、テスト・設計・実装の対象 Issue 番号を変更できます。保存先はダッシュボード起動時に `-config` で指定した YAML ファイルです。承認者の変更はダッシュボードにすぐ反映され、稼働中の Poller の処理には Poller の再起動後に反映されます。

## 自動で設計・実装する

通常運用では `poller.local.yaml` に次の設定を追加します。これで、Issue ごとに設計・実装の番号を設定する必要がなくなります。

```yaml
auto_ready: true
```

1. `design_approver_ids` に人間の承認者の GitHub 数値 ID を登録し、Poller を再起動します。ID は `gh api users/ログイン名 --jq '.id'` で確認できます。`test_issue_numbers` の Issue はスモークテスト専用です。
2. Project #4 で Issue を `Ready` にします。前提がある Issue は本文の「先行Issue」と GitHub の `Blocked by` に同じ先行 Issue を登録し、`Pending` にします。すべての先行 Issue が同じ Project で `Done` になると、Poller が `Ready` に進めます。
3. `Design Review` になったら、Issue にある最新の承認依頼から設計書を開きます。承認者が `/approve-design` だけを1行で新規コメントとして投稿します。Poller は、最新の承認依頼より後に投稿されたコメントを、その依頼の設計コミットに結び付けます。設計が更新されたら、最新の依頼を確認して承認し直します。
4. 次のポーリングで承認を検知すると、Issue は `Implementing` に進みます。実装 PR が作成されたら、修正依頼は PR の **Request changes** で投稿します。CI とレビューを通って `Merge Ready` になったら、登録済みの人間の承認者が実装 PR をマージします。

対象は設定したリポジトリと Project の Issue です。設計・実装途中の Issue は Poller がローカル状態に記録したものを引き継ぎます。試運転や設計だけの実行には `auto_ready: false` を使い、`design_issue_numbers` と `implementation_issue_numbers` で対象を個別指定します。設定例では `auto_ready` を無効にしています。

設計承認、修正依頼、実装 PR のマージは、リポジトリ操作用とは別の、`design_approver_ids` に登録した人間のアカウントで行います。App モードでは、Project に使う人間のアカウントを承認者に登録できます。

## `Design Review` から進まないとき

| 確認すること | 対応 |
| --- | --- |
| コメントを編集した、または古い設計を承認した | 最新の承認依頼と設計書を確認し、`/approve-design` を新規コメントとして投稿する。 |
| コメントしたアカウントが設計承認依頼の投稿者と同じ、または数値 ID が未登録 | 承認依頼の投稿者とは別の人間のアカウントを `design_approver_ids` に登録し、そのアカウントで承認する。 |
| `auto_ready` が無効で、`implementation_issue_numbers` に対象 Issue が含まれていない | 指定方式で自動実装する場合は対象番号を追加して Poller を再起動する。設計だけを行う場合は `Design Review` の表示が残る。 |
| `-once` で終了した、または認証エラーが出ている | Poller を再実行し、起動した端末の出力を確認する。実行ログと SQLite は既定で `.conductor/.local/` に保存される。 |
