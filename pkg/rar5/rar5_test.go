package rar5

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// parsed is what a RAR reader sees in one volume.
type parsed struct {
	volume, numbered bool
	number           uint64
	fileFlags        uint64 // common header flags of the file header
	unpSize          uint64
	sum              uint32
	name             string
	data             []byte
	notLast          bool
}

func readVint(t *testing.T, r *bytes.Reader) uint64 {
	t.Helper()
	v, err := binary.ReadUvarint(r) // same encoding as a RAR vint
	if err != nil {
		t.Fatalf("vint: %v", err)
	}
	return v
}

// parse walks a volume the way the format technote describes it and checks
// every header CRC.
func parse(t *testing.T, raw []byte) parsed {
	t.Helper()
	if !bytes.HasPrefix(raw, signature) {
		t.Fatal("missing RAR 5 signature")
	}
	r := bytes.NewReader(raw[len(signature):])

	var p parsed
	for r.Len() > 0 {
		var sum uint32
		if err := binary.Read(r, binary.LittleEndian, &sum); err != nil {
			t.Fatal(err)
		}
		start := r.Size() - int64(r.Len())
		size := readVint(t, r)
		bodyStart := r.Size() - int64(r.Len())
		end := bodyStart + int64(size)
		if got := crc32.ChecksumIEEE(raw[int64(len(signature))+start : int64(len(signature))+end]); got != sum {
			t.Fatalf("header crc: got %08x, want %08x", got, sum)
		}

		typ, flags := readVint(t, r), readVint(t, r)
		var dataSize uint64
		if flags&flagData != 0 {
			dataSize = readVint(t, r)
		}

		switch typ {
		case headMain:
			af := readVint(t, r)
			p.volume, p.numbered = af&mainVolume != 0, af&mainVolNumber != 0
			if p.numbered {
				p.number = readVint(t, r)
			}
		case headFile:
			p.fileFlags = flags
			if ff := readVint(t, r); ff != fileUnixTime|fileCRC32 {
				t.Fatalf("file flags: %x", ff)
			}
			p.unpSize = readVint(t, r)
			readVint(t, r) // attributes
			var mtime uint32
			_ = binary.Read(r, binary.LittleEndian, &mtime)
			_ = binary.Read(r, binary.LittleEndian, &p.sum)
			if comp := readVint(t, r); comp != 0 {
				t.Fatalf("compression info: %x, want store", comp)
			}
			readVint(t, r) // host os
			name := make([]byte, readVint(t, r))
			_, _ = io.ReadFull(r, name)
			p.name = string(name)
		case headEnd:
			p.notLast = readVint(t, r)&endNotLast != 0
		default:
			t.Fatalf("unexpected header type %d", typ)
		}

		if pos := r.Size() - int64(r.Len()); pos != end {
			t.Fatalf("header type %d: parsed to %d, header ends at %d", typ, pos, end)
		}
		if dataSize > 0 {
			p.data = make([]byte, dataSize)
			if _, err := io.ReadFull(r, p.data); err != nil {
				t.Fatalf("data: %v", err)
			}
		}
	}
	return p
}

func writeSource(t *testing.T, name string, size int) (string, []byte) {
	t.Helper()
	content := make([]byte, size)
	rand.New(rand.NewSource(int64(size))).Read(content)
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, content
}

func readVolume(t *testing.T, v *Volume) []byte {
	t.Helper()
	if err := v.Open(); err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	raw, err := io.ReadAll(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSplit(t *testing.T) {
	for _, tc := range []struct {
		size, volSize int
		volumes       int
	}{
		{size: 5000, volSize: 2000, volumes: 3},
		{size: 30000, volSize: 2048, volumes: 16}, // zero padded names
		{size: 1000, volSize: 4096, volumes: 1},
		{size: 0, volSize: 4096, volumes: 1},
	} {
		t.Run(fmt.Sprintf("%d_%d", tc.size, tc.volSize), func(t *testing.T) {
			path, content := writeSource(t, "Mövie [1].mkv", tc.size)
			vols, err := Split(context.Background(), path, "Mövie [1]", int64(tc.volSize))
			if err != nil {
				t.Fatal(err)
			}
			if len(vols) != tc.volumes {
				t.Fatalf("got %d volumes, want %d", len(vols), tc.volumes)
			}

			var joined []byte
			width := len(fmt.Sprint(len(vols)))
			for i, v := range vols {
				raw := readVolume(t, v)
				last := i == len(vols)-1

				if want := fmt.Sprintf("Mövie [1].part%0*d.rar", width, i+1); v.Name() != want {
					t.Errorf("name: got %q, want %q", v.Name(), want)
				}
				if int64(len(raw)) != v.Size() || len(raw) > tc.volSize {
					t.Errorf("volume %d: %d bytes, Size %d, limit %d", i, len(raw), v.Size(), tc.volSize)
				}
				if !last && tc.volSize-len(raw) > 8 {
					t.Errorf("volume %d wastes %d bytes", i, tc.volSize-len(raw))
				}

				p := parse(t, raw)
				if p.name != "Mövie [1].mkv" || p.unpSize != uint64(tc.size) {
					t.Errorf("volume %d: entry %q size %d", i, p.name, p.unpSize)
				}
				if p.volume != (len(vols) > 1) || p.numbered != (i > 0) || p.number != uint64(i)*b2u(i > 0) {
					t.Errorf("volume %d: main header volume=%v numbered=%v number=%d", i, p.volume, p.numbered, p.number)
				}
				if (p.fileFlags&flagSplitBefore != 0) != (i > 0) || (p.fileFlags&flagSplitAfter != 0) != !last || p.notLast != !last {
					t.Errorf("volume %d: split flags %x, notLast %v", i, p.fileFlags, p.notLast)
				}

				want := crc32.ChecksumIEEE(p.data) // packed data of this volume
				if last {
					want = crc32.ChecksumIEEE(content) // whole file
				}
				if p.sum != want {
					t.Errorf("volume %d: crc %08x, want %08x", i, p.sum, want)
				}
				joined = append(joined, p.data...)
			}

			if !bytes.Equal(joined, content) {
				t.Error("joined volume data differs from the source")
			}
		})
	}
}

func b2u(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// The end of archive header of a last volume is the same in every RAR 5
// archive; matching it proves the header framing and CRC.
func TestTailMatchesRar(t *testing.T) {
	want := []byte{0x1D, 0x77, 0x56, 0x51, 0x03, 0x05, 0x04, 0x00}
	if got := tail(0, 1); !bytes.Equal(got, want) {
		t.Fatalf("got % x, want % x", got, want)
	}
}

func TestVolumeSeek(t *testing.T) {
	path, _ := writeSource(t, "a.bin", 9000)
	vols, err := Split(context.Background(), path, "a", 4096)
	if err != nil {
		t.Fatal(err)
	}
	v := vols[1]
	raw := readVolume(t, v)

	if err = v.Open(); err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	for _, off := range []int64{0, 3, int64(len(v.head)) - 1, int64(len(v.head)) + 100, v.Size() - 5, v.Size()} {
		if _, err = v.Seek(off, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(v)
		if err != nil || !bytes.Equal(got, raw[off:]) {
			t.Fatalf("read from %d: err %v, %d bytes, want %d", off, err, len(got), len(raw)-int(off))
		}
	}
	if end, _ := v.Seek(0, io.SeekEnd); end != v.Size() {
		t.Fatalf("SeekEnd: %d, want %d", end, v.Size())
	}
}

func TestVolumeDetectsChangedSource(t *testing.T) {
	path, _ := writeSource(t, "a.bin", 9000)
	vols, err := Split(context.Background(), path, "a", 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(path, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = vols[0].Open(); err == nil {
		vols[0].Close()
		t.Fatal("Open succeeded on a changed source")
	}
}

func TestWriteFile(t *testing.T) {
	path, _ := writeSource(t, "a.bin", 9000)
	vols, err := Split(context.Background(), path, "a", 4096)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out, err := vols[0].WriteFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if out != filepath.Join(dir, "a.part1.rar") || !bytes.Equal(got, readVolume(t, vols[0])) {
		t.Fatalf("WriteFile wrote %s (%d bytes)", out, len(got))
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("left %d entries, want only the volume", len(entries))
	}
}

func TestSplitCanceled(t *testing.T) {
	path, _ := writeSource(t, "a.bin", 9000)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Split(ctx, path, "a", 4096); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

func TestSplitVolumeTooSmall(t *testing.T) {
	path, _ := writeSource(t, "a.bin", 100)
	if _, err := Split(context.Background(), path, "a", 20); err == nil {
		t.Fatal("expected an error for a volume smaller than its headers")
	}
}
