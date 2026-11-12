// Package rar5 generates store-only RAR 5.0 multi-volume archives on the fly.
//
// A volume is never written to disk: it is a small generated header, a
// section of the source file and a small trailer, exposed as an
// io.ReadSeeker. Stored (uncompressed) RAR data is the file content itself,
// so no RAR tool is needed to create the volumes, and any RAR 5 capable tool
// (WinRAR, unrar, 7-Zip, ...) can extract them.
//
// The header in front of the data holds a checksum of that data. It is
// computed from the bytes as they are read through the volume, so a reader
// that takes the data first and the header last (see Volume.WaitHead) reads
// the source exactly once. A reader that starts at the header still gets a
// correct volume; the data it has not seen yet is then read ahead of time.
//
// Layout follows RarLab's "RAR 5.0 archive format" technote.
package rar5

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var signature = []byte{'R', 'a', 'r', '!', 0x1A, 0x07, 0x01, 0x00}

// Signature returns the bytes every RAR 5 volume starts with.
func Signature() []byte { return append([]byte(nil), signature...) }

const (
	headMain = 1
	headFile = 2
	headEnd  = 5

	// common header flags
	flagData        = 0x0002 // data area follows the header
	flagSkipUnknown = 0x0004
	flagSplitBefore = 0x0008 // data continues from the previous volume
	flagSplitAfter  = 0x0010 // data continues in the next volume

	mainVolume    = 0x0001 // archive is a volume
	mainVolNumber = 0x0002 // volume number field present (all but the first)

	fileUnixTime = 0x0002
	fileCRC32    = 0x0004

	endNotLast = 0x0001 // more volumes follow

	attrArchive = 0x20 // Windows FILE_ATTRIBUTE_ARCHIVE
	hostWindows = 0

	hashBuf = 1 << 20
)

// source is the state shared by all volumes (and their clones) of one file.
type source struct {
	path  string
	entry entry

	mu    sync.Mutex
	parts []*part
	wake  chan struct{} // closed and replaced when a part is done or loses a reader
}

// part is the section of the source stored in one volume.
type part struct {
	off, n int64

	sum     uint32 // CRC32 of the first hashed bytes of the section
	hashed  int64
	readers int    // open volumes on this part
	opened  bool   // was opened at least once
	head    []byte // signature, main header and file header, once sum is final
}

func (p *part) done() bool { return p.hashed == p.n }

// notify wakes up WaitHead callers. s.mu must be held.
func (s *source) notify() {
	close(s.wake)
	s.wake = make(chan struct{})
}

// feed adds data, read from offset at of part i, to the checksum of the part
// if it continues where the checksum stops.
func (s *source) feed(i int, at int64, data []byte) {
	p := s.parts[i]

	s.mu.Lock()
	defer s.mu.Unlock()

	end := at + int64(len(data))
	if at > p.hashed || end <= p.hashed {
		return
	}
	p.sum = crc32.Update(p.sum, crc32.IEEETable, data[p.hashed-at:])
	p.hashed = end
	if p.done() {
		s.notify()
	}
}

// hashTo reads part i from f until its first to bytes are in the checksum.
func (s *source) hashTo(f *os.File, i int, to int64) error {
	p := s.parts[i]

	var buf []byte
	for {
		s.mu.Lock()
		at := p.hashed
		s.mu.Unlock()
		if at >= to {
			return nil
		}

		if buf == nil {
			buf = make([]byte, min(hashBuf, to-at))
		}
		chunk := buf[:min(int64(len(buf)), to-at)]
		n, err := f.ReadAt(chunk, p.off+at)
		if n == 0 {
			if err == nil || err == io.EOF {
				err = io.ErrUnexpectedEOF // source was truncated
			}
			return err
		}
		s.feed(i, at, chunk[:n])
	}
}

// unchanged checks that f still is the file the volumes were planned for.
func (s *source) unchanged(f *os.File) error {
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	if stat.Size() != s.entry.size || !stat.ModTime().Equal(s.entry.mtime) {
		return fmt.Errorf("%s changed while it was being uploaded", s.path)
	}
	return nil
}

// Volume is one generated RAR volume of a split file. Open it before
// reading; it implements io.ReadSeeker and io.Closer.
type Volume struct {
	name string // archive file name, e.g. movie.part1.rar
	src  *source
	i    int // index of the volume, and of its part in src

	headLen int64
	tail    []byte // end of archive header

	f   *os.File
	pos int64
}

// Name returns the archive file name of the volume, e.g. movie.part1.rar.
func (v *Volume) Name() string { return v.name }

// Source returns the path of the file this volume is a part of.
func (v *Volume) Source() string { return v.src.path }

// Size returns the size of the volume in bytes.
func (v *Volume) Size() int64 { return v.headLen + v.src.parts[v.i].n + int64(len(v.tail)) }

// Open opens the source file. It fails if the source changed since Split,
// because the volumes would no longer fit together.
func (v *Volume) Open() error {
	if v.f != nil {
		return nil
	}

	f, err := os.Open(v.src.path)
	if err != nil {
		return err
	}
	if err = v.src.unchanged(f); err != nil {
		_ = f.Close()
		return err
	}

	v.src.mu.Lock()
	p := v.src.parts[v.i]
	p.readers++
	p.opened = true
	v.src.mu.Unlock()

	v.f, v.pos = f, 0
	return nil
}

func (v *Volume) Read(b []byte) (int, error) {
	if v.f == nil {
		return 0, os.ErrClosed
	}
	if len(b) == 0 {
		return 0, nil
	}

	p := v.src.parts[v.i]
	dataEnd := v.headLen + p.n

	var (
		n   int
		err error
	)
	switch {
	case v.pos < v.headLen:
		head, herr := v.header()
		if herr != nil {
			return 0, herr
		}
		n = copy(b, head[v.pos:])
	case v.pos < dataEnd:
		at := v.pos - v.headLen
		if rest := p.n - at; int64(len(b)) > rest {
			b = b[:rest]
		}
		// the checksum needs every byte in order: catch up on skipped data
		if err = v.src.hashTo(v.f, v.i, at); err != nil {
			return 0, err
		}
		n, err = v.f.ReadAt(b, p.off+at)
		if n > 0 {
			v.src.feed(v.i, at, b[:n])
			err = nil // report a short read on the next call
		} else if err == io.EOF {
			err = io.ErrUnexpectedEOF // source was truncated
		}
	case v.pos < dataEnd+int64(len(v.tail)):
		n = copy(b, v.tail[v.pos-dataEnd:])
	default:
		return 0, io.EOF
	}

	v.pos += int64(n)
	return n, err
}

// header returns everything in front of the data. It needs the checksum of
// the data of this volume and, in the last volume, of the whole file;
// whatever has not been read yet is read here.
func (v *Volume) header() ([]byte, error) {
	s, p := v.src, v.src.parts[v.i]

	s.mu.Lock()
	h := p.head
	s.mu.Unlock()
	if h != nil {
		return h, nil
	}

	last := v.i == len(s.parts)-1
	first := v.i
	if last {
		first = 0
	}
	for j := first; j <= v.i; j++ {
		if err := s.hashTo(v.f, j, s.parts[j].n); err != nil {
			return nil, err
		}
	}
	// the checksums are only worth something if the source stayed the same
	if err := s.unchanged(v.f); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if p.head == nil {
		// A split file stores the CRC32 of the data in each volume, except
		// in the last one, which stores the CRC32 of the whole file.
		sum := p.sum
		if last {
			sum = s.parts[0].sum
			for _, next := range s.parts[1:] {
				sum = crc32Combine(sum, next.sum, next.n)
			}
		}
		p.head = head(s.entry, v.i, len(s.parts), p.n, sum)
	}
	return p.head, nil
}

// WaitHead blocks until the header of the volume can be built without
// reading source data a second time. A reader that wants to read the source
// only once reads everything behind the first bytes of the volume, calls
// WaitHead and reads the start of the volume last.
//
// Only the last volume ever waits: its header holds the checksum of the
// whole file, so the data of every earlier volume has to be read first.
// WaitHead waits for the earlier volumes that are open, and fails if one of
// them was closed before all of its data was read.
func (v *Volume) WaitHead(ctx context.Context) error {
	s := v.src
	if v.i != len(s.parts)-1 {
		return nil
	}

	for {
		s.mu.Lock()
		busy := false
		for j, p := range s.parts[:v.i] {
			switch {
			case p.done():
			case p.readers > 0:
				busy = true
			case p.opened:
				s.mu.Unlock()
				return fmt.Errorf("volume %d of %s was not read completely", j+1, s.path)
			}
		}
		wake := s.wake
		s.mu.Unlock()

		if !busy {
			return nil
		}
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (v *Volume) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += v.pos
	case io.SeekEnd:
		offset += v.Size()
	default:
		return 0, errors.New("rar5: invalid whence")
	}
	if offset < 0 {
		return 0, errors.New("rar5: negative position")
	}
	v.pos = offset
	return offset, nil
}

// Close closes the source file. The volume can be opened again.
func (v *Volume) Close() error {
	if v.f == nil {
		return nil
	}
	err := v.f.Close()
	v.f = nil

	v.src.mu.Lock()
	v.src.parts[v.i].readers--
	v.src.notify()
	v.src.mu.Unlock()

	return err
}

// Clone returns an independent, closed copy of the volume, to read it
// without disturbing the position of v.
func (v *Volume) Clone() *Volume {
	c := *v
	c.f, c.pos = nil, 0
	return &c
}

// WriteFile writes the volume into dir under its archive name and returns
// the path. An existing file of that name is replaced.
func (v *Volume) WriteFile(dir string) (string, error) {
	c := v.Clone() // v may be in use
	if err := c.Open(); err != nil {
		return "", err
	}
	defer c.Close()

	dst := filepath.Join(dir, v.name)
	tmp, err := os.CreateTemp(dir, v.name+".*.tmp")
	if err != nil {
		return "", err
	}
	_, err = io.CopyBuffer(tmp, c, make([]byte, hashBuf))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return dst, nil
}

// Split plans store-only RAR volumes of at most volSize bytes for the file
// at path. Volumes are named base.part1.rar, base.part2.rar, ... (zero
// padded when there are ten or more). Nothing is read or written here: the
// checksums RAR stores are computed while the volumes are read.
func Split(path, base string, volSize int64) ([]*Volume, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}

	src := &source{
		path: path,
		entry: entry{
			name:  filepath.Base(path),
			size:  stat.Size(),
			mtime: stat.ModTime(),
		},
		wake: make(chan struct{}),
	}

	sizes, err := plan(src.entry, volSize)
	if err != nil {
		return nil, err
	}

	width := len(fmt.Sprint(len(sizes)))
	vols := make([]*Volume, len(sizes))
	off := int64(0)
	for i, n := range sizes {
		src.parts = append(src.parts, &part{off: off, n: n})
		vols[i] = &Volume{
			name:    fmt.Sprintf("%s.part%0*d.rar", base, width, i+1),
			src:     src,
			i:       i,
			headLen: int64(len(head(src.entry, i, len(sizes), n, 0))),
			tail:    tail(i, len(sizes)),
		}
		off += n
	}

	return vols, nil
}

type entry struct {
	name  string
	size  int64
	mtime time.Time
}

// plan returns the number of source bytes stored in each volume.
func plan(e entry, volSize int64) ([]int64, error) {
	var sizes []int64
	for rest := e.size; rest > 0 || len(sizes) == 0; {
		// header length for the largest values its variable length fields
		// can take here, so a volume never exceeds volSize
		overhead := int64(len(head(e, len(sizes), len(sizes)+2, volSize, 0)) + len(tail(0, 2)))
		room := volSize - overhead
		if room <= 0 {
			return nil, fmt.Errorf("volume size %d is too small", volSize)
		}
		n := min(rest, room)
		sizes = append(sizes, n)
		rest -= n
	}
	return sizes, nil
}

// head builds everything in front of the data of volume i (0-based) of
// total: signature, main archive header and file header.
func head(e entry, i, total int, dataSize int64, sum uint32) []byte {
	out := append([]byte(nil), signature...)

	// main archive header
	archFlags := uint64(0)
	if total > 1 {
		archFlags |= mainVolume
	}
	body := []byte(nil)
	if i > 0 {
		archFlags |= mainVolNumber
		body = appendVint(body, uint64(i))
	}
	body = append(appendVint(nil, archFlags), body...)
	out = append(out, block(headMain, flagSkipUnknown, -1, body)...)

	// file header
	flags := uint64(0)
	if i > 0 {
		flags |= flagSplitBefore
	}
	if i < total-1 {
		flags |= flagSplitAfter
	}

	mtime := e.mtime.Unix()
	if mtime < 0 || mtime > 0xFFFFFFFF {
		mtime = 0
	}

	body = appendVint(nil, fileUnixTime|fileCRC32)
	body = appendVint(body, uint64(e.size)) // unpacked size
	body = appendVint(body, attrArchive)
	body = binary.LittleEndian.AppendUint32(body, uint32(mtime))
	body = binary.LittleEndian.AppendUint32(body, sum)
	body = appendVint(body, 0) // compression: version 0, method 0 (store)
	body = appendVint(body, hostWindows)
	body = appendVint(body, uint64(len(e.name)))
	body = append(body, e.name...)

	return append(out, block(headFile, flags, dataSize, body)...)
}

// tail builds the end of archive header of volume i (0-based) of total.
func tail(i, total int) []byte {
	flags := uint64(0)
	if i < total-1 {
		flags |= endNotLast
	}
	return block(headEnd, flagSkipUnknown, -1, appendVint(nil, flags))
}

// block assembles a header: CRC32, size, type, flags, optional data size
// (dataSize >= 0) and the type specific body.
func block(typ, flags uint64, dataSize int64, body []byte) []byte {
	h := appendVint(nil, typ)
	if dataSize >= 0 {
		flags |= flagData
	}
	h = appendVint(h, flags)
	if dataSize >= 0 {
		h = appendVint(h, uint64(dataSize))
	}
	h = append(h, body...)

	sized := append(appendVint(nil, uint64(len(h))), h...)
	out := binary.LittleEndian.AppendUint32(make([]byte, 0, 4+len(sized)), crc32.ChecksumIEEE(sized))
	return append(out, sized...)
}

// appendVint appends v as a RAR variable length integer: 7 bits per byte,
// lowest first, high bit set on all but the last byte.
func appendVint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}
