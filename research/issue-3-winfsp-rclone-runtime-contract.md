# Windows 盘符挂载的 WinFsp 与 rclone 运行时契约

研究对象：[确认 Windows 盘符挂载的 WinFsp 与 rclone 运行时契约](https://github.com/muou000/quark-rclone/issues/3)

研究日期：2026-10-15。行号均来自当前 checkout；外部 cgofuse 引用固定到
rclone `go.mod` 所依赖的上游提交 `f2c4fccdb286`，WinFsp 文档引用上游仓库的
当前主线。

## 决议

首版桌面程序应把 rclone 作为**独立、受监督的前台进程**运行，并使用它的公开
CLI（或同一进程的公开 Remote Control API）完成挂载和卸载。程序本身保存挂载
配置、启动重试、状态和日志；不要把 `rclone/cmd/cmount` 的未导出函数或
`mountlib` 内部状态链接进 GUI。这样可以把 rclone/WinFsp 的生命周期、崩溃和升级
隔离出来，也能直接复用官方 Windows 构建。

Windows 运行时必须有与进程架构匹配的 WinFsp 安装（内核文件系统驱动和用户态
DLL）。开发者头文件只在从源码编译 cgo 版本时需要。首版按当前登录用户启动，
默认使用固定盘符模式；如果 Windows 应用对网络延迟/固定盘假设不兼容，允许切换
`--network-mode`。启动前做 WinFsp 安装/架构预检，失败时给出安装链接和日志，
不要把“进程成功启动”当作“盘符已经可用”。

## 组件、构建和架构

- rclone 的 Windows mount 实现位于 `cmount` build tag；仓库文档说明 Windows
  的 `mount` 命令需要 `-tags cmount`（[docs/content/install.md#L392-L397](../rclone/docs/content/install.md#L392-L397)）。
  Windows cgofuse 支持 cgo 和 `CGO_ENABLED=0` 两种构建，官方 rclone Windows
  发布物使用后者（[docs/content/install.md#L399-L409](../rclone/docs/content/install.md#L399-L409)）。
  因此首版优先调用经过验证的 `rclone.exe`，不要求用户机器有 GCC。
- 从源码构建 cgo 版本时需要 WinFsp 的 Developer 文件和 `CPATH` 指向
  `inc/fuse`；这是编译时依赖，不是运行时把头文件复制给用户的理由
  （[docs/content/install.md#L411-L415](../rclone/docs/content/install.md#L411-L415)）。当前
  checkout 锁定 `github.com/winfsp/cgofuse` 的版本为
  `v1.6.1-0.20260126094232-f2c4fccdb286`（[rclone/go.mod#L88-L92](../rclone/go.mod#L88-L92)）。
- WinFsp 官方说明其核心由内核模式 FSD 和用户态 DLL 组成，支持 Windows 7--11
  以及 x86、x64、ARM64（[WinFsp README#L32-L53](https://github.com/winfsp/winfsp/blob/master/README.md#L32-L53)）。
  运行时不能只检查一个任意 DLL：cgofuse 根据 `runtime.GOARCH` 选择
  `winfsp-x86.dll`、`winfsp-x64.dll` 或 `winfsp-a64.dll`，先尝试系统加载路径，
  再读取 `HKLM\Software\WinFsp\InstallDir` 并加载其 `bin` 子目录
  （[cgofuse host_nocgo_windows.go#L474-L601](https://github.com/winfsp/cgofuse/blob/f2c4fccdb286/fuse/host_nocgo_windows.go#L474-L601)，
  [cgofuse host_cgo.go#L234-L314](https://github.com/winfsp/cgofuse/blob/f2c4fccdb286/fuse/host_cgo.go#L234-L314)）。
  因此 x64 GUI 要配 x64 rclone/WinFsp，ARM64 同理；不要混用不同架构的私有 DLL。
- 基础挂载没有在 rclone 源码中写死某个 WinFsp 数字版本；实际契约是 DLL 能被加载、
  `fuse_main_real`/`fuse_exit`/相关 FUSE 符号存在且 WinFsp 驱动可完成挂载。若启用
  `FileSecurity`，rclone 文档把它标为 WinFsp 2021/1.9 引入的能力
  （[docs/content/commands/rclone_mount.md#L210-L237](../rclone/docs/content/commands/rclone_mount.md#L210-L237)），
  这应成为显式版本门槛；不启用时不应伪造一个更高的最低版本。

## 盘符和网络模式

- cgofuse/WinFsp 接受的盘符挂载点是裸 `X:`。rclone 用正则识别单字母加冒号，
  明确说明 `X:\` 不是 WinFsp 的挂载格式，并在进入 cgofuse 前裁掉尾部反斜杠
  （[rclone/cmd/cmount/mountpoint_windows.go#L17-L53](../rclone/cmd/cmount/mountpoint_windows.go#L17-L53)，
  [rclone/cmd/cmount/mountpoint_windows.go#L95-L131](../rclone/cmd/cmount/mountpoint_windows.go#L95-L131)）。
  `\\?\X:` 等扩展长度路径也不支持。配置层应规范化成大写或保留用户大小写的
  `^[A-Za-z]:$`，不要传 `X:\` 或 `\\?\X:`。
- `*` 或空挂载点会从 `Z:` 向下寻找未占用盘符；实际盘符由挂载函数返回，不能只
  保存输入值（[rclone/cmd/cmount/mountpoint_windows.go#L55-L75](../rclone/cmd/cmount/mountpoint_windows.go#L55-L75)，
  [rclone/cmd/mountlib/mount.go#L200-L208](../rclone/cmd/mountlib/mount.go#L200-L208)）。
  桌面程序若允许自动盘符，必须持久化返回的实际盘符，并在下次启动前重新检查冲突。
- 默认模式把远端作为固定盘。Windows 对固定盘和网络盘采用不同的缓存、预览和
  延迟假设；rclone 文档建议遇到应用冻结或异常时尝试网络盘
  （[docs/content/commands/rclone_mount.md#L91-L110](../rclone/docs/content/commands/rclone_mount.md#L91-L110)）。
  `--network-mode` 只能使用盘符，不能挂载到目录（[docs/content/commands/rclone_mount.md#L122-L129](../rclone/docs/content/commands/rclone_mount.md#L122-L129)）。
- 网络模式的 `--volname` 必须形成唯一网络共享名。完整 `\\server\share` 会隐式
  开启网络模式；以 UNC 作为挂载点时，rclone 自动分配盘符并把 UNC 转成 WinFsp
  需要的单反斜杠 `\server\share` 前缀（[rclone/cmd/cmount/mountpoint_windows.go#L77-L93](../rclone/cmd/cmount/mountpoint_windows.go#L77-L93)，
  [rclone/cmd/cmount/mountpoint_windows.go#L134-L158](../rclone/cmd/cmount/mountpoint_windows.go#L134-L158)，
  [docs/content/commands/rclone_mount.md#L131-L164](../rclone/docs/content/commands/rclone_mount.md#L131-L164)）。
  首版只提供本地盘符入口时可隐藏 UNC 输入，但应保留网络模式开关供兼容性排障。

## 挂载、就绪和卸载生命周期

- `cmount.mount` 创建 `fuse.FileSystemHost`，在后台 goroutine 调用
  `host.Mount(mountpoint, options)`；Windows 还会等待文件系统初始化和盘符
  `os.Stat` 可见，单次等待总计约 10 秒（[rclone/cmd/cmount/mount.go#L107-L119](../rclone/cmd/cmount/mount.go#L107-L119)，
  [rclone/cmd/cmount/mount.go#L127-L169](../rclone/cmd/cmount/mount.go#L127-L169)，
  [rclone/cmd/cmount/mount.go#L201-L221](../rclone/cmd/cmount/mount.go#L201-L221)）。
  外部编排器应同时观察子进程、日志和盘符可访问性，并将“进程存活但盘符不可用”
  视为启动失败或重试状态。
- cgofuse 的公开 host API 是 `NewFileSystemHost`、`SetCap*`、`Mount` 和
  `Unmount`；`Mount` 返回布尔值，WinFsp 缺失时会 panic `cgofuse: cannot find
  winfsp`，`Unmount` 只能在 Init 后、Destroy 前调用（[cgofuse host.go#L654-L685](https://github.com/winfsp/cgofuse/blob/f2c4fccdb286/fuse/host.go#L654-L685)，
  [cgofuse host.go#L699-L829](https://github.com/winfsp/cgofuse/blob/f2c4fccdb286/fuse/host.go#L699-L829)）。
  WinFsp 在 Windows 侧负责 FUSE 层的清理，不应从 GUI 直接删除盘符或杀死宿主线程来
  “卸载”。
- rclone 的卸载闭包调用 `host.Unmount()`，成功后在 Windows 等待盘符消失约 10 秒；
  超时只记录错误并继续返回，失败则返回 `host unmount failed`
  （[rclone/cmd/cmount/mount.go#L171-L199](../rclone/cmd/cmount/mount.go#L171-L199)）。
  程序退出顺序应是请求卸载、等待盘符消失、再终止 rclone；只有子进程异常退出时才
  进入强制清理和重试。
- `mountlib.MountFn` 是一个内部函数类型，返回错误通道、卸载函数、实际挂载点和
  错误；`MountPoint.Wait` 从错误通道取终止原因，调用卸载并关闭 VFS
  （[rclone/cmd/mountlib/mount.go#L200-L224](../rclone/cmd/mountlib/mount.go#L200-L224)，
  [rclone/cmd/mountlib/mount.go#L383-L455](../rclone/cmd/mountlib/mount.go#L383-L455)）。
  这是 rclone 自身的生命周期实现，不是 GUI 应依赖的跨版本插件 ABI。

## 错误传播和操作能力

- WinFsp 缺失时，rclone 捕获 cgofuse panic，将错误写入错误通道并附加
  `Hint: Install WinFsp from https://winfsp.dev/rel/`；`host.Mount` 返回 false
  时则是通用的 `mount failed`（[rclone/cmd/cmount/mount.go#L150-L169](../rclone/cmd/cmount/mount.go#L150-L169)）。
  `mountlib` 再包装成 `failed to mount FUSE fs: ...`，因此外部程序应保留原始 stderr
  和退出码，同时把“WinFsp 未安装/架构不匹配”与“远端认证/网络失败”分开显示。
- Windows FUSE 操作把 rclone VFS 错误映射为 errno：不存在、已存在、权限拒绝、
  只读、未实现等分别映射为 `ENOENT`、`EEXIST`、`EPERM`、`EROFS`、`ENOSYS`；
  未识别错误记录为 IO 错误并映射为 `EIO`（[rclone/cmd/cmount/fs.go#L607-L641](../rclone/cmd/cmount/fs.go#L607-L641)）。
  GUI 不应仅根据 `EIO` 推断 Cookie 失效；应结合 rclone 日志和 Quark/AList 的认证状态。
- cmount 实际实现了打开、读取、写入、刷新、创建、删除、重命名等 VFS 操作，
  但后端是否支持这些能力仍由远端驱动决定；错误映射不是能力声明。首版应在账号
  探测后显示读写能力，不能因为盘符是可写的就承诺所有 Windows 文件操作。

## 缓存和一致性限制

- rclone mount 默认 `--vfs-cache-mode off`。此模式直接读写远端，不能同时读写同一
  文件、不能对写入文件 seek，上传失败也不能重试；一般 Windows 应用需要
  `writes` 或 `full`（[docs/content/commands/rclone_mount.md#L409-L440](../rclone/docs/content/commands/rclone_mount.md#L409-L440)，
  [docs/content/commands/rclone_mount.md#L700-L714](../rclone/docs/content/commands/rclone_mount.md#L700-L714)）。
- `writes` 会把写入/读写文件先缓存在磁盘，支持普通文件系统操作并对失败上传
  以指数退避重试；`full` 还会缓存读取并使用稀疏文件
  （[docs/content/commands/rclone_mount.md#L728-L753](../rclone/docs/content/commands/rclone_mount.md#L728-L753)）。
  这不是离线同步：文件关闭且经过 `--vfs-write-back` 后才回写远端；进程崩溃前未
  上传内容要依赖同一缓存目录的下次启动恢复（[docs/content/commands/rclone_mount.md#L666-L674](../rclone/docs/content/commands/rclone_mount.md#L666-L674)）。
- 同一个远端不能由两个 rclone 进程共享同一非 off VFS cache，否则可能数据损坏；
  编排器必须为唯一挂载实例固定 `--cache-dir`，退出和升级时不要并行启动旧/新
  rclone（[docs/content/commands/rclone_mount.md#L693-L698](../rclone/docs/content/commands/rclone_mount.md#L693-L698)）。
- 属性缓存默认约 1 秒，过高会在远端文件长度变化时增加截断或尾部垃圾风险；
  `--direct-io` 可关闭数据页缓存，但不替代 VFS 写缓存策略
  （[rclone/cmd/mountlib/mount.go#L37-L57](../rclone/cmd/mountlib/mount.go#L37-L57)，
  [docs/content/commands/rclone_mount.md#L442-L470](../rclone/docs/content/commands/rclone_mount.md#L442-L470)）。
  首版建议把 `writes` 作为可解释的写入默认值，把 `full`、缓存目录、大小和回写
  延迟做成高级设置，并在状态页显示“写入先落本地缓存”。

## 可复用接口边界

### 可复用的公开行为

1. `rclone.exe mount remote:path X: [options]` 是最稳定的跨版本边界；子进程退出、
   日志和盘符可见性都可监控，升级 rclone 时不需要重编译 GUI。
2. rclone Remote Control 的 `mount/mount`、`mount/unmount` 和 `mount/types` 是
   已注册的公开调用。它接受 `fs`、`mountPoint`、`mountType`、`mountOpt` 和 `vfsOpt`，
   自动盘符会返回实际 `mountPoint`；API 不支持 `daemon`，且挂载状态保存在该
   rclone 进程内（[rclone/cmd/mountlib/rc.go#L39-L97](../rclone/cmd/mountlib/rc.go#L39-L97)，
   [rclone/cmd/mountlib/rc.go#L100-L176](../rclone/cmd/mountlib/rc.go#L100-L176)，
   [rclone/cmd/mountlib/rc.go#L179-L216](../rclone/cmd/mountlib/rc.go#L179-L216)）。
   因此若使用 RC，GUI 仍需独立管理一个长期 rclone 进程和 loopback 认证，不要把
   RC 当作无状态远程服务。
3. 如果未来完全自行实现文件系统，cgofuse 的 MIT 公共 API
   `FileSystemInterface`/`FileSystemHost` 可以在进程内使用；但那意味着自行维护
   WinFsp 运行时、VFS 语义、错误映射和 Quark 后端，超出首版。

### 不应作为稳定 ABI 的内部实现

- `cmount.mount`、`getMountpoint`、`mountOptions`、`cmount.FS` 的具体字段和
  `mountlib.liveMounts` 都是 rclone 源码内部实现；其中多个函数未导出，且受
  `cmount` build tag 和版本变化影响。直接 import 当前 checkout 会把 GUI 与 rclone
  的内部包和 Go module 版本绑定。
- `MountPoint` 虽然导出，但其 `MountFn`、错误通道和全局 RC 表都是实现细节；只能
  在锁定 rclone 版本、跟随源码升级并补充 Windows 集成测试的情况下内部复用，不能
  作为独立程序的稳定插件契约。

## 缺少 WinFsp 时的用户路径

1. 启动前根据进程架构读取 `HKLM\Software\WinFsp\InstallDir`，检查对应
   `bin\winfsp-{x86,x64,a64}.dll`；检查失败时显示“需要安装/修复 WinFsp（架构）”和
   官方安装页 `https://winfsp.dev/rel/`，不尝试静默下载驱动。
2. 即使 DLL 预检通过，也要启动一个短暂 mount 自检，因为驱动服务、权限、盘符冲突
   和用户会话不能由 DLL 存在性完全证明。将 rclone 的原始错误、WinFsp 架构和盘符
   一起写入诊断日志。
3. 当前用户登录自动恢复应以**非提升权限**运行。rclone 文档说明管理员创建的盘符
   对普通 Explorer/其他会话不可见；SYSTEM 服务可以做全局盘符，但会改用 SYSTEM
   用户配置文件并改变凭据/DPAPI 所属用户（[docs/content/commands/rclone_mount.md#L239-L278](../rclone/docs/content/commands/rclone_mount.md#L239-L278)，
   [docs/content/install.md#L576-L593](../rclone/docs/content/install.md#L576-L593)）。
   首版不使用 WinFsp Launcher service 或 SYSTEM 服务；如未来要全局盘符，需单独设计
   凭据和会话边界。

## 仍需在实现阶段验证的事项

- 在目标 Windows 版本和 x86/x64/ARM64 构建上验证 WinFsp 安装器、驱动启动、睡眠/唤醒、
  网络断开和盘符冲突；本研究只能确认源码契约，不能替代真实设备测试。
- 用真实夸克账号测 `writes`/`full` 缓存下的关闭回写、断网重试、重命名/删除最终一致性，
  并将后端不支持的操作映射为用户可见的只读/不支持状态。
- 若改用 WinFsp Launcher，需要按官方服务架构注册 `HKEY_LOCAL_MACHINE\Software\WinFsp\Services`，
  让子服务支持命令行参数和 CTRL-BREAK 及时退出；这不是首版当前用户登录方案
  （[WinFsp Service Architecture#L13-L49](https://github.com/winfsp/winfsp/blob/master/doc/WinFsp-Service-Architecture.asciidoc#L13-L49)）。

## 主要来源

- [rclone Windows mount documentation](../rclone/docs/content/commands/rclone_mount.md)
- [rclone install/build and Windows startup documentation](../rclone/docs/content/install.md)
- [rclone cmount Windows path handling](../rclone/cmd/cmount/mountpoint_windows.go)
- [rclone cmount lifecycle](../rclone/cmd/cmount/mount.go)
- [rclone mountlib lifecycle and RC API](../rclone/cmd/mountlib/mount.go)
- [rclone cmount errno translation](../rclone/cmd/cmount/fs.go)
- [cgofuse pinned upstream README](https://github.com/winfsp/cgofuse/blob/f2c4fccdb286/README.md)
- [cgofuse pinned host implementation](https://github.com/winfsp/cgofuse/blob/f2c4fccdb286/fuse/host.go)
- [cgofuse pinned Windows loader](https://github.com/winfsp/cgofuse/blob/f2c4fccdb286/fuse/host_nocgo_windows.go)
- [WinFsp upstream README](https://github.com/winfsp/winfsp/blob/master/README.md)
- [WinFsp Service Architecture](https://github.com/winfsp/winfsp/blob/master/doc/WinFsp-Service-Architecture.asciidoc)
