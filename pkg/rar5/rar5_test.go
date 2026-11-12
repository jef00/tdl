package rar5

import (
	"bytes"
	"context"
	"encoding/binary"
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
			vols, err := Split(path, "Mövie [1]", int64(tc.volSize))
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
	vols, err := Split(path, "a", 4096)
	if err != nil {
		t.Fatal(err)
	}
	v := vols[1]
	raw := readVolume(t, v)

	if err = v.Open(); err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	for _, off := range []int64{0, 3, v.headLen - 1, v.headLen + 100, v.Size() - 5, v.Size()} {
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
	vols, err := Split(path, "a", 4096)
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
	vols, err := Split(path, "a", 4096)
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

func TestSplitVolumeTooSmall(t *testing.T) {
	path, _ := writeSource(t, "a.bin", 100)
	if _, err := Split(path, "a", 20); err == nil {
		t.Fatal("expected an error for a volume smaller than its headers")
	}
}

func TestCRC32Combine(t *testing.T) {
	data := make([]byte, 70000)
	rand.New(rand.NewSource(1)).Read(data)

	for _, cut := range []int{0, 1, 2, 255, 256, 4096, 65535, 69999, 70000} {
		a, b := data[:cut], data[cut:]
		got := crc32Combine(crc32.ChecksumIEEE(a), crc32.ChecksumIEEE(b), int64(len(b)))
		if want := crc32.ChecksumIEEE(data); got != want {
			t.Errorf("cut at %d: got %08x, want %08x", cut, got, want)
		}
	}
}

// openAll opens every volume, like an upload that works on all of them.
func openAll(t *testing.T, vols []*Volume) {
	t.Helper()
	for _, v := range vols {
		if err := v.Open(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { v.Close() })
	}
}

// readBody reads everything behind the first skip bytes of an open volume.
func readBody(t *testing.T, v *Volume, skip int64) []byte {
	t.Helper()
	if _, err := v.Seek(skip, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(v)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// readStart reads the first n bytes of an open volume.
func readStart(t *testing.T, v *Volume, n int64) []byte {
	t.Helper()
	if _, err := v.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	start := make([]byte, n)
	if _, err := io.ReadFull(v, start); err != nil {
		t.Fatal(err)
	}
	return start
}

// Reading the data first and the start of the volume last must give the same
// bytes as reading front to back, without reading any data twice: when the
// start is read, every checksum is already known.
func TestBodyFirstReadsOnce(t *testing.T) {
	path, _ := writeSource(t, "a.bin", 30000)

	ref, err := Split(path, "a", 4096)
	if err != nil {
		t.Fatal(err)
	}
	want := make([][]byte, len(ref))
	for i, v := range ref {
		want[i] = readVolume(t, v)
	}

	vols, err := Split(path, "a", 4096)
	if err != nil {
		t.Fatal(err)
	}
	openAll(t, vols)

	const skip = 512 // more than the header: some data is skipped as well
	bodies := make([][]byte, len(vols))
	for i, v := range vols {
		bodies[i] = readBody(t, v, skip)
	}
	for i, p := range vols[0].src.parts {
		if !p.done() {
			t.Fatalf("part %d: checksum covers %d of %d bytes after its data was read", i, p.hashed, p.n)
		}
	}

	for i, v := range vols {
		if err = v.WaitHead(context.Background()); err != nil {
			t.Fatal(err)
		}
		got := append(readStart(t, v, skip), bodies[i]...)
		if !bytes.Equal(got, want[i]) {
			t.Errorf("volume %d differs from the one read front to back", i)
		}
	}
}

// Volumes are uploaded concurrently; each must end up with the right bytes.
func TestConcurrentVolumes(t *testing.T) {
	path, content := writeSource(t, "a.bin", 200000)
	vols, err := Split(path, "a", 16384)
	if err != nil {
		t.Fatal(err)
	}
	openAll(t, vols)

	raws := make([][]byte, len(vols))
	errs := make(chan error, len(vols))
	for i, v := range vols {
		go func() {
			if _, err := v.Seek(1000, io.SeekStart); err != nil {
				errs <- err
				return
			}
			body, err := io.ReadAll(v)
			if err == nil {
				err = v.WaitHead(context.Background())
			}
			if err != nil {
				errs <- err
				return
			}
			start := make([]byte, 1000)
			_, _ = v.Seek(0, io.SeekStart)
			_, err = io.ReadFull(v, start)
			raws[i] = append(start, body...)
			errs <- err
		}()
	}
	for range vols {
		if err = <-errs; err != nil {
			t.Fatal(err)
		}
	}

	var joined []byte
	for i, raw := range raws {
		p := parse(t, raw)
		want := crc32.ChecksumIEEE(p.data)
		if i == len(raws)-1 {
			want = crc32.ChecksumIEEE(content)
		}
		if p.sum != want {
			t.Errorf("volume %d: crc %08x, want %08x", i, p.sum, want)
		}
		joined = append(joined, p.data...)
	}
	if !bytes.Equal(joined, content) {
		t.Error("joined volume data differs from the source")
	}
}

func TestWaitHead(t *testing.T) {
	path, _ := writeSource(t, "a.bin", 9000)
	split := func() []*Volume {
		vols, err := Split(path, "a", 4096)
		if err != nil {
			t.Fatal(err)
		}
		return vols
	}
	waitAsync := func(v *Volume) chan error {
		done := make(chan error, 1)
		go func() { done <- v.WaitHead(context.Background()) }()
		return done
	}
	stillWaiting := func(done chan error) {
		t.Helper()
		select {
		case err := <-done:
			t.Fatalf("WaitHead returned early: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}

	t.Run("only the last volume waits", func(t *testing.T) {
		vols := split()
		openAll(t, vols)
		if err := vols[0].WaitHead(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("waits for open volumes", func(t *testing.T) {
		vols := split()
		openAll(t, vols)
		done := waitAsync(vols[2])
		stillWaiting(done)
		readBody(t, vols[0], 0)
		stillWaiting(done)
		readBody(t, vols[1], vols[1].headLen)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("fails when a volume is given up", func(t *testing.T) {
		vols := split()
		openAll(t, vols)
		done := waitAsync(vols[2])
		readBody(t, vols[0], 0)
		stillWaiting(done)
		vols[1].Close()
		if err := <-done; err == nil {
			t.Fatal("WaitHead succeeded although volume 2 was never read")
		}
	})

	t.Run("does not wait for volumes nobody reads", func(t *testing.T) {
		vols := split()
		if err := vols[2].WaitHead(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("stops when canceled", func(t *testing.T) {
		vols := split()
		openAll(t, vols)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := vols[2].WaitHead(ctx); err != context.Canceled {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	})
}

// A source that changes between reading its data and building the header
// would get a checksum of data that is gone.
func TestHeaderDetectsChangedSource(t *testing.T) {
	path, _ := writeSource(t, "a.bin", 9000)
	vols, err := Split(path, "a", 4096)
	if err != nil {
		t.Fatal(err)
	}
	openAll(t, vols)
	readBody(t, vols[0], vols[0].headLen)

	if err = os.Chtimes(path, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = vols[0].Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err = vols[0].Read(make([]byte, 16)); err == nil {
		t.Fatal("read a header for a source that changed")
	}
}
