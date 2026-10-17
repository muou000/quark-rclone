// Command quark-adapter is a standalone harness for the Quark adapter core.
//
// It exists so the adapter can be exercised end-to-end without a mount: import
// a cookie, then drive every operation the adapter exposes against the real
// provider. It is deliberately thin — every behaviour lives in the tested
// packages — and deliberately quiet about credentials: the cookie never
// appears in argv, in output, or in errors.
//
// Cookie handling follows ADR 0003's shape as far as a stateless tool can:
// the candidate is read from stdin (piped or typed, never a flag), and the
// effective cookie is written to a file the caller names. DPAPI encryption of
// that file is a separate work item; until then the caller picks a location
// with the same care as an SSH key.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/muou000/quark-rclone/internal/adapter"
	"github.com/muou000/quark-rclone/internal/quarkapi"
	"github.com/muou000/quark-rclone/internal/quarkerr"
	"github.com/muou000/quark-rclone/internal/redact"
	"github.com/muou000/quark-rclone/internal/session"
)

const usage = `quark-adapter — standalone harness for the Quark adapter core

usage:
  quark-adapter <command> [arguments]

commands:
  import        read a candidate cookie on stdin, validate it, and activate it
  list          list a directory by FID (default: the drive root)
  download      mint a download URL for a FID
  mkdir         create a directory
  upload        upload a local file
  rename        rename an object
  move          move an object to a new parent
  delete        delete an object by FID
  refresh       force a session cookie refresh now
  keepalive     run the periodic session refresh until interrupted
  capabilities  print the backend capability statement

common flags:
  -cookie-file path   file holding the active cookie (import writes it)
`

func main() {
	if len(os.Args) < 2 {
		fail(errors.New("missing command"), usage)
	}
	cmd, args := os.Args[1], os.Args[2:]

	ctx, cancel := signalContext()
	defer cancel()

	if err := run(ctx, cmd, args); err != nil {
		fail(err, "")
	}
}

// run dispatches one command.
func run(ctx context.Context, cmd string, args []string) error {
	switch cmd {
	case "import":
		return runImport(ctx, args)
	case "list":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			parent := quarkapi.RootFID
			if len(rest) > 0 {
				parent = rest[0]
			}
			entries, err := svc.List(ctx, parent)
			if err != nil {
				return err
			}
			for _, e := range entries {
				kind := "d"
				if !e.IsDir() {
					kind = "f"
				}
				fmt.Printf("%s %s %12d %s\n", kind, e.FID, e.Size, e.Name)
			}
			return nil
		})
	case "download":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			if len(rest) != 1 {
				return errors.New("usage: download <fid>")
			}
			res, err := svc.DownloadURL(ctx, rest[0])
			if err != nil {
				return err
			}
			fmt.Println(res.URL)
			return nil
		})
	case "mkdir":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			if len(rest) != 2 {
				return errors.New("usage: mkdir <parent-fid> <name>")
			}
			fid, err := svc.Mkdir(ctx, rest[0], rest[1])
			if err != nil {
				return err
			}
			fmt.Println(fid)
			return nil
		})
	case "upload":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			if len(rest) != 3 {
				return errors.New("usage: upload <parent-fid> <name> <local-file>")
			}
			f, err := os.Open(rest[2])
			if err != nil {
				return err
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil {
				return err
			}
			fid, err := svc.Upload(ctx, adapter.UploadRequest{
				ParentFID:   rest[0],
				Name:        rest[1],
				Size:        info.Size(),
				ContentType: "application/octet-stream",
			}, f, func(sent int64) {
				fmt.Fprintf(os.Stderr, "\r%12d / %d", sent, info.Size())
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr)
			fmt.Println(fid)
			return nil
		})
	case "rename":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			if len(rest) != 2 {
				return errors.New("usage: rename <fid> <new-name>")
			}
			return svc.Rename(ctx, rest[0], rest[1])
		})
	case "move":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			if len(rest) != 2 {
				return errors.New("usage: move <fid> <to-parent-fid>")
			}
			return svc.Move(ctx, []string{rest[0]}, rest[1])
		})
	case "delete":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			if len(rest) != 1 {
				return errors.New("usage: delete <fid>")
			}
			return svc.Delete(ctx, rest[0])
		})
	case "refresh":
		return withService(ctx, args, func(svc *adapter.Service, rest []string) error {
			return svc.RefreshSessionCookie(ctx)
		})
	case "keepalive":
		return runKeepalive(ctx, args)
	case "capabilities":
		caps := adapter.CapabilitiesOf()
		fmt.Printf("copy:             %s\n", supportStr(caps.Copy))
		fmt.Printf("overwrite upload: %s\n", supportStr(caps.OverwriteUpload))
		return nil
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// runImport reads a candidate cookie from stdin, validates it, activates it
// and writes the effective cookie to the cookie file.
func runImport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	cookieFile := fs.String("cookie-file", "", "file to write the effective cookie to (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cookieFile == "" {
		return errors.New("import requires -cookie-file")
	}

	fmt.Fprintln(os.Stderr, "paste the browser-exported cookie, then press enter:")
	candidate, err := readLine(os.Stdin)
	if err != nil {
		return err
	}

	store := session.NewStore("", func(effective string) { writeCookieFile(*cookieFile, effective) })
	svc := adapter.New(quarkapi.New(store, quarkapi.Config{}, nil), store)
	if err := svc.ValidateAndActivate(ctx, candidate); err != nil {
		// A rejected import must not create or clobber the cookie file, and
		// the failure must be reported without echoing the candidate.
		return err
	}
	fmt.Fprintln(os.Stderr, "cookie validated and activated")
	return nil
}

// withService wires the adapter from a cookie file and runs fn.
func withService(ctx context.Context, args []string, fn func(svc *adapter.Service, rest []string) error) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cookieFile := fs.String("cookie-file", "", "file holding the active cookie (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cookieFile == "" {
		return errors.New("this command requires -cookie-file")
	}
	svc, _, err := wireService(*cookieFile)
	if err != nil {
		return err
	}
	return fn(svc, fs.Args())
}

// runKeepalive runs the periodic session refresh until interrupted. It is the
// standalone shape of the mount's keep-alive (issue #2): every rotation a
// refresh observes lands in the cookie file, and Ctrl+C stops the loop for
// real rather than leaving it sending requests behind the process.
func runKeepalive(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("keepalive", flag.ContinueOnError)
	cookieFile := fs.String("cookie-file", "", "file holding the active cookie (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cookieFile == "" {
		return errors.New("keepalive requires -cookie-file")
	}
	svc, store, err := wireService(*cookieFile)
	if err != nil {
		return err
	}

	r := session.NewRefresher(store, svc, 0, 0)
	r.Start(ctx)
	defer r.Stop()
	fmt.Fprintln(os.Stderr, "keepalive running; press Ctrl+C to stop")
	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "keepalive stopped")
	return nil
}

// wireService loads the cookie file and builds the adapter on top of a store
// that persists every rotation back to that file.
func wireService(cookieFile string) (*adapter.Service, *session.Store, error) {
	cookie, err := os.ReadFile(cookieFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read cookie file: %w", err)
	}
	store := session.NewStore(strings.TrimSpace(string(cookie)), func(effective string) {
		writeCookieFile(cookieFile, effective)
	})
	return adapter.New(quarkapi.New(store, quarkapi.Config{}, nil), store), store, nil
}

// writeCookieFile persists the effective cookie with owner-only permissions.
// Failures are reported on stderr only: a failed persist must not fail the
// request that caused the rotation.
func writeCookieFile(path, cookie string) {
	if err := os.WriteFile(path, []byte(cookie), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "quark-adapter: could not persist cookie: %v\n", err)
	}
}

// readLine reads one line from r, trimming the newline.
func readLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// supportStr renders a capability flag.
func supportStr(b bool) string {
	if b {
		return "supported"
	}
	return "not supported"
}

// fail prints the error, redacted, plus optional usage, and exits nonzero.
func fail(err error, usage string) {
	msg := redact.Error(err)
	cat := quarkerr.CategoryOf(err)
	if cat != "" && cat != quarkerr.CatUnknown {
		msg = fmt.Sprintf("%s (category: %s)", msg, cat)
	}
	fmt.Fprintln(os.Stderr, "quark-adapter: "+msg)
	if usage != "" {
		fmt.Fprintln(os.Stderr)
		fmt.Fprint(os.Stderr, usage)
	}
	os.Exit(1)
}

// signalContext is a context cancelled on the first interrupt or terminate
// signal, so in-flight requests are abandoned cleanly instead of blocking the
// shutdown until their own timeout.
//
// There is deliberately no wall-clock cap: a large upload legitimately runs
// longer than any bound a harness could guess, and every request is already
// cancellable through this context.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
