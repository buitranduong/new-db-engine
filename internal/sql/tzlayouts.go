package sql

// tzLayouts are timestamp/date spellings with a zone suffix that drivers
// send: pgjdbc setDate sends '2026-03-04 +00'. Appended to timeLayouts.
var tzLayouts = []string{
	"2006-01-02 Z07",
	"2006-01-02 Z07:00",
	"2006-01-02 -07",
	"2006-01-02 -07:00",
	"2006-01-02 15:04:05.999999999 -07",
	"2006-01-02 15:04:05.999999999 -07:00",
	"2006-01-02 15:04:05.999999999-07",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999-0700",
	"2006-01-02 15:04Z07",
}

func init() { timeLayouts = append(timeLayouts, tzLayouts...) }
