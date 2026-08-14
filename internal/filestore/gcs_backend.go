package filestore

import (
	"context"
	"fmt"
	"strings"

	"github.com/acoshift/grokwork/internal/gcs"
)

// GCS is a Backend that delegates to the gcloud storage package.
type GCS struct {
	// Run is optional; nil uses the gcs package default.
	Run gcs.Runner
}

func (b GCS) gcsTarget(t Target) gcs.Target {
	return gcs.Target{
		Bucket:          t.Bucket,
		Prefix:          t.Prefix,
		CredentialsFile: t.CredentialsFile,
	}
}

func toEntries(in []gcs.Entry) []Entry {
	if len(in) == 0 {
		return nil
	}
	out := make([]Entry, len(in))
	for i, e := range in {
		out[i] = Entry{
			Name: e.Name, IsDir: e.IsDir, Size: e.Size,
			Updated: e.Updated, ContentType: e.ContentType,
		}
	}
	return out
}

func (b GCS) List(ctx context.Context, t Target, subPath string) ([]Entry, error) {
	if strings.TrimSpace(t.Backend) != "" && t.Backend != BackendGCS {
		return nil, fmt.Errorf("filestore: gcs backend got target backend %q", t.Backend)
	}
	native, err := NativePath(subPath)
	if err != nil {
		return nil, err
	}
	es, err := gcs.List(ctx, b.Run, b.gcsTarget(t), native)
	return toEntries(es), err
}

func (b GCS) Describe(ctx context.Context, t Target, object string) (Entry, bool, error) {
	native, err := NativePath(object)
	if err != nil {
		return Entry{}, false, err
	}
	e, ok, err := gcs.Describe(ctx, b.Run, b.gcsTarget(t), native)
	if err != nil || !ok {
		return Entry{}, ok, err
	}
	return Entry{
		Name: e.Name, IsDir: e.IsDir, Size: e.Size,
		Updated: e.Updated, ContentType: e.ContentType,
	}, true, nil
}

func (b GCS) Upload(ctx context.Context, localPath string, t Target, object string, overwrite bool) error {
	native, err := NativePath(object)
	if err != nil {
		return err
	}
	if !overwrite {
		_, exists, err := gcs.Describe(ctx, b.Run, b.gcsTarget(t), native)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("object %q already exists (tick Overwrite to replace)", object)
		}
	}
	return gcs.Upload(ctx, b.Run, localPath, b.gcsTarget(t), native)
}

func (b GCS) Download(ctx context.Context, t Target, object, destPath string) error {
	native, err := NativePath(object)
	if err != nil {
		return err
	}
	return gcs.Download(ctx, b.Run, b.gcsTarget(t), native, destPath)
}

func (b GCS) Delete(ctx context.Context, t Target, object string) error {
	native, err := NativePath(object)
	if err != nil {
		return err
	}
	return gcs.Delete(ctx, b.Run, b.gcsTarget(t), native)
}
