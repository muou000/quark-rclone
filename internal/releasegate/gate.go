// Package releasegate is the release acceptance matrix of ADR 0007 (issue
// #26): the gates a build must pass before it ships, executed by
// `quark-adapter accept` and reported in Chinese with an exit code the
// release flow can block on.
//
// Three layers, per ADR 0007's rule that local simulation may assist but
// never replaces the integration gates:
//
//   - test suites: the blocking unit assertions of AC2/AC3 run as `go test`
//     processes (config migration, DPAPI, redaction, SHA-256, staging
//     rollback, lifecycle behaviours). They run on a build machine.
//   - runtime gates: executable here, without credentials — WinFsp and
//     rclone preflight, DAV endpoint health, upgrade staging round-trip
//     with rollback, single-instance guard, autostart.
//   - live gates: the dedicated Quark account's full lifecycle against the
//     real service (AC1). Flag-gated; a release without them is incomplete,
//     not failed.
//
// Every gate is blocking (AC 4). The environment section states the verified
// scope — Windows 10/11 x64 — and marks everything else 未验证 (AC 5).
package releasegate

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/muou000/quark-rclone/internal/redact"
)

// Kind names the layer a gate belongs to.
type Kind string

const (
	KindTestSuite Kind = "test-suite"
	KindRuntime   Kind = "runtime"
	KindLive      Kind = "live"
)

// Gate is one acceptance matrix entry. Run returns the outcome; a nil Run is
// only legal for a gate the runner skips.
type Gate struct {
	ID    string
	Title string // 中文标题
	Kind  Kind
	Run   func(ctx context.Context) Outcome
}

// Outcome is one gate's verdict.
type Outcome struct {
	OK     bool
	Detail string
}

// Result is one executed gate for the report.
type Result struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Kind    Kind   `json:"kind"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	SkipWhy string `json:"skip_reason,omitempty"`
	Detail  string `json:"detail,omitempty"`
	// DurationMS is the gate's wall time in milliseconds — an int64 of the
	// unit it names, so the JSON report never misstates it.
	DurationMS int64 `json:"duration_ms"`
}

// Report is the acceptance run's whole output.
type Report struct {
	Environment Environment `json:"environment"`
	Results     []Result    `json:"results"`
	// ManualChecks are the items no in-process run can verify (real sleep,
	// real logoff, the real WinFsp installer): the release owner signs them
	// off separately (ADR 0007/0014).
	ManualChecks []string `json:"manual_checks"`
	// Passed is true when every executed gate passed and nothing blocking
	// was skipped.
	Passed bool `json:"passed"`
	// Complete is false when the live layer was skipped (no credential): the
	// release verification is unfinished, which is not the same as failed.
	Complete bool `json:"complete"`
}

// ExitCode maps a report onto the release flow's decision: 0 verified, 1
// blocked, 2 incomplete (live layer missing).
func (r Report) ExitCode() int {
	if !r.Passed {
		return 1
	}
	if !r.Complete {
		return 2
	}
	return 0
}

// manualChecks is the fixed list of items that need a human and a real
// machine; every acceptance run surfaces it for sign-off.
var manualChecks = []string{
	"睡眠唤醒后真机重检（唤醒后自动重跑挂载检查）",
	"注销/关机真机卸载清理（盘符、rclone、临时配置全部消失）",
	"真实 WinFsp 安装流程（官方安装器 + UAC）",
}

// Options configures one acceptance run.
type Options struct {
	// RunTests executes the blocking test-suite gates (needs a Go toolchain).
	RunTests bool
	// Live runs the dedicated-account lifecycle gates; needs a provider.
	Live bool
	// Provider backs the live gates. Required with Live.
	Provider Provider
	// LiveCookie is the dedicated account's candidate cookie the live gates
	// validate and use.
	LiveCookie string
	// BundleDir overrides the rclone runtime bundle for the preflight gate.
	BundleDir string
	// StagingRoot overrides the upgrade round-trip's scratch root; empty uses
	// a temp directory.
	StagingRoot string
	// GoCommand is the test-suite entry point; empty means "go".
	GoCommand string
	// WorkDir is the module directory the test suites run in.
	WorkDir string
}

// Run executes the matrix and assembles the report. Gates of an unconfigured
// layer still appear in the report — marked 跳过 with the reason — so it
// shows truthfully which layers did not run (ADR 0014).
func Run(ctx context.Context, o Options, env Environment) Report {
	report := Report{Environment: env, Complete: o.Live && o.Provider != nil, ManualChecks: append([]string(nil), manualChecks...)}

	// The live session outlives the gates: its cleanup removes whatever the
	// lifecycle left behind, whatever the verdict.
	var session *liveSession
	if o.Live && o.Provider != nil {
		session = &liveSession{provider: o.Provider, cookie: o.LiveCookie}
		defer func() { session.cleanup(context.WithoutCancel(ctx)) }()
	}

	var results []Result
	blocked := false
	runOne := func(g Gate) {
		res := Result{ID: g.ID, Title: g.Title, Kind: g.Kind}
		if g.Kind == KindLive && session == nil {
			res.Skipped = true
			res.SkipWhy = "未提供专用账号凭据（-live）"
			results = append(results, res)
			return
		}
		started := time.Now()
		outcome := g.Run(ctx)
		res.DurationMS = time.Since(started).Milliseconds()
		res.OK = outcome.OK
		// The report is a release artefact, not a diagnostic log: every error
		// text is redacted at this boundary (ADR 0010).
		res.Detail = redact.Text(outcome.Detail)
		if !outcome.OK {
			blocked = true
		}
		results = append(results, res)
	}
	if o.RunTests {
		for _, g := range testSuiteGates(o) {
			runOne(g)
		}
	}
	for _, g := range runtimeGates(o) {
		runOne(g)
	}
	for _, g := range liveGates(o, session) {
		runOne(g)
	}

	report.Results = results
	report.Passed = !blocked
	return report
}

// RenderText renders the report as the Chinese summary the operator reads.
func (r Report) RenderText() string {
	var b strings.Builder
	env := r.Environment
	fmt.Fprintf(&b, "验收环境：%s（%s）\n", env.OSVersion, env.Arch)
	fmt.Fprintf(&b, "已验证范围：%s；其它版本/架构：未验证\n", env.VerifiedScope)
	b.WriteString("\n")
	for _, res := range r.Results {
		mark := "通过"
		switch {
		case res.Skipped:
			mark = "跳过"
		case !res.OK:
			mark = "未通过"
		}
		line := fmt.Sprintf("[%s] %s（%s）", mark, res.Title, res.Kind)
		if res.Skipped {
			line += "：" + res.SkipWhy
		} else {
			line += fmt.Sprintf("  %dms", res.DurationMS)
			if res.Detail != "" {
				line += "  " + res.Detail
			}
		}
		b.WriteString(line + "\n")
	}
	if len(r.ManualChecks) > 0 {
		b.WriteString("\n人工验证清单（发布负责人签收）：\n")
		for _, check := range r.ManualChecks {
			b.WriteString("  [ ] " + check + "\n")
		}
	}
	b.WriteString("\n")
	if r.Passed && r.Complete {
		b.WriteString("结论：全部阻断项通过，发布验证完成。\n")
	} else if r.Passed {
		b.WriteString("结论：已执行项通过，但缺少专用账号验证（-live），发布验证未完成。\n")
	} else {
		b.WriteString("结论：存在未通过的阻断项，禁止发布。\n")
	}
	return b.String()
}

// FailedIDs lists the gates that blocked the release, for the exit message.
func (r Report) FailedIDs() []string {
	var ids []string
	for _, res := range r.Results {
		if !res.OK && !res.Skipped {
			ids = append(ids, res.ID)
		}
	}
	sort.Strings(ids)
	return ids
}
