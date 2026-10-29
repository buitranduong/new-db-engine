package cluster

import "syscall"

// The process tests start clusters, proxies and clients one after another in
// one test binary; with the default soft limit of 1024 descriptors a slow
// release of sockets showed up as "no free port block" (listen: too many open
// files). Raise the soft limit to the hard limit before any test runs.
func init() {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err == nil && rl.Cur < rl.Max {
		rl.Cur = rl.Max
		syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rl)
	}
}
