package uploader

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

// fakeTelegram is a tg.Invoker that keeps what is uploaded and sent to it.
type fakeTelegram struct {
	mu sync.Mutex

	parts map[int64]map[int][]byte // file id -> part -> data
	order map[int64][]int          // file id -> parts in order of arrival
	sent  int                      // messages sent

	docs map[int64]string // documents made by messages.uploadMedia, by id -> file name
	msgs [][]string       // file names of the messages sent from such documents

	// fail, if set, can make a request fail
	fail func(req bin.Encoder) error
}

func newFakeTelegram() *fakeTelegram {
	return &fakeTelegram{
		parts: map[int64]map[int][]byte{},
		order: map[int64][]int{},
		docs:  map[int64]string{},
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
		if doc, ok := req.Media.(*tg.InputMediaDocument); ok {
			f.msgs = append(f.msgs, []string{f.docName(doc)})
		}
		f.sent++
		output.(*tg.UpdatesBox).Updates = &tg.Updates{}
	case *tg.MessagesUploadMediaRequest:
		in, ok := req.Media.(*tg.InputMediaUploadedDocument)
		if !ok {
			return fmt.Errorf("unexpected media %T", req.Media)
		}

		doc := &tg.Document{ID: int64(len(f.docs) + 1), Attributes: in.Attributes}
		for _, attr := range in.Attributes {
			if name, ok := attr.(*tg.DocumentAttributeFilename); ok {
				f.docs[doc.ID] = name.FileName
			}
		}
		// like Telegram, the fake has its own idea of what a file is
		if strings.HasSuffix(f.docs[doc.ID], ".mp3") {
			doc.Attributes = append(slices.Clone(doc.Attributes), &tg.DocumentAttributeAudio{})
		}
		media := &tg.MessageMediaDocument{}
		media.SetDocument(doc)
		output.(*tg.MessageMediaBox).MessageMedia = media
	case *tg.MessagesSendMultiMediaRequest:
		var names []string
		for _, m := range req.MultiMedia {
			names = append(names, f.docName(m.Media.(*tg.InputMediaDocument)))
		}
		f.msgs = append(f.msgs, names)
		f.sent++
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

func (f *fakeTelegram) docName(doc *tg.InputMediaDocument) string {
	return f.docs[doc.ID.(*tg.InputDocument).ID]
}

// file puts the parts of an uploaded file together.
func (f *fakeTelegram) file(id int64, parts int) ([]byte, error) {
	if len(f.parts[id]) != parts {
		return nil, fmt.Errorf("file has %d parts, %d were uploaded", parts, len(f.parts[id]))
	}

	var content []byte
	for part := range parts {
		content = append(content, f.parts[id][part]...)
	}
	return content, nil
}

// testElem is an Elem that sends file to oneself.
type testElem struct{ file File }

func (e *testElem) File() File                                 { return e.file }
func (*testElem) Thumb() (File, bool)                          { return nil, false }
func (e *testElem) Caption() (string, []tg.MessageEntityClass) { return e.file.Name(), nil }
func (*testElem) To() tg.InputPeerClass                        { return &tg.InputPeerSelf{} }
func (*testElem) Thread() int                                  { return 0 }
func (*testElem) AsPhoto() bool                                { return false }

// recorder is a Progress that keeps how the uploads ended.
type recorder struct {
	mu   sync.Mutex
	errs []error
}

func (*recorder) OnAdd(Elem)                   {}
func (*recorder) OnUpload(Elem, ProgressState) {}
func (r *recorder) OnDone(_ Elem, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, err)
}

// upload uploads file and returns the error it was reported with.
func upload(t *testing.T, tgram *fakeTelegram, file File) error {
	t.Helper()

	rec := &recorder{}
	u := New(Options{
		Client:   tg.NewClient(tgram),
		Threads:  2,
		Iter:     &fakeIter{elems: []Elem{&testElem{file: file}}},
		Progress: rec,
	})
	if err := u.Upload(context.Background(), 1); err != nil {
		t.Fatal(err)
	}

	if len(rec.errs) != 1 {
		t.Fatalf("OnDone called %d times, want once", len(rec.errs))
	}
	return rec.errs[0]
}
