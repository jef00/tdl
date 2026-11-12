package up

import (
	"context"
	"io"
	"path/filepath"

	"github.com/gotd/td/telegram/message/entity"
	"github.com/gotd/td/telegram/peers"
	"github.com/gotd/td/tg"

	"github.com/iyear/tdl/core/uploader"
	"github.com/iyear/tdl/pkg/rar5"
)

type iterElem struct {
	file    *uploaderFile
	thumb   *uploaderFile
	to      peers.Peer
	caption *entity.Builder
	thread  int

	asPhoto bool
	remove  bool
	vol     *rar5.Volume // set when file is a generated RAR volume
	group   *splitGroup  // nil unless this is a volume of a split file
}

func (e *iterElem) File() uploader.File {
	if e.vol != nil {
		return volumeFile{uploaderFile: e.file, vol: e.vol}
	}
	return e.file
}

func (e *iterElem) Thumb() (uploader.File, bool) {
	if e.thumb == nil {
		return nil, false
	}
	return e.thumb, true
}

func (e *iterElem) Caption() (string, []tg.MessageEntityClass) {
	return e.caption.Complete()
}

func (e *iterElem) To() tg.InputPeerClass {
	return e.to.InputPeer()
}

func (e *iterElem) Thread() int {
	return e.thread
}

func (e *iterElem) AsPhoto() bool {
	return e.asPhoto
}

// uploaderFile is a file on disk or a RAR volume generated on the fly.
type uploaderFile struct {
	io.ReadSeeker
	io.Closer
	path string // real path, or the virtual path of a generated volume
	size int64
}

func (u *uploaderFile) Name() string {
	return filepath.Base(u.path)
}

func (u *uploaderFile) Size() int64 {
	return u.size
}

// volumeFile is a generated RAR volume. Its header holds a checksum of the
// data behind it, so it implements uploader.HeadLast: uploaded back to
// front, the source is read only once.
type volumeFile struct {
	*uploaderFile
	vol *rar5.Volume
}

func (v volumeFile) WaitHead(ctx context.Context) error {
	return v.vol.WaitHead(ctx)
}
