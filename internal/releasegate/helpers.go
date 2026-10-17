package releasegate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"

	"github.com/muou000/quark-rclone/internal/adapter"
	"github.com/muou000/quark-rclone/internal/dav"
)

// runCommand runs one external process (the test-suite gates) and returns
// its combined output for the report.
func runCommand(ctx context.Context, workDir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// firstInteresting condenses a failed test-suite's output: the tail carries
// the FAIL lines, which is all the report needs.
func firstInteresting(out string, err error) string {
	var fails []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "FAIL") || strings.Contains(line, "--- FAIL") {
			fails = append(fails, strings.TrimSpace(line))
		}
	}
	summary := err.Error()
	if len(fails) > 0 {
		summary = strings.Join(fails, " | ")
	}
	return truncateGate(summary, 300)
}

func truncateGate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// httpGetBody fetches one loopback URL's body (the DAV health gate).
func httpGetBody(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return string(body), nil
}

// davServerForGate boots the loopback endpoint without a credential: the
// health gate only reads /healthz and /capabilities, which sit outside the
// cookie gate by contract.
func davServerForGate() *dav.Server {
	return dav.New(dav.Config{
		Addr:    "127.0.0.1:0",
		Cookie:  "release-gate-placeholder",
		Service: (*adapter.Service)(nil),
	})
}
