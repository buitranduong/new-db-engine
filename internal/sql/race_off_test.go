//go:build !race

package sql

// slack scales latency bounds of tests (see race_on_test.go).
const slack = 1
