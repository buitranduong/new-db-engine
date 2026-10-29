// Command segzip measures block compression of log segments: ratio and
// compress / decompress speed per codec and block size, to choose the
// format of compressed sealed segments.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/klauspost/compress/s2"
	"github.com/klauspost/compress/zstd"
)

func main() {
	path := flag.String("seg", "", "segment file")
	flag.Parse()
	raw, err := os.ReadFile(*path)
	if err != nil {
		log.Fatal(err)
	}
	// the written part: frames up to the zero end marker
	end := 0
	for end+8 <= len(raw) {
		n := int(binary.LittleEndian.Uint32(raw[end:]))
		if n == 0 {
			break
		}
		end += 8 + n
	}
	raw = raw[:end]
	fmt.Printf("segment %s: %d bytes written\n", *path, len(raw))
	type codec struct {
		name string
		enc  func(dst, src []byte) []byte
		dec  func(dst, src []byte) ([]byte, error)
	}
	zenc := func(l zstd.EncoderLevel) func(dst, src []byte) []byte {
		e, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(l), zstd.WithEncoderConcurrency(1))
		return func(dst, src []byte) []byte { return e.EncodeAll(src, dst[:0]) }
	}
	zd, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	zdec := func(dst, src []byte) ([]byte, error) { return zd.DecodeAll(src, dst[:0]) }
	codecs := []codec{
		{"s2", func(dst, src []byte) []byte { return s2.Encode(dst[:0], src) }, func(dst, src []byte) ([]byte, error) { return s2.Decode(dst[:cap(dst)], src) }},
		{"s2-better", func(dst, src []byte) []byte { return s2.EncodeBetter(dst[:0], src) }, func(dst, src []byte) ([]byte, error) { return s2.Decode(dst[:cap(dst)], src) }},
		{"zstd-fastest", zenc(zstd.SpeedFastest), zdec},
		{"zstd-default", zenc(zstd.SpeedDefault), zdec},
		{"zstd-better", zenc(zstd.SpeedBetterCompression), zdec},
	}
	for _, bs := range []int{16 << 10, 64 << 10, 256 << 10} {
		for _, c := range codecs {
			var blocks [][]byte
			total := 0
			t0 := time.Now()
			var buf []byte
			for off := 0; off < len(raw); off += bs {
				b := raw[off:min(off+bs, len(raw))]
				buf = c.enc(make([]byte, 0, len(b)), b)
				blocks = append(blocks, buf)
				total += len(buf)
			}
			ct := time.Since(t0)
			out := make([]byte, 0, bs)
			t1 := time.Now()
			var got bytes.Buffer
			for i, b := range blocks {
				d, err := c.dec(out, b)
				if err != nil {
					log.Fatal(err)
				}
				if i < 3 {
					got.Write(d)
				}
			}
			dt := time.Since(t1)
			if !bytes.Equal(got.Bytes(), raw[:min(len(raw), 3*bs)]) {
				log.Fatalf("%s: round trip differs", c.name)
			}
			mb := float64(len(raw)) / 1e6
			fmt.Printf("block %4dKB %-13s ratio %5.2fx  size %6.1f MB  compress %7.0f MB/s  decompress %7.0f MB/s  per-block decode %6.1f us\n",
				bs>>10, c.name, float64(len(raw))/float64(total), float64(total)/1e6, mb/ct.Seconds(), mb/dt.Seconds(),
				float64(dt.Microseconds())/float64(len(blocks)))
		}
	}
}
