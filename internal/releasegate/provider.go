package releasegate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/muou000/quark-rclone/internal/adapter"
)

// GateFile is one listed drive entry, the slice the live gates need.
type GateFile struct {
	FID   string
	Name  string
	IsDir bool
}

// Provider is the live gates' view of the adapter service, injected so the
// gate logic is testable without the real provider. Production hands it
// *adapter.Service.
type Provider interface {
	Validate(ctx context.Context, cookie string) error
	List(ctx context.Context, fid string) ([]GateFile, error)
	Mkdir(ctx context.Context, parent, name string) (string, error)
	Upload(ctx context.Context, parent, name string, content []byte) (string, error)
	Download(ctx context.Context, fid string) ([]byte, error)
	Move(ctx context.Context, fid, toParent string) error
	Rename(ctx context.Context, fid, name string) error
	Delete(ctx context.Context, fid string) error
	Refresh(ctx context.Context) error
	// ValidateExpired expects an unusable cookie and returns the resulting
	// error; the caller asserts it is an auth failure.
	ValidateExpired(ctx context.Context, cookie string) error
	// CopySupported must be false: Copy is an unsupported capability (AC 1),
	// asserted negatively instead of exercised.
	CopySupported() bool
}

// NewAdapterProvider adapts *adapter.Service to the live gates.
func NewAdapterProvider(svc *adapter.Service) Provider { return adapterProvider{svc} }

type adapterProvider struct{ svc *adapter.Service }

func (p adapterProvider) Validate(ctx context.Context, cookie string) error {
	return p.svc.ValidateAndActivate(ctx, cookie)
}

func (p adapterProvider) List(ctx context.Context, fid string) ([]GateFile, error) {
	files, err := p.svc.List(ctx, fid)
	if err != nil {
		return nil, err
	}
	out := make([]GateFile, 0, len(files))
	for _, f := range files {
		out = append(out, GateFile{FID: f.FID, Name: f.Name, IsDir: f.IsDir()})
	}
	return out, nil
}

func (p adapterProvider) Mkdir(ctx context.Context, parent, name string) (string, error) {
	return p.svc.Mkdir(ctx, parent, name)
}

func (p adapterProvider) Upload(ctx context.Context, parent, name string, content []byte) (string, error) {
	return p.svc.Upload(ctx, adapter.UploadRequest{
		ParentFID:   parent,
		Name:        name,
		Size:        int64(len(content)),
		ContentType: "text/plain",
	}, bytes.NewReader(content), nil)
}

func (p adapterProvider) Download(ctx context.Context, fid string) ([]byte, error) {
	res, err := p.svc.DownloadURL(ctx, fid)
	if err != nil {
		return nil, err
	}
	// The signed URL is replayed with exactly the headers it was minted
	// against, the way the DAV proxy does.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, res.URL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range res.Header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (p adapterProvider) Move(ctx context.Context, fid, toParent string) error {
	return p.svc.Move(ctx, []string{fid}, toParent)
}

func (p adapterProvider) Rename(ctx context.Context, fid, name string) error {
	return p.svc.Rename(ctx, fid, name)
}

func (p adapterProvider) Delete(ctx context.Context, fid string) error {
	return p.svc.Delete(ctx, fid)
}

func (p adapterProvider) Refresh(ctx context.Context) error {
	return p.svc.RefreshSessionCookie(ctx)
}

func (p adapterProvider) ValidateExpired(ctx context.Context, cookie string) error {
	return p.svc.ValidateAndActivate(ctx, cookie)
}

func (p adapterProvider) CopySupported() bool { return p.svc.Capabilities().Copy }
