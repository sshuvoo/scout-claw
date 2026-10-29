//go:build windows

package filesystem

import "io/fs"

func ownerInfo(fs.FileInfo) string { return "" }
