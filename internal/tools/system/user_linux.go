//go:build linux

package system

import (
	"os/user"
	"strconv"
)

var userCache = map[uint32]string{}

func lookupUser(uid uint32) string {
	if n, ok := userCache[uid]; ok {
		return n
	}
	name := strconv.FormatUint(uint64(uid), 10)
	if u, err := user.LookupId(name); err == nil {
		name = u.Username
	}
	userCache[uid] = name
	return name
}
