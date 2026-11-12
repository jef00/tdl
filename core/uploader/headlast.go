package uploader

import (
	"context"
	"io"
	"sync"

	"github.com/go-faster/errors"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"golang.org/x/sync/errgroup"
)

// uploadHeadLast uploads f back to front: the parts behind the first one in
// order, the first part last. Telegram accepts the parts of a file in any
// order, so the result is the same file as a front to back upload.
//
// It always uses the big file API, so f must be larger than
// constant.UploadMaxSmallSize.
func (u *Uploader) uploadHeadLast(ctx context.Context, elem Elem, f HeadLast) (tg.InputFileClass, error) {
	size := f.Size()
	total := int((size + MaxPartSize - 1) / MaxPartSize)

	id, err := crypto.RandInt64(crypto.DefaultRand())
	if err != nil {
		return nil, errors.Wrap(err, "id generation")
	}

	type part struct {
		id  int
		buf *bin.Buffer
	}

	threads := max(u.opts.Threads, 1)
	pool := bin.NewPool(MaxPartSize)
	parts := make(chan part, threads)
	g, gctx := errgroup.WithContext(ctx)

	// read loop
	g.Go(func() error {
		defer close(parts)

		read := func(id int) error {
			buf := pool.GetSize(int(min(size-int64(id)*MaxPartSize, MaxPartSize)))
			if _, err := io.ReadFull(f, buf.Buf); err != nil {
				pool.Put(buf)
				return errors.Wrapf(err, "read part %d", id)
			}

			select {
			case parts <- part{id: id, buf: buf}:
				return nil
			case <-gctx.Done():
				pool.Put(buf)
				return gctx.Err()
			}
		}

		if _, err := f.Seek(MaxPartSize, io.SeekStart); err != nil {
			return errors.Wrap(err, "seek file")
		}
		for id := 1; id < total; id++ {
			if err := read(id); err != nil {
				return err
			}
		}

		if err := f.WaitHead(gctx); err != nil {
			return errors.Wrap(err, "wait for first part")
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return errors.Wrap(err, "seek file")
		}
		return read(0)
	})

	var (
		mu       sync.Mutex // keeps progress reports in order
		uploaded int64
	)
	for range threads {
		g.Go(func() error {
			for p := range parts {
				n := int64(p.buf.Len())
				err := u.saveBigFilePart(gctx, id, p.id, total, p.buf.Buf)
				pool.Put(p.buf)
				if err != nil {
					return err
				}

				mu.Lock()
				uploaded += n
				u.opts.Progress.OnUpload(elem, ProgressState{Uploaded: uploaded, Total: size})
				mu.Unlock()
			}
			return nil
		})
	}

	if err = g.Wait(); err != nil {
		return nil, err
	}

	return &tg.InputFileBig{ID: id, Parts: total, Name: f.Name()}, nil
}

func (u *Uploader) saveBigFilePart(ctx context.Context, file int64, part, total int, data []byte) error {
	for {
		ok, err := u.opts.Client.UploadSaveBigFilePart(ctx, &tg.UploadSaveBigFilePartRequest{
			FileID:         file,
			FilePart:       part,
			FileTotalParts: total,
			Bytes:          data,
		})
		if flood, err := tgerr.FloodWait(ctx, err); err != nil {
			if flood {
				continue
			}
			return errors.Wrapf(err, "send upload part %d RPC", part)
		}

		// Telegram did not save the part: send it again
		if ok {
			return nil
		}
	}
}
