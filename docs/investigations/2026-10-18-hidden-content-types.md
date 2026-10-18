# 分卷压缩包与其它未显示内容的排查

对应需求："进入文件夹后不显示 7z 分卷压缩包，排查原因并排查是否还有其他未显示类型的内容，仅排查不修复。"

结论先行：**这条数据通路上不存在任何按名字或扩展名过滤的逻辑。** `a.7z.001` 与 `a.txt` 被同等对待。所以"7z 分卷不显示"不是被过滤掉了，而是**在某一层丢了**。排查用进程内假 provider 逐层验证，凡是能用代码证伪的假设都已证伪并剔除。

## 已证伪：这些都不是原因

用假的 `/file/sort` → 真实 `quarkapi.Client` → 真实 `adapter.Service` → 真实 `dav.Server`，按 rclone 实际发送的 `standardProps` 发 PROPFIND：

- 21 个探针条目全部到达 PROPFIND（root + 21 子项）。`a.7z.001`、`a.7z.002`、`b.7z`、`c.rar`、`c.r00`、`d.zip`、`e.tar.gz`、`f.001`（无归档扩展名的裸分卷）、`rock & roll.mp4`、`100% done.bin`、`issue #3.txt`、`a+b.txt`、`trailingdot.`、217 字符长名——**全部存在、类型正确**，带 `getcontentlength` 与空的 `resourcetype`。
- `&` 正确往返：原始 `rock &amp; roll 2.mp4` → `rock & roll 2.mp4` → href `rock%20&amp;%20roll%202.mp4`。

逐层证伪清单：

| 位置 | 结论 |
|---|---|
| `dav/fs.go` `Readdir` 分片 | 无丢失，`n<=0` 全量返回，x/net 正是用 `Readdir(0)` |
| `dav/fs.go` 子项排序 | 无影响 |
| x/net `props()` 取 `getcontentlength` | 目录上该属性 404 不影响，rclone 的 `StatusOK()` 接受任意 2xx |
| rclone `webdav.go` `HasPrefix(u.Path, baseURL.Path)` | 无影响，href 均为 `/dav/…` |
| rclone `path.Join`/`TrimSuffix` | 除下述 `/` 情形外无额外丢失 |
| rclone `StatusOK()` | 无影响 |
| `ExcludeShares`/`ExcludeMounts` | **默认关闭** |
| rclone `Enc` 编码器 | `vendor=other` 下保持 `EncodeZero`，只有 `sharepoint-ntlm` 才强制编码 |
| rclone `vfs/dir.go` | 无按名过滤，仅跳过 `""`/`.`/`..` |
| `cmd/cmount`（Windows 实际路径） | 从不设置隐藏/系统属性；`Namemax=255`，217 字符名通过 |
| `supervisor` 的 rclone 参数 | 无 `--include`/`--exclude` |

## 已证实：四种确实会丢内容的机制

1. **一个字段类型不对，整页全丢。** `size: "12345"`（字符串而非数字）→ `List` 返回 0 条并报 `json: cannot unmarshal string into Go struct field rawFile.data.list.size of type int64`。不是丢那一条，是**整页全丢**；PROPFIND 变成裸 500，文件夹看起来是空的。同理 `size: 1.5`、`category: "1"`、`file: "true"`、`file: 1`、`fid: 123`、`created_at: "2024"`。`size: null`、`file: null`、`file_name: null` 反而能容忍。位置：`quarkapi/client.go` `decodeEnvelope` 的 `json.Unmarshal(raw, result)`。
2. **`file` 字段缺失 → 被当成目录。** `{"fid":"x","file_name":"a.bin","size":7}` 返回 `DIR`。`rawFile.File` 默认 `false` → `KindDir`（`quarkapi/files.go` 的 `toFile`）。一个分卷若因此变成目录，在文件列表里就"看不见"了。
3. **同名条目塌缩成一个。** 两个 `dup.txt` → 只留最后一个（`dav/fs.go` 的 `entries[file.Name] = ...`）。若文件与目录同名，目录**永久不可达**。
4. **名字含 `/` 被静默丢弃。** `part/01.7z` → 无响应、无错误。x/net `walkFS` 用 `path.Join` 后 `fs.Stat` 落空，`handlePropfindError` 对 `*os.PathError` 直接 `return nil` 跳过。

分页另有两条（已在 PR #38 修掉，此处记录原始证据）：`_total` 少报会截断（150 条只出 100 条，1 次请求）；`_total` 多报则过量请求（999 → 10 次请求）。

## 与已知可用实现的唯一差异

本仓库 vendored 的 AList Quark driver 在排序为 `none` 时**完全省略** `_sort` 参数；本项目始终发送空值 `_sort=`。这是与已知可用实现之间**唯一**的请求形状差异，也是最可疑的一条：空 `_sort` 与缺省在 provider 侧是否等价未经验证。若 provider 对空 `_sort` 采用非唯一默认排序，页边界会漂移，导致条目重复或遗漏——这与"同目录下部分分卷不显示、其它文件正常"的症状高度吻合。

**这一条需要真机验证，无法在离线环境判定。**

## 需要真机确认的步骤

```powershell
$env:GOTMPDIR="D:\quark-rclone\.tmp\gobuild"; $env:GOFLAGS="-mod=mod"
cd D:\quark-rclone; go build -o out\quark-adapter.exe .\cmd\quark-adapter

# 1. 直接看 provider 对问题目录返回什么（分卷在不在？file 字段是什么？_total 是多少？）
.\out\quark-adapter.exe list <问题目录FID>
(.\out\quark-adapter.exe list <问题目录FID>) | Measure-Object -Line   # 与网页版条目数对比

# 2. 只看 DAV 层（不经 rclone/WinFsp）
.\out\quark-adapter.exe serve -cookie-file <COOKIE文件> -port 17385
# 另开一个 shell，与 rclone 发送的请求逐字节一致：
$body = '<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:displayname/><d:getlastmodified/><d:getcontentlength/><d:resourcetype/></d:prop></d:propfind>'
$ck = (Get-Content <COOKIE文件> -Raw).Trim()
$r = Invoke-WebRequest -Method PROPFIND -Uri "http://127.0.0.1:17385/dav/<子目录>/" `
     -Headers @{ Cookie = $ck; Depth = "1" } -Body $body -ContentType "application/xml"
$r.Content | Select-String -Pattern '7z\.\d{3}' -AllMatches
# 这里没有 → 问题在上游（步骤 1）；这里有 → 问题在 rclone/VFS/WinFsp

# 3. rclone 侧：开 debug，看它有没有主动丢弃
.\out\bundle\rclone.exe mount quark: Q: --config <RCLONE配置> `
  --vfs-cache-mode writes --dir-cache-time 30m --vfs-refresh -vv --log-file .\rclone-debug.log
# 开完目录后，这些行会指出具体的丢弃点：
Select-String -Path .\rclone-debug.log -Pattern '7z\.\d{3}|Ignoring item|Item with unknown path|Name too long|couldn.t list files|500'
#   "Ignoring item with bad status"   → rclone webdav.go 丢弃
#   "Item with unknown path received" → 路径前缀不匹配
#   "Name too long (N bytes) for FUSE"→ WinFsp 层
#   "couldn't list files" / HTTP 500  → 上面第 1 条（整页被毒化）
```

## 判读

- 若步骤 1 就看不到分卷 → 上游列举问题，优先怀疑 `_sort=` 形状差异与 `_total`。
- 若步骤 1 看得到、步骤 2 看不到 → 适配层（`dav`）问题。
- 若步骤 2 看得到、盘符看不到 → rclone/VFS/WinFsp 层。

按现有证据，**第一种最可能**：因为过滤层已被逐条证伪，而 `_sort=` 是唯一与已知可用实现不同的地方。
