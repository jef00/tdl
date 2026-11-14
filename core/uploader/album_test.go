package uploader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// memFile is a File in memory.
type memFile struct {
	*bytes.Reader
	name string
}

func (f *memFile) Name() string { return f.name }

func plainFile(name string) File {
	return &memFile{Reader: bytes.NewReader([]byte("just some text\n")), name: name}
}

// albumElem is an Elem for a thread of one's own chat.
type albumElem struct {
	testElem
	thread int
}

func (e *albumElem) Thread() int { return e.thread }

// albumRecorder is a Progress that keeps how each file ended, and how many
// messages were sent by then.
type albumRecorder struct {
	tgram *fakeTelegram

	mu   sync.Mutex
	errs map[string]error
	sent map[string]int
}

func (*albumRecorder) OnAdd(Elem)                   {}
func (*albumRecorder) OnUpload(Elem, ProgressState) {}
func (r *albumRecorder) OnDone(e Elem, err error) {
	r.tgram.mu.Lock()
	sent := r.tgram.sent
	r.tgram.mu.Unlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs[e.File().Name()] = err
	r.sent[e.File().Name()] = sent
}

func uploadAlbums(t *testing.T, tgram *fakeTelegram, limit int, elems ...Elem) *albumRecorder {
	t.Helper()

	rec := &albumRecorder{tgram: tgram, errs: map[string]error{}, sent: map[string]int{}}
	u := New(Options{
		Client:   tg.NewClient(tgram),
		Threads:  2,
		Iter:     &fakeIter{elems: elems},
		Progress: rec,
		Album:    true,
	})
	if err := u.Upload(context.Background(), limit); err != nil {
		t.Fatal(err)
	}

	if len(rec.errs) != len(elems) {
		t.Fatalf("OnDone called for %d files, want %d", len(rec.errs), len(elems))
	}
	return rec
}

func plainElems(n int) ([]Elem, []string) {
	var (
		elems []Elem
		names []string
	)
	for i := range n {
		names = append(names, fmt.Sprintf("%02d.txt", i))
		elems = append(elems, &testElem{file: plainFile(names[i])})
	}
	return elems, names
}

func TestAlbumGroupsFiles(t *testing.T) {
	for _, limit := range []int{1, 3, 20} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			tgram := newFakeTelegram()
			elems, names := plainElems(2*AlbumMax + 3)
			rec := uploadAlbums(t, tgram, limit, elems...)

			want := [][]string{names[:AlbumMax], names[AlbumMax : 2*AlbumMax], names[2*AlbumMax:]}
			if got := tgram.msgs; !slices.EqualFunc(got, want, slices.Equal[[]string]) {
				t.Errorf("sent %v, want %v", got, want)
			}

			for _, name := range names {
				if err := rec.errs[name]; err != nil {
					t.Errorf("%s: %v", name, err)
				}
				// a file is done, and may be removed, only once it was sent
				if rec.sent[name] == 0 {
					t.Errorf("%s: done before any message was sent", name)
				}
			}
		})
	}
}

// Albums arrive in the order of their files, also when a later one is
// uploaded first.
func TestAlbumsSentInOrder(t *testing.T) {
	elems, names := plainElems(2*AlbumMax + 3)

	var (
		tgram  = newFakeTelegram()
		mu     sync.Mutex
		others int
		rest   = make(chan struct{}) // closed when every file but the first is uploaded
	)
	tgram.fail = func(req bin.Encoder) error {
		up, ok := req.(*tg.MessagesUploadMediaRequest)
		if !ok {
			return nil
		}

		for _, attr := range up.Media.(*tg.InputMediaUploadedDocument).Attributes {
			if name, ok := attr.(*tg.DocumentAttributeFilename); ok && name.FileName == names[0] {
				// the first file is the last to finish, long after the
				// later albums are complete
				<-rest
				time.Sleep(100 * time.Millisecond)
				return nil
			}
		}

		mu.Lock()
		defer mu.Unlock()
		if others++; others == len(names)-1 {
			close(rest)
		}
		return nil
	}

	uploadAlbums(t, tgram, len(elems), elems...)

	want := [][]string{names[:AlbumMax], names[AlbumMax : 2*AlbumMax], names[2*AlbumMax:]}
	if !slices.EqualFunc(tgram.msgs, want, slices.Equal[[]string]) {
		t.Errorf("sent %v, want %v", tgram.msgs, want)
	}
}

// A single file is still an ordinary message.
func TestAlbumSingleFile(t *testing.T) {
	tgram := newFakeTelegram()
	uploadAlbums(t, tgram, 1, &testElem{file: plainFile("one.txt")})

	if want := [][]string{{"one.txt"}}; !slices.EqualFunc(tgram.msgs, want, slices.Equal[[]string]) {
		t.Errorf("sent %v, want %v", tgram.msgs, want)
	}
}

// Files only share an album with their own kind and their own destination.
// The fake makes audio of every .mp3, whatever it was uploaded as.
func TestAlbumSplitsByKindAndDestination(t *testing.T) {
	tgram := newFakeTelegram()
	uploadAlbums(t, tgram, 2,
		&testElem{file: plainFile("a.txt")},
		&testElem{file: plainFile("b.txt")},
		&testElem{file: plainFile("c.mp3")},
		&testElem{file: plainFile("d.mp3")},
		&testElem{file: plainFile("e.txt")},
		&albumElem{testElem: testElem{file: plainFile("f.txt")}, thread: 7},
		&albumElem{testElem: testElem{file: plainFile("g.txt")}, thread: 7},
	)

	want := [][]string{{"a.txt", "b.txt"}, {"c.mp3", "d.mp3"}, {"e.txt"}, {"f.txt", "g.txt"}}
	if got := tgram.msgs; !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("sent %v, want %v", got, want)
	}
}

func TestAlbumKinds(t *testing.T) {
	doc := func(m *tg.MessageMediaDocument, attrs ...tg.DocumentAttributeClass) tg.MessageMediaClass {
		m.SetDocument(&tg.Document{ID: 1, Attributes: attrs})
		return m
	}
	photo := &tg.MessageMediaPhoto{}
	photo.SetPhoto(&tg.Photo{ID: 1})

	for name, tt := range map[string]struct {
		media tg.MessageMediaClass
		want  albumKind
	}{
		"photo":       {photo, kindVisual},
		"file":        {doc(&tg.MessageMediaDocument{}, &tg.DocumentAttributeFilename{FileName: "a.mkv"}), kindFile},
		"video":       {doc(&tg.MessageMediaDocument{}, &tg.DocumentAttributeVideo{}), kindVisual},
		"gif":         {doc(&tg.MessageMediaDocument{}, &tg.DocumentAttributeVideo{}, &tg.DocumentAttributeAnimated{}), kindSolo},
		"round video": {doc(&tg.MessageMediaDocument{}, &tg.DocumentAttributeVideo{RoundMessage: true}), kindSolo},
		"audio":       {doc(&tg.MessageMediaDocument{}, &tg.DocumentAttributeAudio{}), kindAudio},
		"voice":       {doc(&tg.MessageMediaDocument{}, &tg.DocumentAttributeAudio{Voice: true}), kindSolo},
		"sticker":     {doc(&tg.MessageMediaDocument{}, &tg.DocumentAttributeSticker{}), kindSolo},
	} {
		if _, got, err := albumMedia(tt.media); err != nil || got != tt.want {
			t.Errorf("%s: kind %d, err %v; want kind %d", name, got, err, tt.want)
		}
	}

	if _, _, err := albumMedia(&tg.MessageMediaDocument{}); err == nil {
		t.Error("media without a document must be an error")
	}
}

// A file that fails is reported as failed and left out, its album is sent
// without it.
func TestAlbumFileFails(t *testing.T) {
	tgram := newFakeTelegram()
	failed := 0
	tgram.fail = func(req bin.Encoder) error {
		if _, ok := req.(*tg.MessagesUploadMediaRequest); !ok {
			return nil
		}
		if failed++; failed == 2 { // limit 1: this is the second file
			return errors.New("boom")
		}
		return nil
	}

	elems, names := plainElems(3)
	rec := uploadAlbums(t, tgram, 1, elems...)

	if want := [][]string{{names[0], names[2]}}; !slices.EqualFunc(tgram.msgs, want, slices.Equal[[]string]) {
		t.Errorf("sent %v, want %v", tgram.msgs, want)
	}
	for i, name := range names {
		if failed := rec.errs[name] != nil; failed != (i == 1) {
			t.Errorf("%s: err %v", name, rec.errs[name])
		}
	}
}

// When the album can't be sent, none of its files is done.
func TestAlbumSendFails(t *testing.T) {
	tgram := newFakeTelegram()
	tgram.fail = func(req bin.Encoder) error {
		if _, ok := req.(*tg.MessagesSendMultiMediaRequest); ok {
			return errors.New("boom")
		}
		return nil
	}

	elems, names := plainElems(3)
	rec := uploadAlbums(t, tgram, 2, elems...)

	for _, name := range names {
		if rec.errs[name] == nil {
			t.Errorf("%s: done although its album was not sent", name)
		}
	}
}

// Canceling stops the upload and still reports every file that was started.
func TestAlbumCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tgram := newFakeTelegram()
	tgram.fail = func(req bin.Encoder) error {
		if _, ok := req.(*tg.MessagesSendMultiMediaRequest); ok {
			cancel()
			return ctx.Err()
		}
		return nil
	}

	elems, _ := plainElems(AlbumMax + 2)
	rec := &albumRecorder{tgram: tgram, errs: map[string]error{}, sent: map[string]int{}}
	u := New(Options{
		Client:   tg.NewClient(tgram),
		Threads:  2,
		Iter:     &fakeIter{elems: elems},
		Progress: rec,
		Album:    true,
	})

	if err := u.Upload(ctx, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("Upload returned %v, want it canceled", err)
	}
	for name, err := range rec.errs {
		if err == nil {
			t.Errorf("%s: done although nothing was sent", name)
		}
	}
	if len(tgram.msgs) != 0 {
		t.Errorf("sent %v, want nothing", tgram.msgs)
	}
}
