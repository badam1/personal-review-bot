// Package atomicfile writes the files the Settings page changes: whole, in one rename, and through a symlink.
package atomicfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Write replaces the file at path with data. When path is a symlink (into a dotfiles repo, say), the file it points
// to is the one replaced, and keeps its mode; a new file gets mode and its directory is created. The data goes to a
// temporary file beside it first and is renamed into place, so nothing ever reads half of it.
func Write(path string, data []byte, mode fs.FileMode) error {
	target, err := filepath.EvalSymlinks(path)
	switch {
	case err == nil:
		info, err := os.Stat(target)
		if err != nil {
			return err
		}
		mode = info.Mode().Perm()
	case errors.Is(err, fs.ErrNotExist):
		target = path
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
	default:
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	_, err = tmp.Write(data)
	err = errors.Join(err, tmp.Chmod(mode), tmp.Sync(), tmp.Close())
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

// Remove deletes path (a symlink itself, not what it points to); a file that isn't there is already removed.
func Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
