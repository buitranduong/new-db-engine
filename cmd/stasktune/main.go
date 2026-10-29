// stasktune detects the machine (usable CPUs and memory, also inside
// containers; the disk under the data directory) and benchmarks that disk
// briefly, then writes a staskd.conf that uses all of it: staskd -config.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"stask/internal/tune"
)

func main() {
	dir := flag.String("dir", "", "data directory staskd will use (must exist; the disk benchmark writes a temporary file there)")
	out := flag.String("out", "", "write the settings to this file (default: standard output)")
	secs := flag.Float64("seconds", 15, "disk benchmark duration (0 = skip; disk settings stay defaults)")
	force := flag.Bool("force", false, "overwrite -out if it exists")
	flag.Parse()
	if *dir == "" {
		log.Fatal("-dir is required")
	}
	if st, err := os.Stat(*dir); err != nil || !st.IsDir() {
		log.Fatalf("-dir %s: not a directory", *dir)
	}
	f, err := tune.Gather(*dir, time.Duration(*secs*float64(time.Second)))
	if err != nil {
		log.Fatal(err)
	}
	conf := tune.Render(f)
	if *out == "" {
		fmt.Print(conf)
		return
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if *force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	w, err := os.OpenFile(*out, flags, 0o644)
	if err != nil {
		log.Fatalf("%v (use -force to overwrite)", err)
	}
	if _, err := w.WriteString(conf); err != nil {
		log.Fatal(err)
	}
	if err := w.Close(); err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s", *out)
}
