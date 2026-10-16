# Issue 4: AList、rclone 与 WinFsp 的组合边界

日期：2026-10-15

## 结论

单账号 Windows 首版采用**外部进程组合**：桌面程序管理 AList 进程，AList 用
`quark_uc` 驱动访问夸克并在 loopback 暴露 `/dav`，再管理一个 rclone 进程，令
rclone 的 WebDAV backend 连接这个 `/dav` 并用 Windows `cmount` 挂到指定盘符。
WinFsp 作为用户预装/单独安装的系统组件，rclone 通过 `github.com/winfsp/cgofuse`
调用它。

```text
桌面程序（DPAPI、单账号配置、开机恢复、重试、状态）
  ├─ alist.exe（Quark Cookie -> /dav，loopback，仅本机）
  └─ rclone.exe（webdav remote -> cmount -> WinFsp -> X:）
```

这条边界把首版的业务代码保持在进程编排和配置上，不把 AList 的 AGPL 代码链接
进新的 GUI，也不立即承担把夸克协议改写为 rclone backend 的长期维护成本。它不
消除随程序分发 AList、rclone 或 WinFsp 时的许可证义务；发布前仍需让法律审查确认
“独立程序的聚合”和具体打包方式。

## 仓库事实

### AList 与 Quark

- 本仓库的 [alist/LICENSE](../alist/LICENSE) 是 GNU AGPL-3.0。第 4、5、6 节要求
  保留版权/许可证/免责声明，修改版标明修改并整体按 AGPL 授权，分发目标码时提供
  Corresponding Source；第 13 节对支持远程网络交互的修改版增加网络免费获取源码的
  途径（参见 [AGPL-3.0 原文](https://www.gnu.org/licenses/agpl-3.0.html)）。
- Quark 驱动的配置字段直接保存 Cookie（[meta.go](../alist/drivers/quark_uc/meta.go#L8-L20)），
  注册名为 `Quark`（同文件 [L29-L43](../alist/drivers/quark_uc/meta.go#L29-L43)）。
  初始化会请求配置并启动会话 Cookie 刷新循环（[driver.go](../alist/drivers/quark_uc/driver.go#L59-L89)）。
- 驱动实现了列目录、取下载链接、建目录、移动、重命名、删除和分片上传；`Copy` 明确
  返回 `errs.NotSupport`（[driver.go](../alist/drivers/quark_uc/driver.go#L102-L119)、
  [L247-L277](../alist/drivers/quark_uc/driver.go#L247-L277)、[L301-L385](../alist/drivers/quark_uc/driver.go#L301-L385)）。
  因此首版应按实际能力报告“复制不支持”，不能只根据 WebDAV 方法名推断云端支持复制。
- AList 路由把 `/dav` 注册为 WebDAV 服务（[server/router.go](../alist/server/router.go#L37-L42)）。
  认证使用 AList 用户名/密码或本机 AList token（[server/webdav.go](../alist/server/webdav.go#L64-L112)），
  并按权限拦截 PUT、MKCOL、MOVE、COPY、DELETE、PROPPATCH（[server/webdav.go](../alist/server/webdav.go#L117-L160)）。
  Quark Cookie 不是 WebDAV 客户端凭据，编排器必须建立隔离的 AList 账号/token 配置并
  将它交给 rclone。
- `alist/go.mod` 直接依赖 `github.com/rclone/rclone v1.67.0` 和
  `github.com/winfsp/cgofuse v1.5.1-0.20230130140708-f87f5db493b5`（[go.mod](../alist/go.mod#L60-L73)）。
  AList 的源码实际导入 rclone 的 `backend/crypt`、`lib/readers` 等包，而不是把当前
  仓库的 `rclone/` 目录当作同一个 Go module；这会增加直接复用时的版本耦合。

### rclone、WebDAV 与 Windows mount

- rclone 的 [README](../rclone/README.md#L196-L199) 和 [COPYING](../rclone/COPYING) 声明
  MIT。MIT 的要求主要是保留版权和许可文本，允许修改、再分发和商业使用。
- rclone 已有 WebDAV backend，配置包含 URL、用户名/密码或 bearer token（[backend/webdav/webdav.go](../rclone/backend/webdav/webdav.go#L64-L125)），
  所以它可以直接消费 AList `/dav`，不需要把 AList 的内部 driver 类型接入 rclone。
- Windows `cmount` 明确使用 cgo 版 cgofuse（[cmd/cmount/mount.go](../rclone/cmd/cmount/mount.go#L1-L22)），
  盘符解析和 WinFsp 约束在 [mountpoint_windows.go](../rclone/cmd/cmount/mountpoint_windows.go#L24-L116)；
  rclone 还会在找不到 WinFsp 时提示安装地址（[cmd/cmount/mount.go](../rclone/cmd/cmount/mount.go#L150-L158)）。
  官方 `rclone mount` 文档列出 Windows 的 `--network-mode`、`--read-only`、`--vfs-cache-mode`
  等选项：[rclone mount](https://rclone.org/commands/rclone_mount/)。
- `github.com/winfsp/cgofuse` 自身是 MIT（[上游 LICENSE.txt](https://github.com/winfsp/cgofuse/blob/master/LICENSE.txt)）。
  WinFsp 项目本身是 GPLv3，并带 FLOSS 特殊例外：允许 FLOSS 软件链接指定 WinFsp DLL、
  分发未修改安装器，但要求 FLOSS 许可证、在 UI/用户文档显示版权和仓库链接，且不能
  与专有软件混合（[WinFsp License.txt](https://github.com/winfsp/winfsp/blob/master/License.txt)）。
  因此“rclone 是 MIT”不能单独证明一个闭源 GUI 可以把 WinFsp DLL/安装器一起打包；
  闭源商业发布应先取得 WinFsp 的商业许可或改为要求用户单独安装并做法律确认。

## 方案比较

| 方案 | 代码/许可证边界 | 维护成本 | 首版影响 |
| --- | --- | --- | --- |
| 直接链接 AList driver 或复制 `quark_uc` | AList driver 的方法签名暴露 `alist/internal/model` 和 `alist/internal/driver`（[driver.go](../alist/internal/driver/driver.go#L9-L39)、[obj.go](../alist/internal/model/obj.go#L27-L58)），不是稳定的外部 API；复制或修改其实现通常属于 AGPL covered work。分发需源码、许可证、修改声明和相应法律提示。 | 高：要跟随 AList 内部模型、全局注册、存储生命周期和 Quark API 变化。 | 功能最接近现状，但会把 AGPL 与 GUI 组合在一起，且外部 module 受 Go `internal` 规则限制；不选作首版。 |
| AList WebDAV + rclone mount（同一程序内启动两个进程） | AList 仍按 AGPL 处理，rclone/cgofuse 按 MIT，WinFsp 按 GPLv3/例外；进程边界有利于保持 GUI 与 AList 的独立聚合，但分发 AList 二进制仍须满足 AGPL。 | 中低：主要是生成 AList 配置、loopback 凭据、进程健康检查、重启/升级和 Windows 服务生命周期。 | 能复用已实现的 Quark 读写和会话刷新；首版只需一个账号。需要在测试中确认 WebDAV 的锁、重命名、分片上传、缓存和错误码。**推荐。** |
| 把 Quark 改写成 rclone backend | 新写的 backend 可按 rclone MIT 发布；但直接复制 AList Quark 源码会把 AGPL 代码带入，不能简单改标 MIT。需要独立实现并保留所有依赖许可证。 | 高：必须实现 rclone `fs.Fs`/`fs.Object`、feature flags、范围读、上传、重试、Cookie 刷新、目录最终一致性和集成测试；还要跟 rclone API 演进。 | 最终可去掉 AList，部署更轻，适合后续路线；首版周期和 API 风险都太高。 |
| 只用 rclone `librclone` 进程内/动态库 | rclone 提供 C RPC 库，但接口目前标为 experimental（[librclone/README.md](../rclone/librclone/README.md#L1-L8)），Windows 要求共享库、运行时动态加载且不能 `FreeLibrary`（[README.md](../rclone/librclone/README.md#L76-L121)）。它仍然没有 Quark backend，不能绕过前两种方案。 | 中高：C/Go ABI、生命周期、WinFsp 和线程问题叠加。 | 不作为首版；外部 `rclone.exe` 的进程边界更容易诊断和重启。 |

## 首版边界与落地约束

1. **组件职责**：GUI/后台代理持有 DPAPI 保护的 Quark Cookie、AList WebDAV 用户/token、
   rclone 配置和盘符；AList 只监听 loopback 随机端口；rclone 只连接该端口并负责 WinFsp
   mount。退出时先卸载 rclone，再停 AList。
2. **启动恢复**：当前用户登录后启动代理；先检查 WinFsp，再启动 AList，轮询 `/ping` 和
   WebDAV `OPTIONS/PROPFIND`，最后启动 rclone。网络未就绪或 Cookie 失效时指数退避，
   失败状态必须可见且不泄露 Cookie。
3. **能力声明**：初始化后执行根目录列举、读取、小文件写入、建目录、重命名、移动、删除
   的探测；复制能力按 AList driver 的 `Copy` 结果显示为不支持/只读。rclone 的
   `--read-only` 是故障保护，不应伪装成云端能力检测。
4. **发布物**：建立第三方清单（AList AGPL、rclone MIT、cgofuse MIT、WinFsp GPLv3/例外
   以及 `go.mod` 全部传递依赖），在 About/许可页和安装文档显示归属。若捆绑 AList 或
   修改它，提供对应源码、构建脚本和修改日期；若捆绑 WinFsp 安装器，先确认 FLOSS/商业
   许可条件。AList 的 `pkg/gowebdav` README 另标 BSD 3-Clause（[README](../alist/pkg/gowebdav/README.md#L143-L150)），
   任何直接复制该库时也要保留其通知。

## 尚未关闭的风险

- 需要产品决策：发布 GUI 是否开源、是否捆绑 AList/rclone/WinFsp 安装器，还是要求用户
  自行安装；这决定 AGPL Corresponding Source、WinFsp 例外或商业许可的具体路径。
- 需要真实夸克账号做 WebDAV 集成测试，特别是 PUT 分片上传、断点/缓存、MOVE/DELETE
  最终一致性、Cookie 刷新和网络中断恢复。
- 需要审计本仓库对 AList Quark driver 的改动来源和版权头；本笔记不是法律意见，发布前
  应由熟悉 GPL/AGPL 和 WinFsp 条款的人复核“聚合”和闭源 GUI 的边界。

## 参考资料

- [AList upstream repository and license](https://github.com/AlistGo/alist)
- [rclone upstream repository and COPYING](https://github.com/rclone/rclone)
- [cgofuse upstream repository and MIT license](https://github.com/winfsp/cgofuse)
- [WinFsp License.txt](https://github.com/winfsp/winfsp/blob/master/License.txt)
- [rclone mount documentation](https://rclone.org/commands/rclone_mount/)
- [GNU AGPLv3](https://www.gnu.org/licenses/agpl-3.0.html)
