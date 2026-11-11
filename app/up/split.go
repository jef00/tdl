package up

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/fatih/color"

	"github.com/iyear/tdl/pkg/rar5"
)

const (
	// Telegram accepts at most 4000 (8000 for Premium) parts of 512 KiB per file.
	splitAlign        = 512 * 1024
	SplitLimitDefault = int64(4000) * splitAlign // 2000 MiB
	SplitLimitPremium = int64(8000) * splitAlign // 4000 MiB
)

// splitGroup tracks the RAR volumes of one split source file, so the source
// is only removed once every part has been uploaded successfully.
type splitGroup struct {
	source    string
	keep      bool // --keep-volumes: save each uploaded volume next to the source
	remaining atomic.Int32
	failed    atomic.Bool
}

// done marks one part as finished. It returns true exactly once: when the
// last part finished and none of the parts failed.
func (g *splitGroup) done(err error) bool {
	if err != nil {
		g.failed.Store(true)
	}
	return g.remaining.Add(-1) == 0 && !g.failed.Load()
}

// alignSplitSize rounds size down to a multiple of the upload part size and
// clamps it to [splitAlign, limit].
func alignSplitSize(size, limit int64) int64 {
	if size <= 0 || size > limit {
		size = limit
	}
	size -= size % splitAlign
	if size < splitAlign {
		size = splitAlign
	}
	return size
}

// Folders in which earlier builds staged RAR volumes on disk. Nothing is
// staged anymore, but walk still skips them so leftovers of an old run are
// not uploaded as ordinary files.
var legacyStageDirs = map[string]struct{}{
	"tdl_split":  {},
	".tdl_split": {},
}

// splitter turns files larger than size into store-only RAR volumes of at
// most size bytes. The volumes are generated while they are uploaded (see
// pkg/rar5): nothing is written to disk and no rar binary is needed.
type splitter struct {
	size int64
	keep bool         // --keep-volumes: save each uploaded volume next to its source
	log  func(string) // progress-safe logger, nil prints to stdout

	used map[string]struct{} // archive names already taken, per source dir
}

func newSplitter(size int64, keep bool) *splitter {
	return &splitter{size: size, keep: keep, used: make(map[string]struct{})}
}

// pack returns the RAR volumes of f, or nil if f fits in a single upload and
// should be sent as is. The file is read once for the checksums stored in
// the volumes, which is why files are packed only when their turn comes.
func (s *splitter) pack(ctx context.Context, f *File) ([]*File, error) {
	if s.size <= 0 {
		return nil, nil
	}

	stat, err := os.Stat(f.File)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", f.File, err)
	}
	if stat.Size() <= s.size {
		return nil, nil
	}

	// "movie.mkv" -> "movie"; fall back to the full name on a clash
	// (movie.mkv + movie.mp4, or Movie.mkv + movie.mkv), so two sources
	// never share volume names
	dir := filepath.Dir(f.File)
	base := strings.TrimSuffix(filepath.Base(f.File), filepath.Ext(f.File))
	if _, clash := s.used[s.key(dir, base)]; clash || base == "" {
		base = filepath.Base(f.File)
	}
	s.used[s.key(dir, base)] = struct{}{}

	s.logf("Splitting %s: reading it once for the RAR checksums", filepath.Base(f.File))
	vols, err := rar5.Split(ctx, f.File, base, s.size)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("split %s: %w", f.File, err)
	}

	g := &splitGroup{source: f.File, keep: s.keep}
	g.remaining.Store(int32(len(vols)))
	out := make([]*File, 0, len(vols))
	for i, v := range vols {
		out = append(out, &File{
			File:  filepath.Join(dir, v.Name()), // where --keep-volumes would save it
			Part:  i + 1,
			Parts: len(vols),
			vol:   v,
			group: g,
		})
	}

	return out, nil
}

func (s *splitter) key(dir, base string) string {
	return filepath.Join(dir, strings.ToLower(base))
}

func (s *splitter) logf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if s.log != nil {
		s.log(msg)
		return
	}
	color.Blue("%s", msg)
}
