// Package bulkconvert handles the "everything we haven't modularised yet"
// half of the build. Given a source xkb tree (typically
// /usr/share/xkeyboard-config-2 or a vendored copy) and a set of
// modular-source-controlled file names, it copies every other file from the
// source tree into the output directory verbatim. The output is therefore
// always a complete drop-in xkb tree: native flexkb files override the
// upstream ones, the rest fall through.
package bulkconvert

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CopyTree copies every file under srcRoot into dstRoot, skipping files
// whose relative path appears in skip. Directories are created as needed.
// skip keys are relative paths from srcRoot, e.g. "symbols/us".
func CopyTree(srcRoot, dstRoot string, skip map[string]bool) error {
	return filepath.Walk(srcRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dstRoot, rel), 0o755)
		}
		if skip[rel] {
			return nil
		}
		return copyFile(path, filepath.Join(dstRoot, rel), info.Mode().Perm())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	return nil
}
