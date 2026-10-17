# Windows Lifecycle Integration

ADR 0004 规定了登录自启、睡眠恢复与注销清理的状态机语义，ADR 0006 规定了升级 staging 与「禁止新旧并行」。本文记录这些契约在 Windows 上的具体落地方式（issue #25）。

## 登录自启动（AC1）

自启动入口是 **HKCU Run 键**（`Software\Microsoft\Windows\CurrentVersion\Run`），值为带引号的托盘路径加 `-profile`：当前用户权限运行，无 SYSTEM 服务、无 UAC、无计划任务。键路径在 `internal/autostart` 中是参数而非常量，测试指向 scratch 键。开关的唯一事实来源是该注册表值——托盘面板的开关直接读写它。

## 睡眠恢复（AC2）

电源事件经隐藏 message-only 窗口的 `WM_POWERBROADCAST` 到达（`internal/power`）。消息到事件的分类是纯函数并有测试；托盘对 `PBT_APMRESUMEAUTOMATIC/RESUMESUSPEND` 的响应是 `Manager.Recheck`——一次全新生命周期，重跑 WinFsp 预检、就绪探测、rclone 启动与盘符可访问检查，正是 ADR 0004 的「从睡眠恢复后重新执行同一套检查」。

## 注销/关机清理（AC3）

`WM_ENDSESSION(wParam=TRUE)` 与 `WM_QUERYENDSESSION` 走同一隐藏窗口：托盘在窗口过程内**同步**执行 `Manager.Stop`（卸载盘符 → 停 rclone → 停 adapter → 删除临时配置，与 ADR 0004 顺序一致），随后才返回允许会话结束。托盘用 `ShutdownBlockReasonCreate` 显示阻塞原因；注意该 API 只承载原因文本，不授予额外宽限——同步完成清理就是设计上的宽限策略。托盘退出后 `Manager` 进入 stopped 状态，面板的 apply/reauth/recheck 一律拒绝（停止接受新请求）。CLI 侧（quark-adapter mount）的信号上下文在收到终止信号时走同一套 supervisor 清理顺序；托盘内 adapter 是进程内组件而非子进程，rclone 是唯一子进程。

## 升级 staging（AC4/AC5）

应用根目录布局：活跃载荷（两个 exe + `bundle\`）、`staging\<版本>\`（新载荷）、`backup\`（上一版本，供回滚）。

- **Stage**（`quark-adapter upgrade-stage`）：复制载荷 → 逐文件 SHA-256 生成 manifest → 对暂存 bundle 跑与挂载相同的 rclone 版本/清单自检；失败则清理 staging。
- **Activate**（托盘面板「应用升级」）：校验暂存完整性 → 停止挂载并等待盘符消失（回调里 `Manager.Stop`）→ 全部 rename 换位（托盘 exe 在运行中也可改名，bundle 因挂载已停而空闲）→ **升级后自检** → 失败则按记录逆序回滚（目录回滚先清目标），成功则保留 `backup\` 直到下一次 Stage。
- 配置、DPAPI 凭据、按盘符派生的 dirty cache 全程不触碰。
- **新旧不并行**：托盘启动时获取会话级命名互斥体（`internal/singleinstance`），任何版本的第二个托盘实例直接退出——同一盘符与 writes 缓存目录在同一会话内不可能被两个版本同时持有；跨盘符挂载本就各用各的实例目录（ADR 0011）。

## 后果

- 应用根目录必须对当前用户可写（per-user 安装布局）；Program Files 等受保护目录中替换会失败并给出明确错误。
- 升级后托盘进程仍运行旧映像，直到下次重启/重新登录——rclone 已换新，待重启状态不自动重启（ADR 0006）。
- 电源/会话窗口不可用时托盘照常运行，仅失去自动恢复触发点，可手动重新检查。
