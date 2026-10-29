// Package conf loads settings from a file into a flag set. A line is
// "name = value" (or "name value"), where name is a flag of the set; blank
// lines and lines starting with # are ignored. Flags given on the command
// line keep their values: the file only replaces defaults.
package conf

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
)

// Load applies the file at path to fs, which must already be parsed. An
// unknown name or an invalid value is an error naming the line.
func Load(fs *flag.FlagSet, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	explicit := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { explicit[fl.Name] = true })
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			name, value, _ = strings.Cut(line, " ")
		}
		name, value = strings.TrimSpace(name), strings.Trim(strings.TrimSpace(value), `"`)
		if fs.Lookup(name) == nil {
			return fmt.Errorf("%s:%d: unknown setting %q", path, n, name)
		}
		if explicit[name] {
			continue
		}
		if err := fs.Set(name, value); err != nil {
			return fmt.Errorf("%s:%d: %s: %v", path, n, name, err)
		}
	}
	return sc.Err()
}
