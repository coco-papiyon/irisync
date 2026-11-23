# Conductor開発環境のセットアップ

## Windowsでの検証

Windowsで動作を確認した構成は、Conductor `0.1.47`、Python `3.12.13`、PowerShell `7.6.10`、Go `1.22.5`、Git `2.46.1.windows.1`、GitHub CLI `2.83.2`、Codex CLI `0.171.0-alpha.13`。ConductorはGitHub Actionsと同じ固定コミットを使う。 [Microsoft Conductorの固定コミット](https://github.com/microsoft/conductor/commit/ac6b1f4abe8023a51045f946f67199b0f9fc47c8)

リポジトリのルートからPowerShell 7で検証する。スクリプトは、日本語と空白を含むパスでのScript実行、JSON結果、終了コードによる分岐、タイムアウトを確認する。

```powershell
.\.conductor\scripts\test-smoke.ps1
```

GitHub Actionsの `Conductor smoke (Windows)` も同じスクリプトを実行する。GitHubのトークン権限は `contents: read` で、チェックアウト後のGit操作に認証情報を残さない。

## Projectと認証

Irisyncは `coco-papiyon` が所有する非公開の [Project #4](https://github.com/users/coco-papiyon/projects/4) を使う。`Status` の定義、権限を確認するコマンド、検証用Issueの更新方法は[Conductor検討書](design/conductor-feasibility-study.md#45-github-cliの認証と起動前確認)を参照する。

Projectの所有者は **… → Workflows** を開き、`Item closed`、`Pull request merged`、`Pull request linked to issue`、`Auto-close issue` を無効にする。`Item added to project` は移行先を `Backlog` に設定する。サブIssueの自動追加を使う場合も `Backlog` に入ることを確認する。開始は運用者が `Ready` に変更した時点、完了はPollerが対応するPRのマージを確認した時点とする。 [GitHub Projectsの組み込み自動化](https://docs.github.com/en/issues/planning-and-tracking-with-projects/automating-your-project/using-the-built-in-automations)

リポジトリ所有者は、人間の設計承認・マージに使うアカウントと、Conductor実行用のGitHubアカウントを決める。実行用アカウントは対象リポジトリで作業ブランチをpushしてPRを操作できる権限と、Project #4の **Settings → Manage access → Write** を持たせる。実行用アカウントの認証は、専用実行ユーザーのPowerShellで行う。

```powershell
gh auth login --hostname github.com --web --scopes project
gh api user --hostname github.com --jq '.login'
gh project view 4 --owner coco-papiyon --format json --jq '.title'
```

`gh api user` の結果が実行用アカウントであることを確認する。Codexの子プロセスにGitHub管理用の認証情報を渡さない。 [個人所有Projectのアクセス管理](https://docs.github.com/en/issues/planning-and-tracking-with-projects/managing-your-project/managing-access-to-your-projects)

## ブランチ保護と受入

`main` には、PR必須と強制push禁止を設定済み。CI必須化と人間に限定したマージ権限は、以下の手順で追加する。 [Irisync main pull requests](https://github.com/coco-papiyon/irisync/rules/27849709)

1. このセットアップを作業ブランチからPRにし、WindowsのCIが成功することを確認する。公開リポジトリに含めるファイルは、PR作成前に確認する。
2. `Irisync main pull requests` に `Conductor smoke (Windows)` の成功を必須チェックとして追加する。自動化のPRを受け付ける前に、人間によるレビューを1件以上必須にする。初回の基盤PRが承認者本人の作成であれば、基盤PRを受け入れてからレビュー必須を有効にする。
3. `Restrict updates` は、PR・承認・CIを強制するルールセットとは別に作成する。マージを担当する人間だけに **For pull requests only** の例外を与え、PR・承認・CI側にはこの例外を追加しない。検証用ブランチにも同じルールを適用する。 [GitHub Rulesetsの作成](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/creating-rulesets-for-a-repository) · [利用できるルール](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets)
4. 検証用IssueをProjectに入れる。実行用アカウントで `Backlog` → `Ready` の読み書きができ、保護された検証用ブランチへの直接pushとPRマージが拒否されることを確認する。人間の承認とCI通過後に、人間が同じPRをマージできることを確認する。

## Polling Controllerの試運転

Projectの読み取り、SQLiteへの実行履歴の保存、Conductorのスモークワークフロー起動を試せる。設定例は対象Issueを空にしているため、既存Issueの処理を開始せずにGitHub接続だけを確認できる。

```powershell
Push-Location tools/conductor-poller
go test ./...
go build -o ../../.conductor/.local/poller.exe .
Pop-Location
.\.conductor\.local\poller.exe -config .conductor/poller.example.yaml -once
```

Conductorまで試すときは `.conductor/poller.example.yaml` を `.conductor/poller.local.yaml` にコピーし、`test_issue_numbers` に検証専用Issueの番号を入れ、ProjectでそのIssueを `Ready` にする。起動時の1回だけ確認する場合は `-once` を指定する。常駐時は `-once` を外し、Ctrl+Cで停止する。SQLiteと各実行のログは `.conductor/.local/` に保存する。

再起動時は中断した実行を記録し、GitHubで対象Issueが引き続き `Ready` の場合にだけ上限内で再試行する。スモークワークフローの実行によるProjectのStatus更新やPR作成は行わない。

## Codexによる設計と承認

Conductorを実行するWindowsユーザーで `codex login status` を確認する。`poller.local.yaml` の `design_issue_numbers` に設計対象のIssue番号を入れ、`design_approver_ids` に承認者のGitHub数値IDを設定する。`gh api users/coco-papiyon --jq .id` で所有者のIDを確認できる。対象IssueはProjectの `Ready` に移す。設定例の空リストでは、Issue情報のCodex送信やGitHubへの書き込みは発生しない。

Polling Controllerは、Issue情報とローカルの設計文書をCodex CLIに渡してMarkdownを生成する。実行時はレビュー済みブランチのチェックアウトを使い、Issue本文には設計に必要な公開情報だけを記載する。設計書は `conductor/design/issue-N` ブランチの `docs/design/issues/issue-N.md` に保存され、Issueにコミット固定リンクと承認コマンドが表示される。

承認者はリンク先を確認し、Issueに表示された `/approve-design コミットSHA` を1行で新規コメントとして投稿する。設計コミットが変更されたら、新しい承認リクエストに対して承認する。Codexの処理は `Design Review` に移ると終了し、Polling Controllerが承認者・コメントID・SHAをSQLiteに記録する。

## 承認済み設計の実装とPR作成

自動実装を許可するIssueを `poller.local.yaml` の `implementation_issue_numbers` に追加する。ここに指定する番号は `design_issue_numbers` にも含める。空リストのままでは設計承認後も実装は開始しない。

Polling Controllerは承認コメントと設計SHAを再確認し、`main` のコミットを基点にIssue専用worktreeを作る。Codexが読み取り専用で生成した差分をGitで確認して適用する。実装後は書式と差分を確認し、WindowsサンドボックスでGoの静的解析・テスト・ビルドとConductorのスモークテストを実行する。Goの依存モジュールは事前に実行環境へ取得する。機械検証の後にCodexが差分を自己チェックし、すべて通過したGitツリーをコミットする。

初回実装後の修正は最大3回。解決できなければ `Blocked` に移る。通過時だけIssue専用ブランチをpushして `main` 向けPRを作り、Projectを `PR Review` に移す。中断後は同じ作業ブランチのPRとコミットを照合する。ログとworktreeは `data_dir` 以下に残る。

## PRレビューとマージ確認

`implementation_issue_numbers` にあるIssueは、PR作成後も監視する。独立したCodexセッションがPRのHEADとベースの差分をレビューし、同じコミットの結果をPRに投稿する。修正時は専用worktreeで差分を適用し、検証と自己チェックを通過したコミットを既存PRのブランチに追加する。新しいHEADはもう一度、独立レビューとWindows CIを通す。

人間が修正を依頼する場合は、`design_approver_ids` に登録されたアカウントで、現在のPRに **Request changes** としてレビューを投稿する。本文とそのレビューの行コメントを修正に使う。AIレビューと人間レビューの修正上限はそれぞれ最大3回。コンフリクト解消は別に最大2回とし、1回あたりの上限は15分または `implementation_timeout` の短い方とする。

コンフリクトがなく、現在のHEADの独立レビューと `Conductor smoke (Windows)` が成功したら `Merge Ready` に移る。人間がPRをレビューしてマージする。Polling ControllerはGitHubが返すマージ結果と承認者IDを照合し、確認できた場合にIssueを `Done` にする。
