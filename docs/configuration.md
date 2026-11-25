# コントローラーの設定

`controller --config config.yaml` は YAML 設定を読み込み、全ペアを検証します。設定ファイルは単一の YAML マッピングです。`{}` と `pairs: []` は有効です。既存設定との互換性のため未知のトップレベル項目は読み飛ばし、認識するセクション内の未知の項目はエラーにします。YAML のアンカーとエイリアスは使えます。設定セクションでのマージキー `<<` は使えません。

```yaml
pairs:
  - id: documents
    startup: restore
    local:
      path: /srv/documents
    remote:
      path: /data/shared/documents
      ssh:
        host: irisync-backup
        use_ssh_config: true
    exclude:
      - "*.tmp"
      - ".git/**"
    sync:
      max_file_size: 500MiB
```

Windows コントローラーでは `local.path` に `C:/data/documents` のような Windows の絶対パスを使います。

## ペア

- `id`、`local.path`、`remote.path`、`remote.ssh.host` は必須です。`id` は大文字・小文字を区別する一意の文字列です。
- `startup` は `start`（開始要求）、`stop`（停止）、`restore`（保存した希望状態を復元）から選びます。既定値は `restore` です。履歴がない場合の停止は後続の状態管理が処理します。
- `remote.ssh.use_ssh_config` の既定値は `true` です。この場合、`host` は OpenSSH のホストエイリアスです。`false` の場合は `host` にホスト名か IP アドレスを指定し、`user` を指定します。IPv6 アドレスは角括弧なしで指定します。`port` は 1〜65535 で既定値は 22 です。`identity_file` には必要に応じて鍵ファイルのパスを指定できます。
- 同じ SSH 接続先にドライブ付きパス（例 `D:/shared`）または先頭の区切りにバックスラッシュを含む UNC パス（例 `\\server\share\data`）があれば、その接続先のすべての `remote.path` を Windows のパスとして検証します。各ルートにドライブ付き絶対パスまたは UNC パスを指定してください。`/data` のようにドライブや共有名を省略したパスは拒否します。`//server/share/data` はこの場合に UNC として扱います。Windows 形式を示すルートがなければ、すべて POSIX の絶対パスとして扱います。
- Windows の同期ルートでは、`NUL`、`COM1`、`LPT1` などの予約デバイス名をパス要素に使えません。拡張子が付いた `NUL.txt` なども拒否します。
- パスには日本語や、空白、括弧、`&`、`+` など OS が許可する記号を使えます。YAML では `'C:/資料（共有）/報告 [Q1] & 集計'` のように引用符で囲めます。
- ローカル側は全ペアを比較します。リモート側は同じ SSH エイリアス、または直接指定したホストとポートの組み合わせが同じ場合に比較します。ホスト名の大文字・小文字の差と IP アドレスの表記差は正規化します。パスは要素単位で比較し、同一または親子関係のルートを拒否します。Windows の静的比較では区切り文字と ASCII の英字 `A–Z`・`a–z` の大小を同一視します。それ以外の文字は記述どおりに比較します。Unicode 正規化やファイルシステム固有の大小文字の扱い、SSH の別名、シンボリックリンクを含む実体の照合は起動検査に引き継ぎます。
- `exclude` はペアのルートからの相対 glob のリストです。既定値は空です。区切りは `/` で、大文字・小文字を区別します。各パス要素は Go の `path.Match` 構文を使い、要素全体が `**` の場合は 0 個以上のパス要素に一致します。例えば `.git/**` はルート直下の `.git` ディレクトリ以下、`**/*.tmp` は任意の階層の一時ファイルを表します。
- `conflict.priority` は `local` または `remote` で、既定値は `local` です。

## 全体設定

既定値、YAML、環境変数の順で適用します。明示した空値、型違い、無効な値は設定エラーになります。

| YAML 項目 | 既定値 | 環境変数 |
| --- | --- | --- |
| `server.http.listen` | `127.0.0.1:8080` | `IRISYNC_HTTP_LISTEN` |
| `sync.min_delay` | `60s` | |
| `sync.idle_delay` | `5s` | |
| `sync.max_delay` | `120s` | |
| `sync.max_file_size` | `1GiB` | `IRISYNC_MAX_FILE_SIZE` |
| `sync.full_scan_interval` | `30m`（暫定） | `IRISYNC_FULL_SCAN_INTERVAL` |
| `sync.retry_interval` | `5s`（暫定） | |
| `performance.max_concurrent_pairs` | `5`（暫定） | `IRISYNC_MAX_CONCURRENT_PAIRS` |
| `performance.max_transfers_per_pair` | `2`（暫定） | |

時間は `60s` や `30m` のような単位付きの正の値です。`sync.min_delay` は `sync.max_delay` 以下にします。同時実行数は正の整数です。サイズはバイト数、または単位 `B`、`KiB`、`MiB`、`GiB`、`TiB` 付きの非負整数です。`0` は無制限を意味します。ペアの `sync.max_file_size` は、明示した `0` も含めて確定済みの全体値より優先します。
