//go:build !windows

package filesystem

import (
	"fmt"
	"io/fs"
	"os/user"
	"syscall"
)

func ownerInfo(fi fs.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	owner := fmt.Sprint(st.Uid)
	if u, err := user.LookupId(owner); err == nil {
		owner = u.Username
	}
	group := fmt.Sprint(st.Gid)
	if g, err := user.LookupGroupId(group); err == nil {
		group = g.Name
	}
	return fmt.Sprintf("owner: %s:%s\n", owner, group)
}
