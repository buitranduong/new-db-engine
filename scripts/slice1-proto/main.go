package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	mode := flag.String("mode", "proto", "proto|core")
	n := flag.Int("n", 10000000, "rows")
	out := flag.String("out", "/tmp/s1out", "output dir")
	dir := flag.String("dir", "/tmp/s1core", "core data dir")
	flag.Parse()
	os.MkdirAll(*out, 0o755)
	switch *mode {
	case "proto":
		runProto(*n, *out)
	case "h2":
		runH2(*n)
	case "core":
		runCore(*n, *dir, *out)
	default:
		fmt.Println("unknown mode")
	}
}
