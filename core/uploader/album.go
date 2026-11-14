package uploader

import (
	"context"
	"reflect"
	"sync"

	"github.com/go-faster/errors"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"
	"golang.org/x/sync/errgroup"
)

// AlbumMax is the number of files Telegram takes in one album.
const AlbumMax = 10

// albumKind tells which files can share an album: photos and videos mix,
// audio and other files only go with their own kind.
type albumKind int

const (
	kindFile   albumKind = iota // plain document
	kindVisual                  // photo or video
	kindAudio
	kindSolo // can't be part of an album at all, e.g. a GIF or a sticker
)

type albumItem struct {
	album *album
	elem  Elem

	// result of the upload, read by the album once all uploads ended
	media tg.InputMediaClass
	kind  albumKind
	err   error
}

// upload uploads the file of the item and keeps the result for its album.
func (it *albumItem) upload(ctx context.Context, u *Uploader) error {
	defer it.album.uploads.Done()

	it.media, it.kind, it.err = u.prepareAlbum(ctx, it.elem)
	return it.err
}

// album is a run of up to AlbumMax elems in a row for the same destination.
type album struct {
	items   []*albumItem
	sealed  chan struct{}  // closed when no more items are added
	uploads sync.WaitGroup // uploads of the items

	prev <-chan struct{} // closed when the album before this one was sent, nil for the first
	sent chan struct{}   // closed when this album was sent, or failed to
}

func (a *album) accepts(elem Elem) bool {
	first := a.items[0].elem
	return len(a.items) < AlbumMax &&
		first.Thread() == elem.Thread() &&
		reflect.DeepEqual(first.To(), elem.To())
}

// albums puts the elems handed to add into albums and sends every album once
// its files are uploaded.
type albums struct {
	u   *Uploader
	ctx context.Context

	cur     *album
	last    <-chan struct{} // sent of the album made last
	senders errgroup.Group  // one per album; they only wait and send, so they are not limited
}

// add puts elem into the current album, or into a new one if it does not fit.
// The caller must run upload of the returned item.
func (as *albums) add(elem Elem) *albumItem {
	if as.cur != nil && !as.cur.accepts(elem) {
		as.seal()
	}
	if as.cur == nil {
		a := &album{sealed: make(chan struct{}), prev: as.last, sent: make(chan struct{})}
		as.cur, as.last = a, a.sent
		as.senders.Go(func() error { return as.send(a) })
	}

	a := as.cur
	item := &albumItem{album: a, elem: elem}
	a.items = append(a.items, item)
	a.uploads.Add(1)

	if len(a.items) == AlbumMax { // don't wait for the next elem to find out
		as.seal()
	}

	return item
}

func (as *albums) seal() {
	if as.cur != nil {
		close(as.cur.sealed)
		as.cur = nil
	}
}

// wait blocks until every album was sent. No elem may be added afterwards.
func (as *albums) wait() error {
	as.seal()
	return as.senders.Wait()
}

// send sends the files of a once all of them are uploaded, and reports each
// of them to OnDone. Files in a row that Telegram accepts in one album go
// out as one message; a file that failed is left out, the rest is still sent.
//
// Albums are sent in the order of their files: an album whose uploads end
// early waits for the one before it, so the messages arrive in order however
// many files are uploaded at a time.
func (as *albums) send(a *album) error {
	defer close(a.sent)

	<-a.sealed
	a.uploads.Wait()
	if a.prev != nil {
		<-a.prev
	}

	var canceled error
	done := func(items []*albumItem, err error) {
		for _, it := range items {
			as.u.opts.Progress.OnDone(it.elem, err)
		}
		if errors.Is(err, context.Canceled) {
			canceled = err
		}
	}

	var run []*albumItem
	flush := func() {
		if len(run) > 0 {
			done(run, as.u.sendAlbum(as.ctx, run))
			run = nil
		}
	}

	for _, it := range a.items {
		if it.err != nil {
			done([]*albumItem{it}, it.err)
			continue
		}
		if len(run) > 0 && (it.kind == kindSolo || it.kind != run[0].kind) {
			flush()
		}
		run = append(run, it)
	}
	flush()

	if canceled != nil { // canceled by user, stop all
		return errors.Wrap(canceled, "send album")
	}
	return nil
}

// prepareAlbum uploads the file of elem and turns it into media of the
// destination chat, without sending it yet.
//
// Telegram only takes media that already exists in an album. Creating it
// right after the upload, and not when the album is sent, also keeps the
// file from expiring while the other files of the album are still uploaded.
func (u *Uploader) prepareAlbum(ctx context.Context, elem Elem) (tg.InputMediaClass, albumKind, error) {
	media, err := u.prepare(ctx, elem)
	if err != nil {
		return nil, 0, err
	}

	uploaded, err := message.NewSender(u.opts.Client).
		To(elem.To()).
		UploadMedia(ctx, media)
	if err != nil {
		return nil, 0, errors.Wrap(err, "upload media")
	}

	return albumMedia(uploaded)
}

// albumMedia returns how to send media that was created by prepareAlbum,
// and what it can share an album with. The kind is taken from what Telegram
// made of the file, not from what was asked for.
func albumMedia(media tg.MessageMediaClass) (tg.InputMediaClass, albumKind, error) {
	switch m := media.(type) {
	case *tg.MessageMediaPhoto:
		if m.Photo == nil {
			break
		}
		photo, ok := m.Photo.AsNotEmpty()
		if !ok {
			break
		}

		return &tg.InputMediaPhoto{ID: photo.AsInput()}, kindVisual, nil
	case *tg.MessageMediaDocument:
		if m.Document == nil {
			break
		}
		doc, ok := m.Document.AsNotEmpty()
		if !ok {
			break
		}

		return &tg.InputMediaDocument{ID: doc.AsInput()}, documentKind(m, doc), nil
	}

	return nil, 0, errors.Errorf("unexpected media %T", media)
}

func documentKind(m *tg.MessageMediaDocument, doc *tg.Document) albumKind {
	if m.Round || m.Voice {
		return kindSolo
	}

	kind := kindFile
	for _, attr := range doc.Attributes {
		switch a := attr.(type) {
		case *tg.DocumentAttributeAnimated, *tg.DocumentAttributeSticker, *tg.DocumentAttributeCustomEmoji:
			return kindSolo
		case *tg.DocumentAttributeVideo:
			if a.RoundMessage {
				return kindSolo
			}
			kind = kindVisual
		case *tg.DocumentAttributeAudio:
			if a.Voice {
				return kindSolo
			}
			kind = kindAudio
		}
	}

	return kind
}

// sendAlbum sends items, which are uploaded and of one kind, as one message.
func (u *Uploader) sendAlbum(ctx context.Context, items []*albumItem) error {
	media := make([]message.MultiMediaOption, 0, len(items))
	for _, it := range items {
		// the media exists already, there is nothing left to do for an album
		media = append(media, message.ForceMulti(message.Media(it.media, caption(it.elem))))
	}

	first := items[0].elem
	_, err := message.NewSender(u.opts.Client).
		To(first.To()).
		Reply(first.Thread()).
		Album(ctx, media[0], media[1:]...) // a single file is sent as a plain message
	if err != nil {
		return errors.Wrap(err, "send message")
	}

	return nil
}
