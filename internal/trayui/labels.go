// Package trayui is the Chinese-first tray experience (issue #24): the
// view model the tray and its local panel render, the recovery flows behind
// it, and the embedded panel page.
//
// The split follows the repository's layering: everything a screen shows —
// Chinese labels, remedies, capability statements, the recovery timeline — is
// computed here against the supervisor and quarkerr vocabularies and is fully
// unit-testable. The tray shell in cmd/quark-tray is a thin renderer over
// this package: an icon, a menu and a loopback web panel, with no business
// logic of its own (ADR 0012).
package trayui

import (
	"github.com/muou000/quark-rclone/internal/quarkerr"
	"github.com/muou000/quark-rclone/internal/supervisor"
)

// StateLabel renders a supervisor state in Chinese. The set is closed over
// ADR 0004's ten states; a state without a label is a bug the label test
// catches the moment a new one is added.
func StateLabel(s supervisor.State) string {
	switch s {
	case supervisor.StateDisabled:
		return "未运行"
	case supervisor.StateStarting:
		return "启动中"
	case supervisor.StateWaitingForNetwork:
		return "等待网络"
	case supervisor.StateRecovering:
		return "恢复中"
	case supervisor.StateMounted:
		return "已挂载"
	case supervisor.StateNeedsReauth:
		return "需要重新登录"
	case supervisor.StateDriveConflict:
		return "盘符冲突"
	case supervisor.StateWinFspRequired:
		return "缺少 WinFsp"
	case supervisor.StateFailed:
		return "挂载失败"
	case supervisor.StateStopping:
		return "停止中"
	}
	return string(s)
}

// StateRemedy is the actionable recovery hint for a state — what the user
// should do next, in Chinese (ADR 0010: the UI shows categories and
// executable recovery actions, never raw errors alone).
func StateRemedy(s supervisor.State) string {
	switch s {
	case supervisor.StateDisabled:
		return "在设置页完成配置后启动挂载。"
	case supervisor.StateStarting:
		return "正在启动并检查就绪状态，无需操作。"
	case supervisor.StateWaitingForNetwork:
		return "请检查网络连接，将在退避后自动重试。"
	case supervisor.StateRecovering:
		return "正在重新挂载，请稍候。"
	case supervisor.StateMounted:
		return "挂载正常，无需操作。"
	case supervisor.StateNeedsReauth:
		return "登录已失效：请在设置页粘贴新的 Cookie 重新登录；旧凭据与 dirty cache 不会丢失。"
	case supervisor.StateDriveConflict:
		return "所选盘符已被其它卷占用：请更换盘符，或在设置页改回自动选择。"
	case supervisor.StateWinFspRequired:
		return "WinFsp 未安装或版本不匹配：请运行 quark-adapter doctor 按提示安装。"
	case supervisor.StateFailed:
		return "请查看诊断页的恢复时间线与脱敏日志定位原因。"
	case supervisor.StateStopping:
		return "正在卸载盘符并停止组件，请稍候。"
	}
	return ""
}

// CategoryLabel renders a stable error category in Chinese.
func CategoryLabel(c quarkerr.Category) string {
	switch c {
	case quarkerr.CatAuthRequired:
		return "登录已失效"
	case quarkerr.CatPermissionDenied:
		return "没有操作权限"
	case quarkerr.CatObjectNotFound:
		return "对象不存在"
	case quarkerr.CatParentNotFound:
		return "上级目录不存在"
	case quarkerr.CatTargetConflict:
		return "目标已存在"
	case quarkerr.CatLocked:
		return "对象被占用"
	case quarkerr.CatNotSupported:
		return "不支持的操作"
	case quarkerr.CatUpstreamTransient:
		return "上游暂时不可用"
	case quarkerr.CatRateLimited:
		return "请求过于频繁"
	case quarkerr.CatQuotaExceeded:
		return "网盘空间不足"
	case quarkerr.CatInvalidRequest:
		return "请求无效"
	case quarkerr.CatInternal:
		return "内部错误"
	case quarkerr.CatCancelled:
		return "已取消"
	case quarkerr.CatUnknown:
		return "未知错误"
	}
	return string(c)
}

// CategoryIsPermanent reports whether the category means retrying cannot
// succeed. The panel uses it to say "需要处理" versus "将自动重试" — a
// transient outage must never be shown as a permanent condition (issue #24,
// ADR 0010).
func CategoryIsPermanent(c quarkerr.Category) bool {
	return quarkerr.Permanent(quarkerr.New(c, "", ""))
}

// PreflightCheckLabel renders a preflight gate's name in Chinese. The set is
// closed over preflight.go's Finding.Check values (plus the panel's own
// unavailable answer); a gate without a label is a bug the label test catches
// the moment a new one is added.
func PreflightCheckLabel(check string) string {
	switch check {
	case "platform":
		return "平台"
	case "rclone":
		return "随包 rclone"
	case "winfsp":
		return "WinFsp 驱动"
	case "mount":
		return "挂载自检"
	case "preflight":
		return "预检"
	}
	return check
}

// UnsupportedNotice is the fixed statement the panel shows for the
// capabilities the backend does not have. It names the limitation as a
// permanent property of Quark, not as a fault of the mount.
const UnsupportedNotice = "夸克网盘不支持复制（Copy）与覆盖上传：同名写入将返回“目标已存在”，不会覆盖旧文件。这是后端能力限制，不是挂载故障。"
