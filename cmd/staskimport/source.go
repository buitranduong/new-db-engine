package main

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/xuri/excelize/v2"
)

// rowSource yields the rows of a file one at a time (constant memory for csv;
// for xlsx excelize streams the sheet XML and holds the shared-string table).
type rowSource interface {
	// next returns the cells of the next row. rowErr is a row that cannot be
	// read (a malformed csv record): reported, not fatal. io.EOF ends the file.
	next() (cells []string, rowErr error, err error)
	close() error
}

func openSource(path string, sp *Spec) (rowSource, error) {
	switch strings.ToLower(ext(path)) {
	case ".csv", ".tsv", ".txt":
		return openCSV(path, sp)
	case ".xlsx", ".xlsm":
		return openXLSX(path, sp)
	}
	return nil, fmt.Errorf("%s: unknown file type (want .csv or .xlsx)", path)
}

func ext(p string) string {
	if i := strings.LastIndexByte(p, '.'); i >= 0 {
		return p[i:]
	}
	return ""
}

type csvSource struct {
	f *os.File
	r *csv.Reader
}

func openCSV(path string, sp *Spec) (*csvSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(f, 1<<20)
	if b, err := br.Peek(3); err == nil && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		br.Discard(3)
	}
	r := csv.NewReader(br)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.ReuseRecord = true
	switch {
	case sp.Delimiter != "":
		rs := []rune(sp.Delimiter)
		if len(rs) != 1 {
			f.Close()
			return nil, fmt.Errorf("delimiter must be one character")
		}
		r.Comma = rs[0]
	case strings.EqualFold(ext(path), ".tsv"):
		r.Comma = '\t'
	}
	return &csvSource{f, r}, nil
}

func (s *csvSource) next() ([]string, error, error) {
	rec, err := s.r.Read()
	if err == io.EOF {
		return nil, nil, io.EOF
	}
	var pe *csv.ParseError
	if errors.As(err, &pe) {
		return nil, err, nil
	}
	return rec, nil, err
}
func (s *csvSource) close() error { return s.f.Close() }

type xlsxSource struct {
	f    *excelize.File
	rows *excelize.Rows
}

func openXLSX(path string, sp *Spec) (*xlsxSource, error) {
	f, err := excelize.OpenFile(path, excelize.Options{RawCellValue: true})
	if err != nil {
		return nil, err
	}
	sheet := sp.Sheet
	if sheet == "" {
		sheet = f.GetSheetName(0)
	}
	rows, err := f.Rows(sheet)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("sheet %q: %w", sheet, err)
	}
	return &xlsxSource{f, rows}, nil
}

func (s *xlsxSource) next() ([]string, error, error) {
	if !s.rows.Next() {
		if err := s.rows.Error(); err != nil {
			return nil, nil, err
		}
		return nil, nil, io.EOF
	}
	cols, err := s.rows.Columns(excelize.Options{RawCellValue: true})
	if err != nil {
		return nil, err, nil
	}
	return cols, nil, nil
}
func (s *xlsxSource) close() error { s.rows.Close(); return s.f.Close() }

// rawRow is one data row of the file.
type rawRow struct {
	n     int64 // data row ordinal, 1-based, counting blank and bad rows
	line  int64 // row number as a person sees it in the file (header and skipped rows counted)
	cells []string
	bad   error
}

// readBatches sends batches of up to size data rows; rows with ordinal <= skip
// are not sent (resume). It resolves the spec's columns from the header first
// and reports the result on ready.
func readBatches(src rowSource, sp *Spec, size int, skip int64, out chan<- []rawRow, ready chan<- error, rerr *error, stop <-chan struct{}) {
	defer close(out)
	line := int64(0)
	for i := 0; i < sp.SkipRows; i++ {
		if _, _, err := src.next(); err != nil {
			ready <- fmt.Errorf("file ends within skip_rows: %v", err)
			return
		}
		line++
	}
	if !sp.NoHeader {
		var h []string
		for {
			c, bad, err := src.next()
			line++
			if err != nil {
				ready <- fmt.Errorf("reading the header: %v", err)
				return
			}
			if bad != nil {
				ready <- fmt.Errorf("header: %v", bad)
				return
			}
			h = append([]string(nil), c...)
			break
		}
		if err := sp.resolve(h); err != nil {
			ready <- err
			return
		}
	} else if err := sp.resolve(nil); err != nil {
		ready <- err
		return
	}
	ready <- nil
	var batch []rawRow
	var n int64
	send := func() bool {
		if len(batch) == 0 {
			return true
		}
		select {
		case out <- batch:
			batch = nil
			return true
		case <-stop:
			return false
		}
	}
	for {
		c, bad, err := src.next()
		line++
		if err == io.EOF {
			send()
			return
		}
		if err != nil {
			send()
			*rerr = fmt.Errorf("reading row %d: %w", line, err) // set before out is closed
			return
		}
		n++
		if n <= skip {
			continue
		}
		r := rawRow{n: n, line: line, bad: bad}
		if bad == nil {
			r.cells = append([]string(nil), c...)
		}
		batch = append(batch, r)
		if len(batch) >= size && !send() {
			return
		}
	}
}
