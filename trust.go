package phpfpm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Autodiscovery finds PHP-FPM masters by scanning the process table and then
// executes the binary it found, with a config path taken from that process's
// command line. Running as root -- which autodiscovery effectively requires,
// since reading another user's /proc/<pid>/exe needs CAP_SYS_PTRACE -- that is
// a path from "any local user can start a process" to "the exporter runs their
// code as root".
//
// So a discovered binary and its config file are used only if they are owned by
// root or by us, and are not writable by anyone else. Ownership is the check
// that matters: a file only root can replace can only have been placed there by
// root.

// trustedPath reports whether a discovered path is safe to execute or read.
// ErrPathMissing reports a path that is not there at all, as opposed to one
// that is there and fails the trust checks.
var ErrPathMissing = errors.New("path does not exist")

func trustedPath(path string) error {
	// Relative paths are refused outright. A caller passing "php-fpm" would have
	// it resolved through PATH by exec, which is the attacker's variable, not
	// ours — and nothing below can check a path that is not yet decided.
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s is not an absolute path", path)
	}

	if err := trustedAncestors(path); err != nil {
		return err
	}

	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Not a trust failure. A master whose config path no longer exists
			// is a leftover — a container gone, a temp directory removed — and
			// there are usually several on any machine that has run php-fpm more
			// than once. Distinguished so the caller can log it quietly instead
			// of opening every run with a screen of warnings about processes
			// nobody is asking it to manage.
			return fmt.Errorf("%w: %s", ErrPathMissing, path)
		}

		return fmt.Errorf("cannot stat %s: %w", path, err)
	}

	// A symlink can be repointed after the check, and its own ownership says
	// nothing about the target's.
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Unknown platform: refuse rather than guess.
		return fmt.Errorf("cannot determine ownership of %s", path)
	}

	if uid := os.Getuid(); stat.Uid != 0 && uint64(stat.Uid) != uint64(uid) {
		return fmt.Errorf("%s is owned by uid %d, expected root or uid %d", path, stat.Uid, uid)
	}

	// Group- or world-writable means someone other than the owner can swap the
	// contents out from under us between this check and the exec.
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others (mode %04o)", path, perm)
	}

	return nil
}

// trustedAncestors checks the directories a path passes through.
//
// A file's own ownership is not enough. A root-owned, root-only-writable binary
// sitting in a directory anyone can write to can be renamed away and replaced
// between the check and the exec — the classic swap, and this package hands
// what it checks straight to exec.CommandContext. So every directory from the
// root down has to be owned by root or by us, and writable by nobody else.
//
// A world-writable directory with the sticky bit set (/tmp) is accepted, because
// the sticky bit is exactly the mitigation for this attack: with +t only the
// owner of an entry may rename or remove it, so the swap the check exists to
// prevent cannot happen. Refusing it would be refusing a defence rather than a
// hole.
func trustedAncestors(path string) error {
	dir := filepath.Dir(filepath.Clean(path))

	for {
		info, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("cannot stat %s: %w", dir, err)
		}

		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("cannot determine ownership of %s", dir)
		}
		if uid := os.Getuid(); stat.Uid != 0 && uint64(stat.Uid) != uint64(uid) {
			return fmt.Errorf("%s is inside %s, which is owned by uid %d", path, dir, stat.Uid)
		}
		if perm := info.Mode().Perm(); perm&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf("%s is inside %s, which is writable by others (%#o) without the sticky bit",
				path, dir, perm)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}
