# Quark Adapter Service Boundary

清洁重写的 Quark adapter process 只负责 Quark API、会话刷新、对象操作和适配层内存状态；Mount supervisor 负责 DPAPI、mount profile、进程状态、恢复重试和日志持久化。适配层拆成 Quark HTTP client、session store、对象/目录服务、上传下载服务和错误分类器，WebDAV handler 只调用这些服务，不直接拼接 Quark 请求。

Quark 文件和目录以 FID 作为稳定对象身份，保存根目录 FID/路径；WebDAV 路径只是请求到 FID 的解析结果，不建立完整本地索引。session store 原子保存当前 Cookie，每次请求使用快照；响应合并 `__puus`/`__pus` 后通过事件通知 supervisor 原子更新 DPAPI，刷新失败不能覆盖并发更新。

适配层只对安全的幂等请求和明确的 Quark 控制面瞬态错误重试。上传 pre 不重放，分片和 finish 按阶段规则重试；适配层外的恢复、进程重启和挂载重试由 supervisor 负责。这样把云端协议一致性留在适配层，把本地生命周期和秘密管理留在 supervisor，且不引入 AList 运行时依赖。
