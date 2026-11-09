package uploader

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

type fakeElem struct{ id int }

func (fakeElem) File() File                                 { return nil }
func (fakeElem) Thumb() (File, bool)                        { return nil, false }
func (fakeElem) Caption() (string, []tg.MessageEntityClass) { return "", nil }
func (fakeElem) To() tg.InputPeerClass                      { return &tg.InputPeerSelf{} }
func (fakeElem) Thread() int                                { return 0 }
func (fakeElem) AsPhoto() bool                              { return false }

type fakeIter struct {
	elems []Elem
	cur   int
}

func (i *fakeIter) Next(context.Context) bool {
	if i.cur >= len(i.elems) {
		return false
	}
	i.cur++
	return true
}
func (i *fakeIter) Value() Elem { return i.elems[i.cur-1] }
func (i *fakeIter) Err() error  { return nil }

type fakeProgress struct {
	mu   sync.Mutex
	done map[int]error
}

func (*fakeProgress) OnAdd(Elem)                   {}
func (*fakeProgress) OnUpload(Elem, ProgressState) {}
func (p *fakeProgress) OnDone(e Elem, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.done[e.(fakeElem).id] = err
}

// A failed upload must reach OnDone as an error, otherwise callers such as
// `tdl up --rm` treat it as uploaded and delete the local file.
func TestUploadReportsFailures(t *testing.T) {
	// an expired deadline makes every upload fail with a non-cancel error
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	progress := &fakeProgress{done: map[int]error{}}
	u := New(Options{
		Iter:     &fakeIter{elems: []Elem{fakeElem{1}, fakeElem{2}}},
		Progress: progress,
	})

	if err := u.Upload(ctx, 1); err != nil {
		t.Fatalf("non-cancel failures must not abort the batch: %v", err)
	}

	if len(progress.done) != 2 {
		t.Fatalf("OnDone called for %d elems, want 2", len(progress.done))
	}
	for id, err := range progress.done {
		if err == nil {
			t.Errorf("elem %d: OnDone got nil error for a failed upload", id)
		}
	}
}
