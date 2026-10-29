// Package buildinfo reports which source revision a binary was built from
// (Go stamps vcs.* settings into binaries built with go build inside a git
// tree). staskd publishes it in /v1/status so that benchmarks can refuse to
// measure a binary older than the source (docs/14).
package buildinfo

import "runtime/debug"

type Info struct {
	Revision string `json:"revision"`       // vcs.revision, "" when not stamped
	Modified bool   `json:"modified"`       // vcs.modified: built from a dirty tree
	Time     string `json:"time,omitempty"` // vcs.time of the commit
	Go       string `json:"go"`             // toolchain version
	Path     string `json:"path,omitempty"` // main package path
}

// Read returns the running binary build info. Binaries from go run, go test
// or a build outside git have an empty Revision.
func Read() Info {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return Info{}
	}
	in := Info{Go: bi.GoVersion, Path: bi.Path}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			in.Revision = s.Value
		case "vcs.modified":
			in.Modified = s.Value == "true"
		case "vcs.time":
			in.Time = s.Value
		}
	}
	return in
}
