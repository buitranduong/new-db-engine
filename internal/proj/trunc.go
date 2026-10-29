package proj

import (
	"fmt"
	"strings"
	"sync"

	"stask/internal/schema"
)

// date_trunc on timestamps (unix microseconds, UTC: the only time zone
// STASK sessions use), with PostgreSQL's units. Columns of the legacy
// "time" type hold milliseconds: forMillis adapts a unit to them. A truncated value is kept
// as a bucket number (days, months, ... since the epoch), so that GROUP BY
// date_trunc('day', t) has a small key range and takes the dense path.

const (
	msSecond = int64(1_000_000) // in microseconds
	msMinute = 60 * msSecond
	msHour   = 60 * msMinute
	msDay    = 24 * msHour
	msWeek   = 7 * msDay
)

// truncUnit is a unit: bucket maps a time to its bucket, start a bucket to
// its first microsecond.
type truncUnit struct {
	bucket func(ms int64) int64
	start  func(b int64) int64
}

func fixedUnit(w, offset int64) truncUnit {
	return truncUnit{
		bucket: func(ms int64) int64 { return floorDiv(ms+offset, w) },
		start:  func(b int64) int64 { return b*w - offset },
	}
}

// months: bucket = year*12 + month-1, grouped by n months
func monthUnit(n int64) truncUnit {
	return truncUnit{
		bucket: func(ms int64) int64 {
			y, m, _ := civil(floorDiv(ms, msDay))
			return floorDiv(y*12+m-1, n)
		},
		start: func(b int64) int64 {
			mm := b * n
			return daysFromCivil(floorDiv(mm, 12), mm-floorDiv(mm, 12)*12+1, 1) * msDay
		},
	}
}

var truncUnits = map[string]truncUnit{
	"microseconds": fixedUnit(1, 0),
	"milliseconds": fixedUnit(1000, 0),
	"second":       fixedUnit(msSecond, 0),
	"minute":       fixedUnit(msMinute, 0),
	"hour":         fixedUnit(msHour, 0),
	"day":          fixedUnit(msDay, 0),
	"week":         fixedUnit(msWeek, 3*msDay), // ISO weeks start on Monday; 1970-01-01 was a Thursday
	"month":        monthUnit(1),
	"quarter":      monthUnit(3),
	"year":         monthUnit(12),
	"decade":       monthUnit(120),
}

func lookupUnit(unit string) (truncUnit, error) {
	u, ok := truncUnits[strings.ToLower(unit)]
	if !ok {
		return u, fmt.Errorf("unit %q not recognized", unit)
	}
	return u, nil
}

// forMillis is u on a column of milliseconds.
func (u truncUnit) forMillis() truncUnit {
	return truncUnit{
		bucket: func(ms int64) int64 { return u.bucket(ms * 1000) },
		start:  func(b int64) int64 { return floorDiv(u.start(b), 1000) },
	}
}

// lookupUnitFor is lookupUnit for a column of type ct.
func lookupUnitFor(unit string, ct schema.ColType) (truncUnit, error) {
	u, err := lookupUnit(unit)
	if err == nil && ct == schema.TTime {
		u = u.forMillis()
	}
	return u, err
}

// TruncTime is date_trunc(unit, us) on microseconds.
func TruncTime(unit string, ms int64) (int64, error) {
	u, err := lookupUnit(unit)
	if err != nil {
		return 0, err
	}
	return u.start(u.bucket(ms)), nil
}

// TruncUnitOK reports whether unit is a date_trunc unit.
func TruncUnitOK(unit string) bool {
	_, err := lookupUnit(unit)
	return err == nil
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// civil and daysFromCivil convert days since 1970-01-01 to and from a
// proleptic Gregorian date (H. Hinnant, "chrono-compatible low-level date
// algorithms").
func civil(z int64) (y, m, d int64) {
	z += 719468
	era := floorDiv(z, 146097)
	doe := z - era*146097
	yoe := (doe - doe/1460 + doe/36524 - doe/146096) / 365
	y = yoe + era*400
	doy := doe - (365*yoe + yoe/4 - yoe/100)
	mp := (5*doy + 2) / 153
	d = doy - (153*mp+2)/5 + 1
	m = mp + 3
	if m > 12 {
		m -= 12
	}
	if m <= 2 {
		y++
	}
	return y, m, d
}

func daysFromCivil(y, m, d int64) int64 {
	if m <= 2 {
		y--
	}
	era := floorDiv(y, 400)
	yoe := y - era*400
	mp := (m + 9) % 12
	doy := (153*mp+2)/5 + d - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

var truncBufs sync.Pool // *[]int64

// truncColumn is a column of the buckets of time column src (same NULLs),
// whose group values are the buckets' first instants. release returns
// its buffer once the query is done with it.
func (ct *colTable) truncColumn(src *colVec, u truncUnit, q *AggQuery) (cv colVec, release func()) {
	bp, _ := truncBufs.Get().(*[]int64)
	if bp == nil {
		bp = new([]int64)
	}
	size := (ct.n + chunkMask) &^ chunkMask // whole chunks
	if cap(*bp) < size {
		*bp = make([]int64, size)
	}
	ints := (*bp)[:size]
	nchunks := (ct.n + chunkRows - 1) / chunkRows
	q.each(nchunks, func(c int) {
		out := ints[c*chunkRows : c*chunkRows+src.ints.chunkLen(c)]
		src.ints.decodeInto(c, out)
		for i, v := range out {
			out[i] = u.bucket(v)
		}
	})
	return colVec{t: src.t, ints: pvecOver(ints, ct.n), nulls: src.nulls, stored: true, derive: u.start},
		func() { truncBufs.Put(bp) }
}
