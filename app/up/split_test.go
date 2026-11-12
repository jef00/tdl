package up

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gabriel-vasile/mimetype"
)

func TestAlignSplitSize(t *testing.T) {
	cases := []struct{ size, limit, want int64 }{
		{0, SplitLimitDefault, SplitLimitDefault},
		{0, SplitLimitPremium, SplitLimitPremium},
		{SplitLimitPremium, SplitLimitDefault, SplitLimitDefault}, // clamp to limit
		{3900 * 1024 * 1024, SplitLimitPremium, 3900 * 1024 * 1024},
		{splitAlign + 1, SplitLimitDefault, splitAlign},
		{1, SplitLimitDefault, splitAlign},
	}
	for _, c := range cases {
		if got := alignSplitSize(c.size, c.limit); got != c.want {
			t.Errorf("alignSplitSize(%d, %d) = %d, want %d", c.size, c.limit, got, c.want)
		}
	}
}

func TestSplitGroupDone(t *testing.T) {
	g := &splitGroup{}
	g.remaining.Store(3)
	if g.done(nil) || g.done(nil) {
		t.Fatal("finished before last part")
	}
	if !g.done(nil) {
		t.Fatal("last successful part should finish the group")
	}

	g = &splitGroup{}
	g.remaining.Store(2)
	g.done(errors.New("boom"))
	if g.done(nil) {
		t.Fatal("group with a failed part must not finish")
	}
}

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestSplitterPack(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "movie.mkv")
	small := filepath.Join(dir, "small.mkv")
	writeFile(t, big, 40000)
	writeFile(t, small, 100)

	s := newSplitter(16384, false)

	if vols, err := s.pack(&File{File: small}); err != nil || vols != nil {
		t.Fatalf("small file: got %v, %v; want it left alone", vols, err)
	}

	vols, err := s.pack(&File{File: big})
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 3 {
		t.Fatalf("got %d volumes, want 3", len(vols))
	}

	total := int64(0)
	for i, f := range vols {
		want := filepath.Join(dir, "movie.part"+string(rune('1'+i))+".rar")
		if f.File != want || f.Part != i+1 || f.Parts != 3 || f.vol == nil || f.group != vols[0].group {
			t.Errorf("volume %d = %+v, want %s part %d/3 in one group", i, f, want, i+1)
		}
		if f.vol.Size() > 16384 {
			t.Errorf("volume %d is %d bytes, over the limit", i, f.vol.Size())
		}

		// readable the way the uploader reads it
		if err = f.vol.Open(); err != nil {
			t.Fatal(err)
		}
		n, err := io.Copy(io.Discard, f.vol)
		_ = f.vol.Close()
		if err != nil || n != f.vol.Size() {
			t.Errorf("volume %d: read %d of %d bytes: %v", i, n, f.vol.Size(), err)
		}
		total += n
	}
	if total <= 40000 {
		t.Errorf("volumes hold %d bytes, less than the source", total)
	}
	if g := vols[0].group; g.source != big || g.remaining.Load() != 3 {
		t.Errorf("group = %+v", g)
	}

	// the whole point: splitting leaves nothing behind on disk
	if names := dirNames(t, dir); len(names) != 2 {
		t.Errorf("source dir holds %v, want only the two sources", names)
	}
}

// Sources that would get the same volume names fall back to the full file name.
func TestSplitterPackNameClash(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Show.mkv", "show.mp4"} {
		writeFile(t, filepath.Join(dir, name), 40000)
	}

	s := newSplitter(16384, false)
	a, err := s.pack(&File{File: filepath.Join(dir, "Show.mkv")})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.pack(&File{File: filepath.Join(dir, "show.mp4")})
	if err != nil {
		t.Fatal(err)
	}

	if got := filepath.Base(a[0].File); got != "Show.part1.rar" {
		t.Errorf("first source: %s", got)
	}
	if got := filepath.Base(b[0].File); got != "show.mp4.part1.rar" {
		t.Errorf("clashing source: %s", got)
	}
}

// Oversize files are split only when the iterator reaches them, and no
// volume is ever written to disk.
func TestIterPacksLazily(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mkv")
	small := filepath.Join(dir, "small.mkv")
	b := filepath.Join(dir, "b.mkv")
	writeFile(t, a, 40000)
	writeFile(t, b, 40000)
	writeFile(t, small, 4)

	var logged []string
	split := newSplitter(16384, false)
	split.log = func(msg string) { logged = append(logged, msg) }
	it := &iter{
		files: []*File{{File: a}, {File: small}, {File: b}},
		split: split,
	}

	want := []string{
		filepath.Join(dir, "a.part1.rar"),
		filepath.Join(dir, "a.part2.rar"),
		filepath.Join(dir, "a.part3.rar"),
		small,
		filepath.Join(dir, "b.part1.rar"),
		filepath.Join(dir, "b.part2.rar"),
		filepath.Join(dir, "b.part3.rar"),
	}
	for n, w := range want {
		f, err := it.advance()
		if err != nil {
			t.Fatal(err)
		}
		if f.File != w {
			t.Fatalf("file %d = %s, want %s", n, f.File, w)
		}
		if (f.vol != nil) != (w != small) {
			t.Fatalf("file %d: volume = %v", n, f.vol != nil)
		}
		// b.mkv must not be read until a.mkv and small.mkv were handed out
		if n < 4 && len(logged) != 1 {
			t.Fatalf("b.mkv split too early, at file %d: %v", n, logged)
		}
	}
	if len(it.pending) != 0 || it.cur != len(it.files) {
		t.Errorf("iterator not drained: pending=%d cur=%d", len(it.pending), it.cur)
	}
	if names := dirNames(t, dir); len(names) != 3 {
		t.Errorf("source dir holds %v, want only the three sources", names)
	}
}

func TestWalkSkipsLegacyStageDirs(t *testing.T) {
	dir := t.TempDir()
	for _, sub := range []string{"tdl_split", ".tdl_split", "season"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, sub, "x.part1.rar"), 1)
	}

	files, err := walk([]string{dir}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].File != filepath.Join(dir, "season", "x.part1.rar") {
		t.Fatalf("walk returned %d files, want only the one outside the legacy folders", len(files))
	}
}

// The MIME type of a volume is taken from the RAR signature alone; it must be
// what sniffing the finished volume gives.
func TestDetectMIMEOfVolume(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "movie.mkv")
	writeFile(t, big, 40000)

	vols, err := newSplitter(16384, false).pack(&File{File: big})
	if err != nil {
		t.Fatal(err)
	}
	got, err := detectMIME(vols[0])
	if err != nil {
		t.Fatal(err)
	}

	if err = vols[0].vol.Open(); err != nil {
		t.Fatal(err)
	}
	defer vols[0].vol.Close()
	want, err := mimetype.DetectReader(vols[0].vol)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != want.String() || !strings.Contains(got.String(), "rar") {
		t.Errorf("detectMIME = %s, the volume itself is %s", got, want)
	}
}
