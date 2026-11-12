package uploader

import (
	"context"
	"io"

	"github.com/gotd/td/tg"
)

type Iter interface {
	Next(ctx context.Context) bool
	Value() Elem
	Err() error
}

type File interface {
	io.ReadSeeker
	Name() string
	Size() int64
}

// HeadLast is implemented by a File whose first bytes depend on the rest of
// its content, like an archive header with a checksum of the data behind it.
//
// Such a file is uploaded back to front: everything behind its first part is
// read and sent first, then WaitHead is called, and the first part is read
// and sent last. The file is read only once that way, instead of once to
// work out its start and once more to upload it.
type HeadLast interface {
	File
	// WaitHead blocks until the first part of the file can be read.
	WaitHead(ctx context.Context) error
}

type Elem interface {
	File() File
	Thumb() (File, bool)
	Caption() (string, []tg.MessageEntityClass)
	To() tg.InputPeerClass
	Thread() int
	AsPhoto() bool
}
