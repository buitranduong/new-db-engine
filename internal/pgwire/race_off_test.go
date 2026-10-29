//go:build !race

package pgwire

// slack scales latency bounds of tests (see race_on_test.go).
const slack = 1
