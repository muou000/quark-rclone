# Capability And Error Mapping

首版适配层使用稳定的 `Error class` 统一 WebDAV、rclone/VFS、Windows FUSE 和托盘状态。`COPY` 与 `PROPPATCH` 返回 HTTP 405，并带 `X-Quark-Error-Class: operation_not_supported`；rclone/VFS 将其映射为 `ENOSYS`，托盘显示“不支持复制/属性修改”。

夸克不允许覆盖上传时，目标已存在返回 HTTP 409 Conflict，并带 `target_conflict`；不删除旧文件，rclone/VFS 映射为 `EEXIST`。真实的条件请求失败可以使用 412，但仍归入同一目标冲突类别。

认证失效返回 HTTP 401 和 `auth_required`，Mount supervisor 进入 `NeedsReauth`。权限错误返回 403/`permission_denied`；对象不存在返回 404/`object_not_found`；429、5xx、网关错误和超时保留对应 HTTP 语义并归入 `upstream_transient`，监督器进入 `Recovering`。锁冲突使用 WebDAV 423/`locked`，不被误判为认证失效。

dirty cache 回写失败不把本地文件变成只读、不隐藏也不自动删除；文件继续可读和可写，失败请求和后台回写记录 `writeback_failed`，托盘显示文件、重试次数和下一次重试时间。Windows FUSE 的通用 `EIO` 只作为最后的底层兜底，监督器必须结合稳定错误类别和子进程日志判断真实状态。

启动或重新认证只运行无副作用的认证、根目录列举和读链接探测。静态能力矩阵声明 list/read/write/mkdir/move/rename/delete 可用、copy 和 overwrite 不可用；用户可手动触发 `Write diagnostic`，用小文件完成写入和删除检查，结果保存到非敏感能力快照和脱敏日志。

稳定错误类别不包含 Cookie、Authorization、下载 URL 查询参数或原始 provider message。原始错误只进入严格脱敏的诊断日志，UI 使用中文类别和可执行的恢复动作。
