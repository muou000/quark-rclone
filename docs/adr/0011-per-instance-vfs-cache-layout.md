# Per-Instance VFS Cache Layout

ADR 0002 定义了 writes 缓存与失败语义，ADR 0005 把「独立缓存目录」纳入 mount profile。本文记录目录的具体布局与生命周期决策（issue #23）。

## 布局

每个挂载实例的 rclone `--cache-dir` 派生自两层：

```
<cache root>/<mount-point key>
```

- **cache root**：profile 的 `cache_dir` 显式设置；未设置时，profile 模式使用 `<profile dir>\cache`，纯 cookie 文件模式使用 `%TEMP%\quark-rclone-cache`。
- **mount-point key**：裸盘符（`Q:`、`q:`、`Q`）折叠为小写字母本身——同一卷必须同一目录，dirty cache 才能跨挂载恢复；其余挂载点取「清洗后的可读前缀 + 原始挂载点的 FNV 短哈希」（如 `d-data-1a2b3c4d`），保证不同目录不共享缓存；键恒为单一路径段，不可能逃逸出根目录；空值为 `default`。

## 生命周期

- 键取盘符而非会话 ID：同一盘符的两次挂载复用同一目录，进程重启或 supervisor 重启后 rclone 扫描既有缓存元数据，**继续上传遗留的 dirty cache**——这是「网络恢复后重试」的载体（ADR 0002）。
- supervisor 在每次启动 rclone 前以 0700 预建实例目录：挂载早期失败时用户仍能定位缓存位置。
- supervisor 与任何命令都**不删除、不清理、不轮转**缓存目录（ADR 0002：用户清理前不自动删除）。独占性：盘符挂载的并发互斥由 DriveConflict 保证；目录挂载点的键带原始挂载点哈希，不同目录不会同键，因此任何两个活跃实例都不会共享同一实例目录。
- 会话结束时，若回写台账仍有条目，CLI 打印未完成文件、错误类别与重试次数，并指向缓存目录与 diag.json。

## 重试归属

「有限重试」分三层，各管一段：rclone 自身对 dirty item 的回写重试（数据面）；supervisor 对 far-side 失败的 5s→5min 退避（生命周期面，ADR 0004）；回写台账的 5s→5min 节奏仅是**展示**的下次重试时间（诊断面，ADR 0010），不驱动任何重试。

## 后果

- 更换盘符后旧盘符目录遗留（含可能的 dirty cache），等用户清理；这是「不自动删除」的直接代价。
- 缓存目录持有未上传的明文文件数据，其权限边界与 profile 目录一致（per-user ACL，ADR 0009 的同一论证）。
- rclone 仍是外部二进制：dirty cache 的实际保留与重试由其 VFS 实现，本项目负责配置正确（writes 模式 + 独占目录）并把失败状态如实上浮。
