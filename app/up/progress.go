package up

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/fatih/color"
	"github.com/go-faster/errors"
	pw "github.com/jedib0t/go-pretty/v6/progress"

	"github.com/iyear/tdl/core/uploader"
	"github.com/iyear/tdl/pkg/prog"
	"github.com/iyear/tdl/pkg/utils"
)

type progress struct {
	pw       pw.Writer
	trackers *sync.Map // map[tuple]*pw.Tracker
}

type tuple struct {
	name string
	to   int64
}

func newProgress(p pw.Writer) *progress {
	return &progress{
		pw:       p,
		trackers: &sync.Map{},
	}
}

func (p *progress) OnAdd(elem uploader.Elem) {
	tracker := prog.AppendTracker(p.pw, utils.Byte.FormatBinaryBytes, p.processMessage(elem), elem.File().Size())
	p.trackers.Store(p.tuple(elem), tracker)
}

func (p *progress) OnUpload(elem uploader.Elem, state uploader.ProgressState) {
	tracker, ok := p.trackers.Load(p.tuple(elem))
	if !ok {
		return
	}

	t := tracker.(*pw.Tracker)
	t.UpdateTotal(state.Total)
	t.SetValue(state.Uploaded)
}

func (p *progress) OnDone(elem uploader.Elem, err error) {
	tracker, ok := p.trackers.Load(p.tuple(elem))
	if !ok {
		return
	}
	t := tracker.(*pw.Tracker)
	e := elem.(*iterElem)

	if cerr := p.closeFile(e); cerr != nil {
		if e.group != nil {
			e.group.done(cerr)
		}
		p.fail(t, elem, errors.Wrap(cerr, "close file"))
		return
	}

	if err != nil {
		if e.group != nil {
			e.group.done(err)
		}
		p.fail(t, elem, errors.Wrap(err, "progress"))
		return
	}

	path := e.file.path
	if e.group != nil {
		// A RAR volume only exists while it is uploaded. --keep-volumes
		// saves a copy next to the source; if that fails the source must
		// survive --rm, so the part counts as failed.
		var kerr error
		if e.vol != nil && e.group.keep {
			_, kerr = e.vol.WriteFile(filepath.Dir(e.group.source))
		}

		// split file: act only after its last part succeeded
		last := e.group.done(kerr)
		if kerr != nil {
			p.fail(t, elem, errors.Wrap(kerr, "keep volume"))
			return
		}
		if !last {
			return
		}
		path = e.group.source
	}

	if e.remove {
		if err := os.Remove(path); err != nil {
			p.fail(t, elem, errors.Wrap(err, "remove file"))
			return
		}
	}
}

func (p *progress) closeFile(e *iterElem) error {
	if err := e.file.Close(); err != nil {
		return errors.Wrap(err, "close file")
	}

	if e.thumb != nil {
		if err := e.thumb.Close(); err != nil {
			return errors.Wrap(err, "close thumb")
		}
	}

	return nil
}

func (p *progress) fail(t *pw.Tracker, elem uploader.Elem, err error) {
	p.pw.Log(color.RedString("%s error: %s", p.elemString(elem), err.Error()))
	t.MarkAsErrored()
}

func (p *progress) tuple(elem uploader.Elem) tuple {
	return tuple{elem.(*iterElem).file.path, elem.(*iterElem).to.ID()}
}

func (p *progress) processMessage(elem uploader.Elem) string {
	return p.elemString(elem)
}

func (p *progress) elemString(elem uploader.Elem) string {
	e := elem.(*iterElem)
	return fmt.Sprintf("%s -> %s(%d)", e.file.path, e.to.VisibleName(), e.to.ID())
}
