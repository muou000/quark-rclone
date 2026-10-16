# Versioned Mount Profile And State Model

首版把非敏感配置保存为 `%LocalAppData%\\QuarkDriveMount\\config.json`，凭据另用 DPAPI CurrentUser 保存；rclone 临时配置、适配层端口、一次性密码和运行 PID 不持久化。Mount profile 持久化显式/自动盘符模式、最近一次实际盘符、Quark 根目录 FID/路径、自动启动、自动恢复、独立缓存目录、`vfs-cache-mode writes` 及回写参数和网络模式。

配置带 `configuration version`。启动时只执行已知旧版本的显式迁移，迁移前原子备份旧文件；迁移失败进入 `Failed` 且保留旧配置，遇到更高未知版本要求升级。`explicit` 盘符被占用进入 `DriveConflict`；`auto` 模式优先尝试最近一次实际盘符，被占用后选择空闲盘符并更新记录。

Mount supervisor 使用统一有限状态集合：`Disabled`、`Starting`、`WaitingForNetwork`、`Recovering`、`Mounted`、`NeedsReauth`、`DriveConflict`、`WinFspRequired`、`Failed`、`Stopping`。状态带阶段、最近非敏感错误、时间和下一次重试时间；滚动日志严格脱敏 Cookie、Authorization、WebDAV 密码和下载 URL 查询参数。

能力矩阵以适配层静态契约为主；每次 Cookie 验证或重新认证后刷新无副作用的认证、根目录列举和读链接探测，保存时间和探测错误。写入能力诊断由用户手动触发，默认不自动创建测试文件。

这样把用户选择、秘密、运行时状态和能力观测分开，配置升级可回滚，自动盘符可恢复，托盘能够把修复动作映射到明确状态，而不会把一次探测结果当成永久权限保证。
