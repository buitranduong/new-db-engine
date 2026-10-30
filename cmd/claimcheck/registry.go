package main

// extraChecks collects probes registered by probes_*.go files in their init
// functions, so that branches adding probes do not all edit checks():
//
//	func init() { extraChecks = append(extraChecks, check{...}, ...) }
var extraChecks []check
