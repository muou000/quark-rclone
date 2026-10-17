package releasegate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/muou000/quark-rclone/internal/quarkerr"
)

// memProvider is an in-memory drive the live-gate logic runs against.
type memProvider struct {
	validateErr   error
	dirs          map[string]string   // fid → name
	children      map[string][]string // parent fid → child fids
	files         map[string][]byte   // fid → content
	names         map[string]string   // fid → name
	refreshCalled bool
	copySupported bool
	next          int
	// ValidateExpiredForTest overrides the expired-cookie failure for the
	// wrong-category case.
	ValidateExpiredForTest func() error
}

func newMemProvider() *memProvider {
	return &memProvider{
		dirs:     map[string]string{"0": "root"},
		children: map[string][]string{"0": {}},
		files:    map[string][]byte{},
		names:    map[string]string{"0": "root"},
	}
}

func (p *memProvider) Validate(_ context.Context, _ string) error { return p.validateErr }
func (p *memProvider) ValidateExpired(_ context.Context, _ string) error {
	if p.ValidateExpiredForTest != nil {
		return p.ValidateExpiredForTest()
	}
	return quarkerr.New(quarkerr.CatAuthRequired, "validate", "31001 need login")
}
func (p *memProvider) Refresh(_ context.Context) error { p.refreshCalled = true; return nil }
func (p *memProvider) CopySupported() bool             { return p.copySupported }

func (p *memProvider) mkNode(parent, name string, content []byte) string {
	p.next++
	fid := fmt.Sprintf("fid-%d", p.next)
	p.names[fid] = name
	p.children[fid] = nil
	if content == nil {
		p.dirs[fid] = name
	} else {
		p.files[fid] = content
	}
	p.children[parent] = append(p.children[parent], fid)
	return fid
}

func (p *memProvider) List(_ context.Context, fid string) ([]GateFile, error) {
	var out []GateFile
	for _, child := range p.children[fid] {
		out = append(out, GateFile{FID: child, Name: p.names[child], IsDir: p.files[child] == nil})
	}
	return out, nil
}

func (p *memProvider) Mkdir(_ context.Context, parent, name string) (string, error) {
	return p.mkNode(parent, name, nil), nil
}

func (p *memProvider) Upload(_ context.Context, parent, name string, content []byte) (string, error) {
	return p.mkNode(parent, name, content), nil
}

func (p *memProvider) Download(_ context.Context, fid string) ([]byte, error) {
	data, ok := p.files[fid]
	if !ok {
		return nil, errors.New("not a file")
	}
	return data, nil
}

func (p *memProvider) Move(_ context.Context, fid, toParent string) error {
	for parent, kids := range p.children {
		for i, kid := range kids {
			if kid == fid {
				p.children[parent] = append(kids[:i], kids[i+1:]...)
				p.children[toParent] = append(p.children[toParent], fid)
				return nil
			}
		}
	}
	return errors.New("not found")
}

func (p *memProvider) Rename(_ context.Context, fid, name string) error {
	p.names[fid] = name
	return nil
}

func (p *memProvider) Delete(_ context.Context, fid string) error {
	// Detach from the parent's listing first: the live delete gate verifies
	// disappearance through List, exactly like the provider does.
	for parent, kids := range p.children {
		for i, kid := range kids {
			if kid == fid {
				p.children[parent] = append(kids[:i], kids[i+1:]...)
			}
		}
	}
	delete(p.names, fid)
	delete(p.files, fid)
	delete(p.dirs, fid)
	delete(p.children, fid)
	return nil
}

func TestLiveLifecyclePassesOnACleanDrive(t *testing.T) {
	p := newMemProvider()
	o := Options{Live: true, Provider: p, LiveCookie: "gate-cookie"}

	report := Run(context.Background(), o, Environment{VerifiedScope: VerifiedScope})
	if !report.Complete {
		t.Fatal("a live run must be complete")
	}
	// Only the live layer is asserted here: the runtime and test-suite gates
	// depend on the machine (WinFsp installed, toolchain present), which the
	// release flow verifies separately.
	for _, res := range report.Results {
		if res.Kind == KindLive && (res.Skipped || !res.OK) {
			t.Errorf("live gate %s = skipped %t ok %t detail %q", res.ID, res.Skipped, res.OK, res.Detail)
		}
	}
	if !p.refreshCalled {
		t.Error("the lifecycle never refreshed the session")
	}

	// Every live gate ran, none skipped.
	ran := map[string]bool{}
	for _, res := range report.Results {
		if res.Kind == KindLive {
			if res.Skipped {
				t.Errorf("%s was skipped", res.ID)
			}
			ran[res.ID] = true
		}
	}
	for _, want := range []string{"live.cookie", "live.root", "live.mkdir", "live.upload", "live.download", "live.move", "live.rename", "live.delete", "live.refresh", "live.reauth", "live.copy-negative"} {
		if !ran[want] {
			t.Errorf("live gate %s did not run", want)
		}
	}

	// The quarantine directory is gone from the drive root.
	files, _ := p.List(context.Background(), "0")
	for _, f := range files {
		if strings.HasPrefix(f.Name, "quark-release-gate-") {
			t.Errorf("quarantine directory %s survived the lifecycle", f.Name)
		}
	}
}

func TestLiveFailurePoisonsAndBlocks(t *testing.T) {
	p := newMemProvider()
	p.validateErr = errors.New("bad cookie")

	report := Run(context.Background(), Options{Live: true, Provider: p, LiveCookie: "c"}, Environment{})
	// The later live gates fail fast with the poisoned cause, not nonsense.
	for _, res := range report.Results {
		if res.Kind == KindLive && res.ID != "live.cookie" && !res.Skipped {
			if !strings.Contains(res.Detail, "前置步骤失败") {
				t.Errorf("%s detail = %q, want the poisoned fast-fail", res.ID, res.Detail)
			}
		}
	}
	// A failed live gate blocks the run, whatever the other layers did.
	if report.Passed {
		t.Fatal("a failed live cookie gate must block the release")
	}
	found := false
	for _, id := range report.FailedIDs() {
		if id == "live.cookie" {
			found = true
		}
	}
	if !found {
		t.Errorf("FailedIDs = %v, want live.cookie", report.FailedIDs())
	}
}

func TestReauthRejectsNonAuthFailure(t *testing.T) {
	// A provider whose expired-cookie validation fails with the WRONG
	// category: the gate must not count that as a pass.
	p := newMemProvider()
	// Cookie validation succeeds for the good cookie; the expired one fails
	// with a plain error instead of auth_required.
	p.ValidateExpiredForTest = func() error { return errors.New("boom") }

	report := Run(context.Background(), Options{Live: true, Provider: p, LiveCookie: "c"}, Environment{})
	var reauth *Result
	for i, res := range report.Results {
		if res.ID == "live.reauth" {
			reauth = &report.Results[i]
		}
	}
	if reauth == nil || reauth.OK {
		t.Fatalf("reauth gate = %+v, want failed", reauth)
	}
	if !strings.Contains(reauth.Detail, "auth_required") {
		t.Errorf("reauth detail = %q, want the category mismatch", reauth.Detail)
	}
}

func TestLiveSkippedMeansIncompleteNotFailed(t *testing.T) {
	report := Run(context.Background(), Options{RunTests: false, Live: false}, Environment{VerifiedScope: VerifiedScope})
	for _, res := range report.Results {
		if res.Kind == KindLive && !res.Skipped {
			t.Errorf("%s ran without -live", res.ID)
		}
	}
	if report.Complete {
		t.Error("skipping the live layer must mark the run incomplete")
	}
}

func TestExitCodeTable(t *testing.T) {
	cases := []struct {
		name             string
		passed, complete bool
		want             int
	}{
		{"verified", true, true, 0},
		{"blocked", false, true, 1},
		{"blocked and incomplete", false, false, 1},
		{"incomplete", true, false, 2},
	}
	for _, tc := range cases {
		report := Report{Passed: tc.passed, Complete: tc.complete}
		if got := report.ExitCode(); got != tc.want {
			t.Errorf("%s: ExitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestRenderTextStatesTheScope(t *testing.T) {
	report := Report{
		Environment: Environment{OSVersion: "Windows 11 (build 22631)", Arch: "amd64", VerifiedScope: VerifiedScope},
		Complete:    true,
		Passed:      true,
	}
	text := report.RenderText()
	for _, want := range []string{"Windows 11", VerifiedScope, "未验证", "发布验证完成"} {
		if !strings.Contains(text, want) {
			t.Errorf("render missing %q:\n%s", want, text)
		}
	}
}

func TestClassifyWindowsBuild(t *testing.T) {
	cases := map[string]bool{
		"19045": true,  // Windows 10 22H2
		"22000": true,  // Windows 11 21H2
		"22631": true,  // Windows 11 23H2
		"10240": true,  // Windows 10 1507
		"7601":  false, // Windows 7
		"abc":   false, // unreadable
	}
	for build, want := range cases {
		if got := classifyWindowsBuild(build); got != want {
			t.Errorf("classifyWindowsBuild(%q) = %t, want %t", build, got, want)
		}
	}
}
