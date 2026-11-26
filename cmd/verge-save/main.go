// Command verge-save writes a machine's workspace to stdout as a tar stream.
//
// It is the machine half of a save: the control plane runs it over ssh and spools
// what it writes (internal/syncer/ssh). It is the sibling of verge-join, and both are
// baked into the image, because nothing fetches code while a machine boots.
//
// The rules it keeps, and why each one is load-bearing:
//
//   - **Names are relative to the workspace**, so an archive restores into whatever
//     directory the next machine extracts it into. An absolute name would restore over
//     the next machine's own paths.
//   - **A symlink is archived as a symlink**, never followed. A workspace can contain a
//     link to anywhere, and an archive that copied the target's contents would carry
//     the box's files off it.
//   - **Sockets, FIFOs and devices are skipped.** They are live processes or kernel
//     handles rather than state, tar cannot represent them usefully, and failing the
//     save over an agent's socket would lose the workspace to keep the socket.
//   - **An unreadable file fails the save.** The control plane's response to a failed
//     save is to keep the machine and retry; a save that silently dropped a file would
//     say the workspace is safe when it is not.
//   - **Only regular files, directories and symlinks are written**, so a change to the
//     filesystem's contents cannot quietly add a new kind of entry to an archive the
//     control plane will restore.
package main

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// DefaultWorkspace is where the image puts the work. The argument exists so the image
// can move it without rebuilding this binary.
const DefaultWorkspace = "/workspace"

func main() {
	root := DefaultWorkspace
	if len(os.Args) > 1 {
		root = os.Args[1]
	}

	if err := writeTar(os.Stdout, root); err != nil {
		// stdout is the archive, so every diagnostic goes to stderr: a message written
		// to stdout would be stored as part of the workspace.
		fmt.Fprintln(os.Stderr, "verge-save:", err)
		os.Exit(1)
	}
}

// writeTar writes root as a tar stream.
//
// It is exported to the package's tests and nowhere else, which is why it takes a
// writer: the alternative is a test that spawns the binary and reads its stdout, which
// would be slower and would test less.
func writeTar(w io.Writer, root string) error {
	root = filepath.Clean(root)

	// The root is resolved before the walk, and this is load-bearing rather than tidy.
	// os.Stat follows a symlink but filepath.WalkDir lstats its first path, so a
	// /workspace that is a link to a mounted volume would walk zero entries and produce
	// a valid, empty archive -- which the control plane reads as "the workspace was
	// saved" and destroys the machine over. Resolving makes the walk start where the
	// content actually is; names are still relative to the workspace, because they are
	// relative to the resolved root.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("the workspace is not there: %w", err)
	}
	if info, err := os.Stat(resolved); err != nil {
		return fmt.Errorf("the workspace is not there: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("the workspace %s is a %s, not a directory", root, info.Mode().Type())
	}
	root = resolved

	tw := tar.NewWriter(w)

	// WalkDir is lexical by definition, so the same workspace produces the same order
	// every time. That is not required for correctness, and it makes two archives of
	// one workspace comparable by eye.
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory we cannot read is as bad as a file we cannot read: something
			// in the workspace would be missing from the archive.
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Slashes, not the host separator: tar names are always slash-separated, and a
		// Windows-built archive read on Linux is the failure that would find this.
		name := filepath.ToSlash(rel)

		info, err := d.Info()
		if err != nil {
			return err
		}

		switch {
		case info.IsDir(), info.Mode().IsRegular(), info.Mode()&fs.ModeSymlink != 0:
			// The three kinds this archive can represent.

		case info.Mode()&fs.ModeSocket != 0,
			info.Mode()&fs.ModeNamedPipe != 0,
			info.Mode()&fs.ModeDevice != 0,
			info.Mode()&fs.ModeCharDevice != 0:
			// Live process or kernel handle, not state. Reported so an operator can see
			// what was left out, and skipped rather than failing the workspace.
			fmt.Fprintf(os.Stderr, "verge-save: skipping %s: %s\n", name, info.Mode().Type())
			return nil

		default:
			return fmt.Errorf("%s is a %s, which a workspace archive does not carry", name, info.Mode().Type())
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return fmt.Errorf("describing %s: %w", name, err)
		}
		hdr.Name = name
		hdr.Uname, hdr.Gname = "", "" // numeric ids only: names differ between machines
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("reading the link at %s: %w", name, err)
			}
			hdr.Linkname = target
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("writing the header for %s: %w", name, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("opening %s: %w", name, err)
		}
		defer f.Close()

		// Exactly the size the header promised: a file that grew since it was described
		// would otherwise corrupt the stream, and tar's own check is a parse error on
		// whoever reads it.
		if _, err := io.CopyN(tw, f, hdr.Size); err != nil {
			return fmt.Errorf("writing %s: %w", name, err)
		}
		return nil
	})
	if walkErr != nil {
		return walkErr
	}

	// Close writes the two zero blocks that end an archive. Without it the stream is a
	// truncated tar, which reads as a workspace with a missing file.
	if err := tw.Close(); err != nil {
		return fmt.Errorf("ending the archive: %w", err)
	}
	return nil
}
