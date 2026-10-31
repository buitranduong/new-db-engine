package sql

import (
	"regexp"
	"strings"
)

type gucInfo struct {
	typ  string // bool, enum, string, integer, real
	enum string // comma separated values of an enum
}

var gucSpecial = map[string]bool{"names": true, "schema": true, "constraints": true, "role": true, "session authorization": true,
	"transaction": true, "session characteristics": true, "time zone": true, "all": true, "xml option": true}

var clientEncodingRE = regexp.MustCompile(`^(utf-?8|unicode|sql_ascii|latin[1-9]|latin10|iso[-_]?8859[-_]?\d+|win\d+|windows[-_]?\d+|koi8[ru]?|euc[-_]?\w+|big5|gbk|gb18030|gb2312|sjis|shift[-_]?jis[-_]?2004|uhc|johab|mule[-_]internal|default)$`)

// checkGUC is the validation PostgreSQL does in SET: unknown parameter (42704), a value that is not a boolean
// or one of the enum values, an unknown client_encoding (22023).
func checkGUC(name, val string) *Error {
	if strings.Contains(name, ".") || gucSpecial[name] {
		return nil
	}
	g, ok := gucTable[name]
	if !ok {
		if _, known := settingDefaults[name]; known {
			return nil
		}
		return errf(codeUndefinedObject, "unrecognized configuration parameter %q", name)
	}
	v := strings.ToLower(strings.TrimSpace(val))
	if v == "default" {
		return nil
	}
	switch g.typ {
	case "bool":
		switch v {
		case "on", "off", "true", "false", "yes", "no", "1", "0", "t", "f", "y", "n", "tr", "tru", "fa", "fal", "fals", "ye", "o":
			return nil
		}
		return errf(codeInvalidParameter, "parameter %q requires a Boolean value", name)
	case "enum":
		if strings.Contains(","+g.enum+",", ",on,") || strings.Contains(","+g.enum+",", ",true,") {
			return nil // PostgreSQL also takes boolean spellings there
		}
		for _, e := range strings.Split(g.enum, ",") {
			if e == v {
				return nil
			}
		}
		e := errf(codeInvalidParameter, "invalid value for parameter %q: %q", name, val)
		e.Hint = "Available values: " + strings.ReplaceAll(g.enum, ",", ", ") + "."
		return e
	}
	if name == "client_encoding" && !clientEncodingRE.MatchString(v) {
		return errf(codeInvalidParameter, "invalid value for parameter %q: %q", name, val)
	}
	return nil
}
