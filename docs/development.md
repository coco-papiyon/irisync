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
