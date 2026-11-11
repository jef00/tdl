// Package rar5 generates store-only RAR 5.0 multi-volume archives on the fly.
//
// A volume is never written to disk: it is a small generated header, a
// section of the source file and a small trailer, exposed as an
// io.ReadSeeker. Stored (uncompressed) RAR data is the file content itself,
// so no RAR tool is needed to create the volumes, and any RAR 5 capable tool
// (WinRAR, unrar, 7-Zip, ...) can extract them.
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
	"time"
)

var signature = []byte{'R', 'a', 'r', '!', 0x1A, 0x07, 0x01, 0x00}

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

// Volume is one generated RAR volume of a split file. Open it before
// reading; it implements io.ReadSeeker and io.Closer.
type Volume struct {
	name string // archive file name, e.g. movie.part1.rar
	src  string // source file

	head []byte // signature, main header and file header
	tail []byte // end of archive header
	off  int64  // start of this volume's data in src
	n    int64  // length of this volume's data

	srcSize  int64 // source size and mtime when the volume was planned
	srcMtime time.Time

	f   *os.File
	pos int64
}

// Name returns the archive file name of the volume, e.g. movie.part1.rar.
func (v *Volume) Name() string { return v.name }

// Source returns the path of the file this volume is a part of.
func (v *Volume) Source() string { return v.src }

// Size returns the size of the volume in bytes.
func (v *Volume) Size() int64 { return int64(len(v.head)) + v.n + int64(len(v.tail)) }

// Open opens the source file. It fails if the source changed since Split,
// because the checksums stored in the volume would no longer match.
func (v *Volume) Open() error {
	if v.f != nil {
		return nil
	}

	f, err := os.Open(v.src)
	if err != nil {
		return err
	}
	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	if stat.Size() != v.srcSize || !stat.ModTime().Equal(v.srcMtime) {
		_ = f.Close()
		return fmt.Errorf("%s changed while it was being uploaded", v.src)
	}

	v.f, v.pos = f, 0
	return nil
}

func (v *Volume) Read(p []byte) (int, error) {
	if v.f == nil {
		return 0, os.ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}

	headEnd := int64(len(v.head))
	dataEnd := headEnd + v.n

	var (
		n   int
		err error
	)
	switch {
	case v.pos < headEnd:
		n = copy(p, v.head[v.pos:])
	case v.pos < dataEnd:
		if rest := dataEnd - v.pos; int64(len(p)) > rest {
			p = p[:rest]
		}
		n, err = v.f.ReadAt(p, v.off+v.pos-headEnd)
		if n > 0 {
			err = nil // report a short read on the next call
		} else if err == io.EOF {
			err = io.ErrUnexpectedEOF // source was truncated
		}
	case v.pos < dataEnd+int64(len(v.tail)):
		n = copy(p, v.tail[v.pos-dataEnd:])
	default:
		return 0, io.EOF
	}

	v.pos += int64(n)
	return n, err
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
// padded when there are ten or more). The file is read once to compute the
// checksums RAR stores per volume; nothing is written.
func Split(ctx context.Context, path, base string, volSize int64) ([]*Volume, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}

	entry := entry{
		name:  filepath.Base(path),
		size:  stat.Size(),
		mtime: stat.ModTime(),
	}

	sizes, err := plan(entry, volSize)
	if err != nil {
		return nil, err
	}

	// A split file stores the CRC32 of the data in each volume, except in
	// the last one, which stores the CRC32 of the whole file.
	sums := make([]uint32, len(sizes))
	whole := crc32.NewIEEE()
	buf := make([]byte, hashBuf)
	for i, n := range sizes {
		part := crc32.NewIEEE()
		if err = copyN(ctx, io.MultiWriter(part, whole), f, n, buf); err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		sums[i] = part.Sum32()
	}
	sums[len(sums)-1] = whole.Sum32()

	// the size was sampled before reading: make sure it still holds
	if after, err := f.Stat(); err != nil {
		return nil, err
	} else if after.Size() != entry.size || !after.ModTime().Equal(entry.mtime) {
		return nil, fmt.Errorf("%s changed while it was being read", path)
	}

	width := len(fmt.Sprint(len(sizes)))
	vols := make([]*Volume, len(sizes))
	off := int64(0)
	for i, n := range sizes {
		vols[i] = &Volume{
			name:     fmt.Sprintf("%s.part%0*d.rar", base, width, i+1),
			src:      path,
			head:     head(entry, i, len(sizes), n, sums[i]),
			tail:     tail(i, len(sizes)),
			off:      off,
			n:        n,
			srcSize:  entry.size,
			srcMtime: entry.mtime,
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

// copyN copies exactly n bytes and stops early when ctx is done.
func copyN(ctx context.Context, dst io.Writer, src io.Reader, n int64, buf []byte) error {
	for n > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk := buf
		if int64(len(chunk)) > n {
			chunk = chunk[:n]
		}
		read, err := io.ReadFull(src, chunk)
		if read > 0 {
			_, _ = dst.Write(chunk[:read])
			n -= int64(read)
		}
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
	}
	return nil
}
