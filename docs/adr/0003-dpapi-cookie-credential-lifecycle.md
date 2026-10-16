# DPAPI Cookie Credential Lifecycle

首版接受用户粘贴的浏览器 Cookie 字符串，保留未知 Cookie，只做首尾空白和基本 `name=value` 校验。候选 Cookie 必须先通过 Quark `/config` 和所选根目录列举，验证成功后才替换当前凭据；普通 mount profile 配置与凭据分开保存，凭据以版本化 blob 通过 Windows DPAPI CurrentUser 加密，并以原子替换方式更新。

适配层收到 `__puus`/`__pus` 等会话刷新字段时静默合并并原子持久化，不打断挂载。已知 Quark 认证失效签名（包括 `31001 / need login`）会停止并卸载 rclone，保留适配进程、mount profile 和 dirty cache，进入 `NeedsReauth`；网络错误、429、5xx 和传输错误保持凭据并进入 `Recovering`。重新认证验证成功后沿用原 mount profile 和 dirty cache 恢复挂载。

选择 DPAPI CurrentUser 和候选先验证，是为了让凭据只能由当前 Windows 用户解密，并避免错误 Cookie 覆盖仍可用的会话。选择保留 mount profile 与 dirty cache，是为了把“需要重新认证”和“配置丢失/本地写入丢失”明确分开。
