# Supervised Clean-Room Mount Boundary

首版由托盘程序作为单一 `Mount supervisor`，监督两个独立子进程：完全清洁重写的 Quark adapter process，以及负责 WebDAV 消费和 WinFsp 盘符挂载的 rclone 进程。适配进程只绑定配置中的 loopback 端口，提供 `/dav`、`/healthz` 和能力探测端点；rclone 在临时配置中直接携带 Quark Cookie，端点拒绝非回环访问。适配进程健康后才启动 rclone，盘符可访问后才报告 Mounted。任一子进程异常都先卸载并停止 rclone，再重启适配进程和 rclone；退出或升级时先等待盘符消失，再停止适配进程。

选择清洁重写 Quark API 和 WebDAV 协议，是为了让运行时不链接或复制 AList 实现，同时保留 AList `quark_uc` 的行为作为兼容参考。这个边界隔离挂载崩溃和升级，保持内部 WebDAV 不成为用户-facing 服务，也为未来替换适配层或增加账号提供清晰的进程接口。
