package up

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/expr-lang/expr/vm"
	"github.com/gabriel-vasile/mimetype"
	"github.com/go-faster/errors"
	"github.com/go-viper/mapstructure/v2"
	"github.com/gotd/td/telegram/message/entity"
	"github.com/gotd/td/telegram/message/html"
	"github.com/gotd/td/telegram/peers"

	"github.com/iyear/tdl/core/uploader"
	"github.com/iyear/tdl/core/util/mediautil"
	"github.com/iyear/tdl/core/util/tutil"
	"github.com/iyear/tdl/pkg/rar5"
	"github.com/iyear/tdl/pkg/texpr"
)

type File struct {
	File  string
	Thumb string

	// set for RAR volumes made by --split; File is then a virtual path
	// next to the source, the volume itself only exists as vol
	Part  int
	Parts int
	vol   *rar5.Volume
	group *splitGroup // all volumes of one source file
}

type dest struct {
	Peer   string
	Thread int
}

type iter struct {
	files   []*File
	to      *vm.Program
	caption *vm.Program
	chat    string
	topic   int
	photo   bool
	remove  bool
	delay   time.Duration
	manager *peers.Manager

	split   *splitter // nil unless --split
	pending []*File   // volumes of the current split file not yet handed out
	started bool

	cur  int
	err  error
	file uploader.Elem
}

func newIter(files []*File, to, caption *vm.Program, chat string, topic int, photo, remove bool, delay time.Duration, manager *peers.Manager, split *splitter) *iter {
	return &iter{
		files:   files,
		to:      to,
		caption: caption,
		chat:    chat,
		topic:   topic,
		photo:   photo,
		remove:  remove,
		delay:   delay,
		manager: manager,
		split:   split,

		cur:  0,
		err:  nil,
		file: nil,
	}
}

func (i *iter) Next(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		i.err = ctx.Err()
		return false
	default:
	}

	if i.err != nil || (len(i.pending) == 0 && i.cur >= len(i.files)) {
		return false
	}

	// if delay is set, sleep for a while for each iteration
	if i.delay > 0 && i.started { // skip first delay
		time.Sleep(i.delay)
	}
	i.started = true

	cur, err := i.advance(ctx)
	if err != nil {
		i.err = errors.Wrap(err, "split file")
		return false
	}

	file, err := i.next(ctx, cur)
	if err != nil {
		i.err = err
		return false
	}

	i.file = file
	return true
}

// advance returns the next file to upload. With --split, an oversize file is
// turned into RAR volumes only when it is reached, and its volumes are handed
// out one by one before the next file is looked at.
func (i *iter) advance(ctx context.Context) (*File, error) {
	if len(i.pending) > 0 {
		f := i.pending[0]
		i.pending = i.pending[1:]
		return f, nil
	}

	f := i.files[i.cur]
	i.cur++
	if i.split == nil {
		return f, nil
	}

	vols, err := i.split.pack(ctx, f)
	if err != nil {
		return nil, err
	}
	if len(vols) == 0 {
		return f, nil
	}

	i.pending = vols[1:]
	return vols[0], nil
}

func (i *iter) next(ctx context.Context, cur *File) (*iterElem, error) {
	file, err := i.resolveFile(cur)
	if err != nil {
		return nil, errors.Wrap(err, "resolve file")
	}

	env := exprEnv(ctx, cur)

	to, thread, err := i.resolveDest(ctx, env)
	if err != nil {
		return nil, errors.Wrap(err, "resolve destination")
	}

	caption, err := i.resolveCaption(env)
	if err != nil {
		return nil, errors.Wrap(err, "resolve caption")
	}

	thumb, err := i.resolveThumb(cur.Thumb)
	if err != nil {
		return nil, errors.Wrap(err, "resolve thumbnail")
	}

	return &iterElem{
		file:    file,
		thumb:   thumb,
		to:      to,
		caption: caption,
		thread:  thread,

		asPhoto: i.photo,
		remove:  i.remove,
		vol:     cur.vol,
		group:   cur.group,
	}, nil
}

func (i *iter) resolveFile(cur *File) (*uploaderFile, error) {
	if cur.vol != nil { // generated RAR volume, reads straight from the source
		if err := cur.vol.Open(); err != nil {
			return nil, errors.Wrap(err, "open volume")
		}

		return &uploaderFile{
			ReadSeeker: cur.vol,
			Closer:     cur.vol,
			path:       cur.File,
			size:       cur.vol.Size(),
		}, nil
	}

	f, err := os.Open(cur.File)
	if err != nil {
		return nil, errors.Wrap(err, "open file")
	}

	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, errors.Wrap(err, "stat file")
	}

	return &uploaderFile{
		ReadSeeker: f,
		Closer:     f,
		path:       cur.File,
		size:       stat.Size(),
	}, nil
}

func (i *iter) resolveDest(ctx context.Context, env Env) (peers.Peer, int, error) {
	if i.chat != "" { // compatible with old version
		to, err := i.resolvePeer(ctx, i.chat)
		if err != nil {
			return nil, 0, errors.Wrap(err, "resolve chat")
		}

		return to, i.topic, nil
	}

	// message routing
	result, err := texpr.Run(i.to, env)
	if err != nil {
		return nil, 0, errors.Wrap(err, "parse expression")
	}

	var (
		to     peers.Peer
		thread int
	)

	switch r := result.(type) {
	case string:
		// pure chat, no reply to, which is a compatible with old version
		// and a convenient way to send message to self
		to, err = i.resolvePeer(ctx, r)
	case map[string]interface{}:
		// chat with reply to topic or message
		var d dest

		if err = mapstructure.WeakDecode(r, &d); err != nil {
			return nil, 0, errors.Wrapf(err, "decode dest: %v", result)
		}

		to, err = i.resolvePeer(ctx, d.Peer)
		thread = d.Thread
	default:
		return nil, 0, errors.Errorf("message router must return string or dest: %T", result)
	}

	if err != nil {
		return nil, 0, errors.Wrap(err, "resolve peer")
	}

	return to, thread, nil
}

func (i *iter) resolvePeer(ctx context.Context, peer string) (peers.Peer, error) {
	if peer == "" { // self
		return i.manager.Self(ctx)
	}

	return tutil.GetInputPeer(ctx, i.manager, peer)
}

func (i *iter) resolveCaption(env Env) (*entity.Builder, error) {
	// parse caption
	captionStr, err := texpr.Run(i.caption, env)
	if err != nil {
		return nil, errors.Wrap(err, "parse caption")
	}

	r, ok := captionStr.(string)
	if !ok {
		return nil, errors.Errorf("caption must return string, got %T", captionStr)
	}

	caption := &entity.Builder{}
	if len(r) > 0 {
		if err = html.HTML(strings.NewReader(r), caption, html.Options{
			UserResolver:          nil,
			DisableTelegramEscape: false,
		}); err != nil {
			return nil, errors.Wrap(err, "parse caption HTML")
		}
	}

	return caption, nil
}

func (i *iter) resolveThumb(path string) (*uploaderFile, error) {
	if path == "" {
		return nil, nil
	}

	// has thumbnail
	mime, err := mimetype.DetectFile(path)
	if err != nil || !mediautil.IsImage(mime.String()) { // TODO(iyear): jpg only
		return nil, errors.Wrapf(err, "invalid thumbnail file: %v", path)
	}

	thumb, err := os.Open(path)
	if err != nil {
		return nil, errors.Wrap(err, "open thumbnail file")
	}

	return &uploaderFile{
		ReadSeeker: thumb,
		Closer:     thumb,
		path:       path,
		size:       0,
	}, nil
}

func (i *iter) Value() uploader.Elem {
	return i.file
}

func (i *iter) Err() error {
	return i.err
}
