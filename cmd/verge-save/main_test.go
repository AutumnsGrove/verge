package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// entries reads a tar stream into a map keyed by name, which is what the tests
// assert on: a tarball is a set of entries, not an opaque blob.
func entries(t *testing.T, b []byte) map[string]*tar.Header {
	t.Helper()
	out := map[string]*tar.Header{}
	r := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("reading the tar stream: %v", err)
		}
		out[h.Name] = h
		if _, err := io.Copy(io.Discard, r); err != nil {
			t.Fatalf("reading %s: %v", h.Name, err)
		}
	}
}

// body returns one entry's content.
func body(t *testing.T, b []byte, name string) string {
	t.Helper()
	r := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := r.Next()
		if err != nil {
			t.Fatalf("looking for %s: %v", name, err)
		}
		if h.Name == name {
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("reading %s: %v", name, err)
			}
			return string(got)
		}
	}
}

// tree builds a workspace: a file, a nested file, a symlink, a directory, and an
// executable bit.
func tree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "repo", ".git"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "repo", "main.go"), []byte("package main\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "repo", ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\n"), 0o755))
	must(t, os.Symlink("repo/main.go", filepath.Join(root, "link")))
	return root
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("building the fixture: %v", err)
	}
}

func TestWriteTar(t *testing.T) {
	t.Run("happy/the_workspace_is_in_the_archive", func(t *testing.T) {
		root := tree(t)
		var buf bytes.Buffer
		if err := writeTar(&buf, root); err != nil {
			t.Fatalf("writeTar: %v", err)
		}

		got := entries(t, buf.Bytes())
		for _, name := range []string{"repo", "repo/main.go", "repo/.git", "repo/.git/HEAD", "run.sh", "link"} {
			if _, ok := got[name]; !ok {
				t.Fatalf("the archive has no %q: %v", name, names(got))
			}
		}
		if want := "package main\n"; body(t, buf.Bytes(), "repo/main.go") != want {
			t.Fatalf("repo/main.go = %q, want %q", body(t, buf.Bytes(), "repo/main.go"), want)
		}
	})

	t.Run("happy/relative_names_and_the_modes_that_matter", func(t *testing.T) {
		// Names are relative so the archive restores into the directory it is
		// extracted into, and the executable bit survives because a restored
		// workspace whose scripts are not executable is a workspace someone has to
		// fix by hand.
		root := tree(t)
		var buf bytes.Buffer
		if err := writeTar(&buf, root); err != nil {
			t.Fatalf("writeTar: %v", err)
		}
		got := entries(t, buf.Bytes())

		if h := got["run.sh"]; h.Mode&0o111 == 0 {
			t.Fatalf("run.sh mode = %v, want the executable bit", os.FileMode(h.Mode))
		}
		if h := got["repo/main.go"]; h.Mode&0o111 != 0 {
			t.Fatalf("repo/main.go mode = %v, want it not executable", os.FileMode(h.Mode))
		}
		if h := got["link"]; h.Typeflag != tar.TypeSymlink || h.Linkname != "repo/main.go" {
			t.Fatalf("link = type %q -> %q, want a symlink to repo/main.go", h.Typeflag, h.Linkname)
		}
		for name := range got {
			if filepath.IsAbs(name) || len(name) > 1 && name[:2] == ".." {
				t.Fatalf("entry %q is not a relative path inside the workspace", name)
			}
		}
	})

	t.Run("happy/an_empty_workspace_is_a_valid_archive", func(t *testing.T) {
		// The control plane treats zero bytes as "nothing to save"; a valid empty tar
		// is "an empty workspace", which is a different and equally true statement.
		var buf bytes.Buffer
		if err := writeTar(&buf, t.TempDir()); err != nil {
			t.Fatalf("writeTar: %v", err)
		}
		if buf.Len() == 0 {
			t.Fatal("an empty workspace produced no bytes, which reads as \"nothing to save\"")
		}
		if got := entries(t, buf.Bytes()); len(got) != 0 {
			t.Fatalf("an empty workspace produced %v", names(got))
		}
	})

	t.Run("happy/a_socket_is_not_state", func(t *testing.T) {
		// An agent's unix socket is a live process, not a file worth restoring; tar
		// cannot archive one, and failing the whole save over it would lose the
		// workspace to save the socket.
		// The socket gets its own short directory: a unix socket path is limited to
		// ~100 bytes, and a t.TempDir() under macOS's per-test path exceeds it, which
		// made this case skip on the machine it was written on.
		sockDir, err := os.MkdirTemp("/tmp", "vsock")
		if err != nil {
			t.Fatalf("making a socket directory: %v", err)
		}
		t.Cleanup(func() { os.RemoveAll(sockDir) })

		l, err := net.Listen("unix", filepath.Join(sockDir, "agent.sock"))
		if err != nil {
			t.Skipf("this platform cannot make a unix socket: %v", err)
		}
		defer l.Close()

		var buf bytes.Buffer
		if err := writeTar(&buf, sockDir); err != nil {
			t.Fatalf("writeTar: %v", err)
		}
		if _, ok := entries(t, buf.Bytes())["agent.sock"]; ok {
			t.Fatal("the socket was archived")
		}
	})

	t.Run("sad/an_unreadable_file_fails_the_save", func(t *testing.T) {
		// A save that silently skips a file it could not read is a save that says the
		// workspace is safe when it is not. The control plane's answer to a failed
		// save is to keep the machine, which is the right trade.
		if os.Geteuid() == 0 {
			t.Skip("running as root: chmod 000 does not make a file unreadable")
		}
		root := tree(t)
		secret := filepath.Join(root, "repo", "secret")
		must(t, os.WriteFile(secret, []byte("x"), 0o000))

		var buf bytes.Buffer
		err := writeTar(&buf, root)
		if err == nil {
			t.Fatal("writeTar with an unreadable file = nil, want an error")
		}
		// The permission error specifically, not whatever error a corrupt stream would
		// eventually produce: a save that skipped the file and then failed on the
		// archive would pass a test that only asked for "an error", and it would be
		// failing for the wrong reason.
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("writeTar = %v, want it to wrap fs.ErrPermission", err)
		}
	})

	t.Run("bad/a_missing_workspace", func(t *testing.T) {
		var buf bytes.Buffer
		if err := writeTar(&buf, filepath.Join(t.TempDir(), "absent")); err == nil {
			t.Fatal("writeTar of a missing directory = nil, want an error")
		}
	})
}

func names(m map[string]*tar.Header) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	return out
}

// TestMain pins the two things a save is not allowed to do: follow a symlink out of
// the workspace, and depend on the wall clock for its own success.
func TestRefusesToLeaveTheWorkspace(t *testing.T) {
	t.Run("bad/a_symlink_pointing_outside_is_archived_as_a_link", func(t *testing.T) {
		// A link is restored as a link, so a path outside the workspace never has its
		// contents copied into an archive that leaves the machine.
		root := t.TempDir()
		outside := t.TempDir()
		must(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("not ours"), 0o644))
		must(t, os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "escape")))

		var buf bytes.Buffer
		if err := writeTar(&buf, root); err != nil {
			t.Fatalf("writeTar: %v", err)
		}
		got := entries(t, buf.Bytes())
		h := got["escape"]
		if h == nil || h.Typeflag != tar.TypeSymlink {
			t.Fatalf("escape = %+v, want a symlink entry", h)
		}
		if h.Size != 0 {
			t.Fatalf("escape has %d bytes of content: the target was read", h.Size)
		}
	})

	t.Run("happy/times_are_carried", func(t *testing.T) {
		root := t.TempDir()
		path := filepath.Join(root, "f")
		must(t, os.WriteFile(path, []byte("x"), 0o644))
		when := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
		must(t, os.Chtimes(path, when, when))

		var buf bytes.Buffer
		if err := writeTar(&buf, root); err != nil {
			t.Fatalf("writeTar: %v", err)
		}
		if got := entries(t, buf.Bytes())["f"]; !got.ModTime.Equal(when) {
			t.Fatalf("mod time = %s, want %s", got.ModTime, when)
		}
	})
}
