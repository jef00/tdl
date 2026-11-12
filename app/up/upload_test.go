package up

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram/message/entity"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"
	pw "github.com/jedib0t/go-pretty/v6/progress"

	"github.com/iyear/tdl/core/uploader"
	"github.com/iyear/tdl/pkg/rar5"
)

// fakeTelegram is a tg.Invoker that keeps the files sent to it.
type fakeTelegram struct {
	mu sync.Mutex

	parts map[int64]map[int][]byte // file id -> part -> data
	order map[int64][]int          // file id -> parts in order of arrival

	sent  []string          // file names of the messages sent, in order
	docs  map[string][]byte // file name -> content
	first map[string]int    // file name -> position at which part 0 arrived

	fail func(req bin.Encoder) error
}

func newFakeTelegram() *fakeTelegram {
	return &fakeTelegram{
		parts: map[int64]map[int][]byte{},
		order: map[int64][]int{},
		docs:  map[string][]byte{},
		first: map[string]int{},
	}
}

func (f *fakeTelegram) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.fail != nil {
		if err := f.fail(input); err != nil {
			return err
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	switch req := input.(type) {
	case *tg.UploadSaveBigFilePartRequest:
		f.savePart(req.FileID, req.FilePart, req.Bytes)
		output.(*tg.BoolBox).Bool = &tg.BoolTrue{}
	case *tg.UploadSaveFilePartRequest:
		f.savePart(req.FileID, req.FilePart, req.Bytes)
		output.(*tg.BoolBox).Bool = &tg.BoolTrue{}
	case *tg.MessagesSendMediaRequest:
		media := req.Media.(*tg.InputMediaUploadedDocument)
		var (
			file  int64
			parts int
		)
		switch in := media.File.(type) {
		case *tg.InputFileBig:
			file, parts = in.ID, in.Parts
		case *tg.InputFile:
			file, parts = in.ID, in.Parts
		}
		if len(f.parts[file]) != parts {
			return fmt.Errorf("file has %d parts, %d were uploaded", parts, len(f.parts[file]))
		}

		var name string
		for _, attr := range media.Attributes {
			if n, ok := attr.(*tg.DocumentAttributeFilename); ok {
				name = n.FileName
			}
		}
		for part := range parts {
			f.docs[name] = append(f.docs[name], f.parts[file][part]...)
		}
		f.first[name] = slices.Index(f.order[file], 0)
		f.sent = append(f.sent, name)
		output.(*tg.UpdatesBox).Updates = &tg.Updates{}
	default:
		return fmt.Errorf("unexpected request %T", input)
	}

	return nil
}

func (f *fakeTelegram) savePart(file int64, part int, data []byte) {
	if f.parts[file] == nil {
		f.parts[file] = map[int][]byte{}
	}
	f.parts[file][part] = bytes.Clone(data)
	f.order[file] = append(f.order[file], part)
}

type fakePeer struct{ peers.Peer }

func (fakePeer) ID() int64                    { return 1 }
func (fakePeer) VisibleName() string          { return "test" }
func (fakePeer) InputPeer() tg.InputPeerClass { return &tg.InputPeerSelf{} }

// splitIter hands out files like iter does, without needing a session to
// resolve where they go.
type splitIter struct {
	it     *iter
	remove bool
	elem   *iterElem
}

func (s *splitIter) Next(ctx context.Context) bool {
	i := s.it
	if ctx.Err() != nil || i.err != nil || (len(i.pending) == 0 && i.cur >= len(i.files)) {
		return false
	}

	cur, err := i.advance()
	if err != nil {
		i.err = err
		return false
	}
	file, err := i.resolveFile(cur)
	if err != nil {
		i.err = err
		return false
	}

	s.elem = &iterElem{
		file:    file,
		to:      fakePeer{},
		caption: &entity.Builder{},
		remove:  s.remove,
		vol:     cur.vol,
		group:   cur.group,
	}
	return true
}

func (s *splitIter) Value() uploader.Elem { return s.elem }
func (s *splitIter) Err() error           { return s.it.err }

const (
	threads    = 4
	testVolume = 12 << 20             // big enough for the back to front upload
	testSource = 2*testVolume + 5<<20 // two full volumes and a small one
)

// writeMovie writes a source file and returns it with the volumes a RAR tool
// expects for it, read front to back.
func writeMovie(t *testing.T, dir string) (string, [][]byte) {
	t.Helper()
	content := make([]byte, testSource)
	rand.New(rand.NewSource(1)).Read(content)
	path := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	vols, err := rar5.Split(path, "movie", testVolume)
	if err != nil {
		t.Fatal(err)
	}
	want := make([][]byte, len(vols))
	for i, v := range vols {
		if err = v.Open(); err != nil {
			t.Fatal(err)
		}
		if want[i], err = io.ReadAll(v); err != nil {
			t.Fatal(err)
		}
		_ = v.Close()
	}
	return path, want
}

func uploadSplit(t *testing.T, tgram *fakeTelegram, path string, keep bool, limit int) error {
	t.Helper()
	split := newSplitter(testVolume, keep)
	split.log = func(string) {}

	u := uploader.New(uploader.Options{
		Client:   tg.NewClient(tgram),
		Threads:  threads,
		Iter:     &splitIter{it: &iter{files: []*File{{File: path}}, split: split}, remove: true},
		Progress: newProgress(pw.NewWriter()),
	})

	done := make(chan error, 1)
	go func() { done <- u.Upload(context.Background(), limit) }()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Minute):
		t.Fatal("upload hangs")
		return nil
	}
}

// The whole way of a split file: its volumes arrive as valid RAR volumes,
// each uploaded back to front, and only then the source goes.
func TestUploadSplitFile(t *testing.T) {
	for _, limit := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			dir := t.TempDir()
			path, want := writeMovie(t, dir)
			tgram := newFakeTelegram()

			if err := uploadSplit(t, tgram, path, false, limit); err != nil {
				t.Fatal(err)
			}

			if len(tgram.sent) != len(want) {
				t.Fatalf("sent %v, want %d volumes", tgram.sent, len(want))
			}
			for i := range want {
				name := fmt.Sprintf("movie.part%d.rar", i+1)
				if !bytes.Equal(tgram.docs[name], want[i]) {
					t.Errorf("%s arrived different from the volume read front to back", name)
				}

				// back to front: the first part is sent last, so it arrives
				// among the last ones, give or take the parts still on their way
				parts := (len(want[i]) + uploader.MaxPartSize - 1) / uploader.MaxPartSize
				if big := len(want[i]) > 10<<20; big && tgram.first[name] < parts-threads {
					t.Errorf("%s: its first part arrived at position %d of %d", name, tgram.first[name], parts)
				}
			}

			if names := dirNames(t, dir); len(names) != 0 {
				t.Errorf("left %v, want the source removed and nothing else", names)
			}
		})
	}
}

// --keep-volumes saves what was uploaded.
func TestUploadSplitFileKeepVolumes(t *testing.T) {
	dir := t.TempDir()
	path, want := writeMovie(t, dir)

	if err := uploadSplit(t, newFakeTelegram(), path, true, 2); err != nil {
		t.Fatal(err)
	}

	for i := range want {
		name := fmt.Sprintf("movie.part%d.rar", i+1)
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, want[i]) {
			t.Errorf("%s: %d bytes, err %v; want the uploaded volume", name, len(got), err)
		}
	}
	if names := dirNames(t, dir); len(names) != len(want) {
		t.Errorf("dir holds %v, want only the volumes", names)
	}
}

// When a volume fails the source stays, and the last volume, whose header
// needs the checksum of every volume, fails too instead of waiting forever.
func TestUploadSplitFileFailure(t *testing.T) {
	for _, limit := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			dir := t.TempDir()
			path, _ := writeMovie(t, dir)

			tgram := newFakeTelegram()
			var (
				mu   sync.Mutex
				seen []int64 // big uploads, in order of their first part
			)
			tgram.fail = func(req bin.Encoder) error {
				part, ok := req.(*tg.UploadSaveBigFilePartRequest)
				if !ok {
					return nil
				}
				mu.Lock()
				defer mu.Unlock()
				if !slices.Contains(seen, part.FileID) {
					seen = append(seen, part.FileID)
				}
				// one of the two full volumes fails halfway
				if len(seen) > 1 && part.FileID == seen[1] && part.FilePart == 12 {
					return errors.New("boom")
				}
				return nil
			}

			if err := uploadSplit(t, tgram, path, false, limit); err != nil {
				t.Fatalf("a failed file must not abort the batch: %v", err)
			}

			if len(tgram.sent) != 1 || slices.Contains(tgram.sent, "movie.part3.rar") {
				t.Errorf("sent %v, want only the volume that did not fail or depend on it", tgram.sent)
			}
			if names := dirNames(t, dir); !slices.Equal(names, []string{"movie.mkv"}) {
				t.Errorf("dir holds %v, want the source kept", names)
			}
		})
	}
}
