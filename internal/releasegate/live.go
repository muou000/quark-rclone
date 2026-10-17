package releasegate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/muou000/quark-rclone/internal/quarkerr"
)

// gateFileContent is the lifecycle probe file's body.
const gateFileContent = "quark release gate — safe to delete\n"

// liveSession carries state across the ordered live gates: the quarantine
// directory and the ids the later steps work on. A failed step poisons the
// session so the remaining gates fail fast with the cause instead of
// cascading nonsense.
type liveSession struct {
	provider Provider
	cookie   string

	gateDir string // name
	gateFID string
	subFID  string
	fileFID string

	failed string
}

func (s *liveSession) poison(step string, err error) Outcome {
	s.failed = fmt.Sprintf("%s: %v", step, err)
	return Outcome{OK: false, Detail: s.failed}
}

func (s *liveSession) precondition() Outcome {
	if s.failed != "" {
		return Outcome{OK: false, Detail: "前置步骤失败：" + s.failed}
	}
	return Outcome{}
}

// cleanup removes whatever the lifecycle left behind, best effort.
func (s *liveSession) cleanup(ctx context.Context) {
	for _, fid := range []string{s.fileFID, s.subFID, s.gateFID} {
		if fid != "" {
			_ = s.provider.Delete(ctx, fid)
		}
	}
}

// liveGates assembles the dedicated-account lifecycle of AC 1, in order.
// Copy appears only as a negative assertion — it is an unsupported
// capability, never exercised (ADR 0007). The session may be nil when the
// live layer was not configured: the runner marks those gates 跳过.
func liveGates(o Options, session *liveSession) []Gate {
	gate := func(id, title string, run func(ctx context.Context) Outcome) Gate {
		return Gate{ID: id, Title: title, Kind: KindLive, Run: func(ctx context.Context) Outcome {
			if msg := session.precondition(); msg.Detail != "" {
				return msg
			}
			return run(ctx)
		}}
	}
	return []Gate{
		gate("live.cookie", "专用账号 Cookie 验证", func(ctx context.Context) Outcome {
			if err := session.provider.Validate(ctx, session.cookie); err != nil {
				return session.poison("cookie", err)
			}
			return Outcome{OK: true}
		}),
		gate("live.root", "根目录列举", func(ctx context.Context) Outcome {
			if _, err := session.provider.List(ctx, "0"); err != nil {
				return session.poison("root", err)
			}
			return Outcome{OK: true}
		}),
		gate("live.mkdir", "建目录（隔离验收目录）", func(ctx context.Context) Outcome {
			name := "quark-release-gate-" + fmt.Sprint(time.Now().Unix())
			fid, err := session.provider.Mkdir(ctx, "0", name)
			if err != nil {
				return session.poison("mkdir", err)
			}
			session.gateDir, session.gateFID = name, fid
			return Outcome{OK: true}
		}),
		gate("live.upload", "上传", func(ctx context.Context) Outcome {
			fid, err := session.provider.Upload(ctx, session.gateFID, "gate.txt", []byte(gateFileContent))
			if err != nil {
				return session.poison("upload", err)
			}
			session.fileFID = fid
			return Outcome{OK: true}
		}),
		gate("live.download", "下载并校验内容", func(ctx context.Context) Outcome {
			body, err := session.provider.Download(ctx, session.fileFID)
			if err != nil {
				return session.poison("download", err)
			}
			if string(body) != gateFileContent {
				return session.poison("download", fmt.Errorf("内容不一致：%q", body))
			}
			return Outcome{OK: true}
		}),
		gate("live.move", "移动（到验收子目录）", func(ctx context.Context) Outcome {
			subFID, err := session.provider.Mkdir(ctx, session.gateFID, "moved")
			if err != nil {
				return session.poison("move-mkdir", err)
			}
			session.subFID = subFID
			if err := session.provider.Move(ctx, session.fileFID, subFID); err != nil {
				return session.poison("move", err)
			}
			return Outcome{OK: true}
		}),
		gate("live.rename", "重命名", func(ctx context.Context) Outcome {
			if err := session.provider.Rename(ctx, session.fileFID, "gate-renamed.txt"); err != nil {
				return session.poison("rename", err)
			}
			return Outcome{OK: true}
		}),
		gate("live.delete", "删除并验证消失", func(ctx context.Context) Outcome {
			for _, fid := range []string{session.fileFID, session.subFID, session.gateFID} {
				if err := session.provider.Delete(ctx, fid); err != nil {
					return session.poison("delete", err)
				}
			}
			files, err := session.provider.List(ctx, "0")
			if err != nil {
				return session.poison("delete-list", err)
			}
			for _, f := range files {
				if f.FID == session.gateFID {
					return session.poison("delete", fmt.Errorf("验收目录仍可见：%s", f.Name))
				}
			}
			return Outcome{OK: true}
		}),
		gate("live.refresh", "会话刷新", func(ctx context.Context) Outcome {
			if err := session.provider.Refresh(ctx); err != nil {
				return session.poison("refresh", err)
			}
			return Outcome{OK: true}
		}),
		gate("live.reauth", "认证失效与恢复（坏 Cookie 必须被拒绝）", func(ctx context.Context) Outcome {
			err := session.provider.ValidateExpired(ctx, "__puus=expired-by-release-gate")
			if err == nil {
				return session.poison("reauth", errors.New("失效 Cookie 被接受"))
			}
			if !quarkerr.IsCategory(err, quarkerr.CatAuthRequired) {
				return session.poison("reauth", fmt.Errorf("失效 Cookie 的错误类别不是 auth_required：%w", err))
			}
			// The recovery: the good credential still validates.
			if err := session.provider.Validate(ctx, session.cookie); err != nil {
				return session.poison("reauth", fmt.Errorf("恢复验证失败：%w", err))
			}
			return Outcome{OK: true}
		}),
		gate("live.copy-negative", "Copy 能力反向断言（不支持即通过）", func(ctx context.Context) Outcome {
			if session.provider.CopySupported() {
				return session.poison("copy-negative", errors.New("Copy 被声明为支持：与 ADR 0007 相矛盾"))
			}
			return Outcome{OK: true}
		}),
	}
}
