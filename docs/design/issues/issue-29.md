# A01 Go CLI と Windows・Linux CI の詳細設計

GitHub Issue [#29](https://github.com/coco-papiyon/irisync/issues/29) の設計。先行する Conductor の試験運用は完了済み。 [C07 の完了記録](https://github.com/coco-papiyon/irisync/issues/28)

## CLI と Go モジュール

- リポジトリのルートにアプリの Go モジュール `github.com/coco-papiyon/irisync` を作り、`cmd/controller` と `cmd/agent` を独立したコマンドとしてビルドする。Conductor Polling Controller は既存のサブモジュールで引き続き管理する。
- `controller --help` と `agent --help` は使用方法を表示し、終了コード `0` を返す。未定義のフラグや余分な位置引数は標準エラーに理由を出し、終了コード `2` を返す。
- `controller` は省略可能な `--config <path>` を受け取る。指定した場合は設定の読み込みが成功すれば `0`、失敗すれば理由を標準エラーに出して `1` で終了する。パスを省略した場合は既定のファイル探索をせず、ネットワークや Web UI を起動せずに `0` で終了する。
- `agent` も引数なしで `0` で終了する。標準出力に起動メッセージを出さず、後続の SSH 上のプロトコルに使えるようにする。

## 設定読み込み

`internal/config` が指定されたファイルを開き、単一の YAML マッピングを読み込む。既存 Poller と同じ `gopkg.in/yaml.v3` のバージョンを使用する。ファイルを開けない場合、空文書、構文不正、ルートがマッピング以外の場合、複数の YAML 文書はエラーとする。空のマッピング `{}` は有効とする。エラーメッセージには設定値を含めず、ファイル操作の失敗か YAML の失敗かを判断できるようにする。

ペア定義、必須項目と既定値、環境変数による上書きは A02-1 で実装する。SSH 起動と標準入出力での通信は A03-2 で実装する。

## CI と受入条件

- アプリ用の GitHub Actions ジョブを Windows と Linux で実行する。既存 CI と同じ Go バージョンを使い、ルートの Go モジュールに対して `go vet ./...`、`go test ./...`、`go build ./cmd/controller ./cmd/agent` を実行する。両コマンドの起動・終了を CLI のテストで確認する。
- CLI の正常終了、両方のヘルプ、引数エラー、正常な YAML、存在しない設定パス、構文不正、マッピング以外、複数文書をテストする。引数なしで起動して Web UI の起動や追加設定を必要としないことを確認する。
- Go ファイルは `gofmt` を適用する。アプリ CI と既存 Conductor CI は別ワークフローとして動作させる。

基本設計の [システム構成](../basic-design.md#2-システム構成) と [実装計画](../implementation-plan.md#21-実装順序) に従う。
