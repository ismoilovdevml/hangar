//go:build !unix

package observability

// diskUsage is not implemented on this platform; fs store free space is
// then not reported.
func diskUsage(string) (size, free uint64, ok bool) { return 0, 0, false }
