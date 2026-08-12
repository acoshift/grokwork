package filestore

import (
	"context"
	"fmt"
	"strings"

	"github.com/acoshift/grokwork/internal/gdrive"
)

// GDrive is a Backend that delegates to the Drive REST client.
type GDrive struct {
	// Client is required.
	Client *gdrive.Client
}

func (b GDrive) driveTarget(t Target) gdrive.Target {
	return gdrive.Target{
		FolderID:         t.FolderID,
		IsolationSegment: t.IsolationSegment,
		CredentialsFile:  t.CredentialsFile,
	}
}

func (b GDrive) check(t Target) error {
	if b.Client == nil {
		return fmt.Errorf("filestore: gdrive client is nil")
	}
	if strings.TrimSpace(t.Backend) != "" && t.Backend != BackendGDrive {
		return fmt.Errorf("filestore: gdrive backend got target backend %q", t.Backend)
	}
	return nil
}

func toDriveEntries(in []gdrive.Entry) []Entry {
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

func (b GDrive) List(ctx context.Context, t Target, subPath string) ([]Entry, error) {
	if err := b.check(t); err != nil {
		return nil, err
	}
	if err := ValidateObjectPath(subPath); err != nil {
		return nil, err
	}
	es, err := b.Client.List(ctx, b.driveTarget(t), subPath)
	return toDriveEntries(es), err
}

func (b GDrive) Describe(ctx context.Context, t Target, object string) (Entry, bool, error) {
	if err := b.check(t); err != nil {
		return Entry{}, false, err
	}
	if err := ValidateObjectPath(object); err != nil {
		return Entry{}, false, err
	}
	e, ok, err := b.Client.Describe(ctx, b.driveTarget(t), object)
	if err != nil || !ok {
		return Entry{}, ok, err
	}
	return Entry{
		Name: e.Name, IsDir: e.IsDir, Size: e.Size,
		Updated: e.Updated, ContentType: e.ContentType,
	}, true, nil
}

func (b GDrive) Upload(ctx context.Context, localPath string, t Target, object string, overwrite bool) error {
	if err := b.check(t); err != nil {
		return err
	}
	if err := ValidateObjectPath(object); err != nil {
		return err
	}
	return b.Client.Upload(ctx, localPath, b.driveTarget(t), object, overwrite)
}

func (b GDrive) Download(ctx context.Context, t Target, object, destPath string) error {
	if err := b.check(t); err != nil {
		return err
	}
	if err := ValidateObjectPath(object); err != nil {
		return err
	}
	return b.Client.Download(ctx, b.driveTarget(t), object, destPath)
}

func (b GDrive) Delete(ctx context.Context, t Target, object string) error {
	if err := b.check(t); err != nil {
		return err
	}
	if err := ValidateObjectPath(object); err != nil {
		return err
	}
	return b.Client.Delete(ctx, b.driveTarget(t), object)
}
