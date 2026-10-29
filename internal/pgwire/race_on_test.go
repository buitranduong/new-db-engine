//go:build race

package pgwire

// slack scales latency bounds of tests: the race detector slows row
// processing about tenfold.
const slack = 10
