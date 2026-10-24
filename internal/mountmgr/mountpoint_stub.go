//go:build !unix

package mountmgr

// IsMountPoint 在非 unix 平台恒为 false（p37 的生产与开发平台均为 unix）。
func IsMountPoint(string) bool { return false }
