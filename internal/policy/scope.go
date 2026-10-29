package policy

import (
	"os"
	"path/filepath"
	"strings"
)

// Scope defines the workspace and resolves paths against it.
type Scope struct {
	Root string // absolute, symlink-resolved
}

func NewScope(root string) Scope {
	abs, err := filepath.Abs(root)
	if err != nil {
		abs = root
	}
	return Scope{Root: realPath(abs)}
}

// Resolve turns p into an absolute, cleaned path. Relative paths are taken
// relative to base (or the workspace root when base is empty); "~" expands to
// the home directory. Symlinks are resolved so a link cannot smuggle a path
// out of the workspace.
func (s Scope) Resolve(p, base string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if !filepath.IsAbs(p) {
		if base == "" {
			base = s.Root
		}
		p = filepath.Join(base, p)
	}
	return realPath(filepath.Clean(p))
}

// Contains reports whether the already-resolved absolute path is inside the workspace.
func (s Scope) Contains(abs string) bool {
	rel, err := filepath.Rel(s.Root, abs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// realPath resolves symlinks on the longest existing prefix of p, so it works
// for files that do not exist yet.
func realPath(p string) string {
	var tail []string
	cur := p
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				real = filepath.Join(real, tail[i])
			}
			return real
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

// IsSensitive reports whether abs is a credential or key file that the agent
// must never read or write (the model has no legitimate need for them).
func IsSensitive(abs string) bool {
	base := strings.ToLower(filepath.Base(abs))
	switch base {
	case "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519", ".netrc", ".git-credentials", ".pgpass", "shadow", "gshadow":
		return true
	}
	for _, suf := range []string{".key", ".p12", ".pfx", ".kdbx"} {
		if strings.HasSuffix(base, suf) {
			return true
		}
	}
	slash := filepath.ToSlash(abs)
	for _, frag := range []string{"/.gnupg/", "/.aws/credentials", "/.config/gcloud/", "/.kube/config", "/.docker/config.json", "/.npmrc", "/.ssh/id_", "/etc/shadow", "/etc/gshadow", "/etc/sudoers"} {
		if strings.Contains(slash+"/", frag) || strings.HasSuffix(slash, strings.TrimSuffix(frag, "/")) {
			return true
		}
	}
	return false
}

var systemPrefixes = []string{"/etc", "/usr", "/bin", "/sbin", "/lib", "/lib64", "/boot", "/var", "/opt", "/sys", "/proc", "/dev", "/root"}

// IsSystemPath reports whether abs lives in a system directory.
func IsSystemPath(abs string) bool {
	for _, p := range systemPrefixes {
		if abs == p || strings.HasPrefix(abs, p+"/") {
			return true
		}
	}
	return false
}

func homeDir() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return realPath(h), nil
}
