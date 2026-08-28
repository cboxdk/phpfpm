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
// sitting in a directory someone else can write to can be renamed away and
// replaced between the check and the exec — the classic swap, and this package
// hands what it checks straight to exec.CommandContext.
//
// The strict form applies when running as root, which is the threat this
// guards: an unprivileged local user arranging for a root process to execute
// something of theirs. Every ancestor must then be owned by root and writable by
// nobody else.
//
// Running as an ordinary user, only world-writable-without-sticky is refused.
// The escalation is not available — anyone who could swap a directory we own
// could equally run their own code as us — and the strict rule costs a great
// deal for nothing: Homebrew installs its whole tree group-writable, so
// /opt/homebrew/Cellar is 0775 and every php-fpm on a Mac would be refused
// outright. Observed exactly that, on a machine where the binary was perfectly
// legitimate.
//
// A world-writable directory with the sticky bit set (/tmp) is accepted either
// way. The sticky bit is the mitigation for this attack: with +t only the owner
// of an entry may rename or remove it, so the swap cannot happen. Refusing it
// would be refusing a defence rather than a hole.
func trustedAncestors(path string) error {
	strict := os.Getuid() == 0
	dir := filepath.Dir(filepath.Clean(path))

	for {
		info, err := os.Lstat(dir)
		if err != nil {
			if os.IsNotExist(err) {
				// The same leftover as a missing file, and far commoner: a
				// master whose whole directory tree has been removed. Reported
				// as such so the caller can log it quietly instead of opening
				// every run with warnings about processes nobody is managing.
				return fmt.Errorf("%w: %s", ErrPathMissing, dir)
			}

			return fmt.Errorf("cannot stat %s: %w", dir, err)
		}

		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("cannot determine ownership of %s", dir)
		}

		sticky := info.Mode()&os.ModeSticky != 0
		perm := info.Mode().Perm()

		if strict {
			if stat.Uid != 0 {
				return fmt.Errorf("%s is inside %s, which is owned by uid %d rather than root",
					path, dir, stat.Uid)
			}
			if perm&0o022 != 0 && !sticky {
				return fmt.Errorf("%s is inside %s, which is writable by others (%#o) without the sticky bit",
					path, dir, perm)
			}
		} else if perm&0o002 != 0 && !sticky {
			return fmt.Errorf("%s is inside %s, which is world-writable (%#o) without the sticky bit",
				path, dir, perm)
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}
