// Package atomicfile durably replaces a file's contents.
//
// It exists because three separate stores now persist a single JSON file that
// is the system of record for something the process cannot rebuild —
// config.json (every project, allowlist, channel map and credential),
// sessions.json (every thread/session/PR link) and identity-links.json (which
// logins belong to one person). The logic was duplicated verbatim in two of
// them; a third caller is the point where one copy has to become the copy.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write durably replaces path with data.
//
// A bare os.WriteFile is not good enough for a system-of-record file: it
// truncates the existing file before writing the new bytes, and a crash or
// full disk mid-write leaves a truncated, unparseable file with no way back.
// A truncated config.json is not a lost setting, it is a bot that will not
// boot.
//
// Instead we write to a temp file in the SAME directory as path (rename is
// only atomic within one filesystem — a temp dir on another mount would
// defeat that), fsync the temp file so its bytes are durable, rename it over
// path (an atomic swap on POSIX filesystems: readers always see either the
// whole old file or the whole new one, never a partial write), and then
// fsync the parent directory too.
//
// That last fsync looks redundant to a future reader who already fsynced
// the file and will be tempted to delete it as dead code — it is not. The
// file fsync only guarantees the new file's *contents* are durable; the
// rename is a change to the *directory's* metadata (the name now points at
// the new inode), and POSIX filesystems do not guarantee a directory entry
// change survives a power loss unless the directory itself is fsynced. Skip
// it and a crash right after a "successful" rename can still resurrect the
// old file, or leave a directory entry pointing at nothing, even though the
// new file's bytes made it to disk.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup: once the rename below succeeds tmpPath no longer
	// exists and this is a silent no-op; it only matters on an early return.
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}

	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
