package uploader

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math/rand"
	"slices"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/constant"
	"github.com/gotd/td/tg"
)

// sealedFile is a HeadLast file like a RAR volume: its first four bytes are
// the checksum of everything behind them, known once all of that was read.
// Reading the start before the rest is noted as early.
type sealedFile struct {
	name string
	body []byte
	pos  int64

	sum    uint32
	summed int64 // bytes of body in sum
	waited bool
	early  bool // the start was read before the rest
}

func (f *sealedFile) Name() string { return f.name }
func (f *sealedFile) Size() int64  { return int64(len(f.body)) + 4 }

func (f *sealedFile) Seek(offset int64, whence int) (int64, error) {
	if whence != io.SeekStart {
		return 0, errors.New("unsupported")
	}
	f.pos = offset
	return offset, nil
}

func (f *sealedFile) Read(p []byte) (int, error) {
	if f.pos >= f.Size() {
		return 0, io.EOF
	}

	if f.pos < 4 {
		if f.summed != int64(len(f.body)) || !f.waited {
			f.early = true
		}
		head := binary.LittleEndian.AppendUint32(nil, f.sum)
		n := copy(p, head[f.pos:])
		f.pos += int64(n)
		return n, nil
	}

	at := f.pos - 4
	n := copy(p, f.body[at:])
	if end := at + int64(n); end > f.summed { // the checksum takes the bytes in order
		f.sum = crc32.Update(f.sum, crc32.IEEETable, f.body[f.summed:end])
		f.summed = end
	}
	f.pos += int64(n)
	return n, nil
}

func (f *sealedFile) WaitHead(context.Context) error {
	f.waited = true
	return nil
}

func (f *sealedFile) want() []byte {
	return append(binary.LittleEndian.AppendUint32(nil, crc32.ChecksumIEEE(f.body)), f.body...)
}

func newSealedFile(name string, size int) *sealedFile {
	body := make([]byte, size-4)
	rand.New(rand.NewSource(int64(size))).Read(body)
	return &sealedFile{name: name, body: body}
}

// A HeadLast file is uploaded back to front: its first part goes last, when
// the checksum in it is known from the upload of the rest.
func TestHeadLast(t *testing.T) {
	for _, size := range []int{
		constant.UploadMaxSmallSize + 1,
		constant.UploadMaxSmallSize + MaxPartSize,         // last part is a full one
		3*constant.UploadMaxSmallSize + MaxPartSize/2 + 1, // more parts than threads
	} {
		tgram := newFakeTelegram()
		file := newSealedFile("sealed.rar", size)

		if err := upload(t, tgram, file); err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if file.early {
			t.Errorf("size %d: the first part was read before the rest of the file", size)
		}

		if len(tgram.order) != 1 {
			t.Fatalf("size %d: %d files uploaded", size, len(tgram.order))
		}
		for id, order := range tgram.order {
			parts := (size + MaxPartSize - 1) / MaxPartSize
			// the first part is sent last, so it arrives among the last
			// ones, give or take the parts still on their way
			if first := slices.Index(order, 0); len(order) != parts || first < parts-2 {
				t.Errorf("size %d: %d of %d parts arrived, part 0 at position %d", size, len(order), parts, first)
			}
			content, err := tgram.file(id, parts)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(content, file.want()) {
				t.Errorf("size %d: uploaded file differs from the source", size)
			}
		}
	}
}

// A small HeadLast file goes the usual way, front to back, after WaitHead.
func TestHeadLastSmallFile(t *testing.T) {
	tgram := newFakeTelegram()
	file := newSealedFile("sealed.rar", MaxPartSize+100)

	if err := upload(t, tgram, file); err != nil {
		t.Fatal(err)
	}
	if !file.waited {
		t.Error("WaitHead was not called")
	}
	for _, order := range tgram.order {
		if len(order) != 2 || order[0] != 0 {
			t.Errorf("parts arrived as %v, want front to back", order)
		}
	}
}

// A failing part fails the upload instead of leaving it hanging.
func TestHeadLastFailure(t *testing.T) {
	boom := errors.New("boom")
	tgram := newFakeTelegram()
	tgram.fail = func(req bin.Encoder) error {
		if part, ok := req.(*tg.UploadSaveBigFilePartRequest); ok && part.FilePart == 7 {
			return boom
		}
		return nil
	}

	file := newSealedFile("sealed.rar", 2*constant.UploadMaxSmallSize)
	if err := upload(t, tgram, file); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the failed part", err)
	}
	if tgram.sent != 0 {
		t.Error("a file with a failed part was sent")
	}
}
