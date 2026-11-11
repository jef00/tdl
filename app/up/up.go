package up

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"
	"github.com/fatih/color"
	"github.com/gabriel-vasile/mimetype"
	"github.com/go-faster/errors"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/peers"
	"github.com/spf13/viper"
	"go.uber.org/multierr"
	"go.uber.org/zap"

	"github.com/iyear/tdl/core/dcpool"
	"github.com/iyear/tdl/core/logctx"
	"github.com/iyear/tdl/core/storage"
	"github.com/iyear/tdl/core/tclient"
	"github.com/iyear/tdl/core/uploader"
	"github.com/iyear/tdl/core/util/tutil"
	"github.com/iyear/tdl/pkg/consts"
	"github.com/iyear/tdl/pkg/prog"
	"github.com/iyear/tdl/pkg/texpr"
	"github.com/iyear/tdl/pkg/utils"
)

type Options struct {
	Chat     string
	Thread   int
	To       string
	Paths    []string
	Includes []string
	Excludes []string
	Remove   bool
	Photo    bool
	Caption  string

	// Split uploads files larger than SplitSize as store-only RAR volumes.
	Split bool
	// SplitSize in bytes; 0 picks the account limit (2000 MiB, or 4000 MiB with Premium).
	SplitSize int64
	// KeepVolumes saves each uploaded RAR volume next to its source file.
	KeepVolumes bool
	// Prune removes directories left empty under the input paths (requires Remove).
	Prune bool
}

type Env struct {
	FilePath  string `comment:"File path"`
	FileName  string `comment:"File name"`
	FileExt   string `comment:"File extension"`
	ThumbPath string `comment:"Thumbnail path"`
	MIME      string `comment:"File mime type"`
	Part      int    `comment:"RAR volume number of a split file (1-based), 0 if not split"`
	Parts     int    `comment:"Total RAR volumes of a split file, 0 if not split"`
}

func Run(ctx context.Context, c *telegram.Client, kvd storage.Storage, opts Options) (rerr error) {
	if opts.To == "-" || opts.Caption == "-" {
		fg := texpr.NewFieldsGetter(nil)

		fields, err := fg.Walk(exprEnv(context.Background(), nil))
		if err != nil {
			return fmt.Errorf("failed to walk fields: %w", err)
		}

		fmt.Print(fg.Sprint(fields, true))
		return nil
	}

	files, err := walk(opts.Paths, opts.Includes, opts.Excludes)
	if err != nil {
		return err
	}

	pool := dcpool.NewPool(c,
		int64(viper.GetInt(consts.FlagPoolSize)),
		tclient.NewDefaultMiddlewares(ctx, viper.GetDuration(consts.FlagReconnectTimeout))...)
	defer multierr.AppendInvoke(&rerr, multierr.Close(pool))

	manager := peers.Options{Storage: storage.NewPeers(kvd)}.Build(pool.Default(ctx))

	to, err := resolveDest(ctx, manager, opts.To)
	if err != nil {
		return errors.Wrap(err, "get target peer")
	}

	var split *splitter
	if opts.Split {
		size, err := resolveSplitSize(ctx, manager, opts.SplitSize)
		if err != nil {
			return errors.Wrap(err, "resolve split size")
		}
		color.Blue("Split size: %s", utils.Byte.FormatBinaryBytes(size))

		// Volumes are generated while uploading (see iter.advance), so there
		// is nothing on disk to clean up, whether the run ends or is interrupted.
		split = newSplitter(size, opts.KeepVolumes)
	}

	color.Blue("Files count: %d", len(files))

	caption, err := resolveCaption(ctx, opts.Caption)
	if err != nil {
		return errors.Wrap(err, "get caption")
	}

	upProgress := prog.New(utils.Byte.FormatBinaryBytes)
	upProgress.SetNumTrackersExpected(len(files))
	if !viper.GetBool(consts.FlagDisableProgressPS) {
		prog.EnablePS(ctx, upProgress)
	}

	if split != nil {
		split.log = func(msg string) { upProgress.Log(color.BlueString(msg)) }
	}

	options := uploader.Options{
		Client:   pool.Default(ctx),
		Threads:  viper.GetInt(consts.FlagThreads),
		Iter:     newIter(files, to, caption, opts.Chat, opts.Thread, opts.Photo, opts.Remove, viper.GetDuration(consts.FlagDelay), manager, split),
		Progress: newProgress(upProgress),
	}

	up := uploader.New(options)

	go upProgress.Render()

	err = up.Upload(ctx, viper.GetInt(consts.FlagLimit))
	prog.Wait(ctx, upProgress)

	if opts.Remove && opts.Prune {
		for _, dir := range pruneEmptyDirs(opts.Paths) {
			logctx.From(ctx).Info("pruned empty dir", zap.String("dir", dir))
		}
	}

	return err
}

// detectMIME sniffs a file on disk, or a generated RAR volume through its reader.
func detectMIME(file *File) (*mimetype.MIME, error) {
	if file.vol == nil {
		return mimetype.DetectFile(file.File)
	}

	vol := file.vol.Clone() // the original may already be open for upload
	if err := vol.Open(); err != nil {
		return nil, err
	}
	defer vol.Close()

	return mimetype.DetectReader(vol)
}

func resolveSplitSize(ctx context.Context, manager *peers.Manager, size int64) (int64, error) {
	limit := SplitLimitDefault

	self, err := manager.Self(ctx)
	if err != nil {
		return 0, errors.Wrap(err, "get self")
	}
	if self.Raw().Premium {
		limit = SplitLimitPremium
	}

	return alignSplitSize(size, limit), nil
}

func resolveDest(ctx context.Context, manager *peers.Manager, input string) (*vm.Program, error) {
	compile := func(i string) (*vm.Program, error) {
		return expr.Compile(i, expr.Env(exprEnv(ctx, nil)))
	}

	if input == "" {
		return compile(`""`)
	}

	if exp, err := os.ReadFile(input); err == nil {
		return compile(string(exp))
	}

	if _, err := tutil.GetInputPeer(ctx, manager, input); err == nil {
		return compile(fmt.Sprintf(`"%s"`, input))
	}

	return compile(input)
}

func resolveCaption(ctx context.Context, input string) (*vm.Program, error) {
	compile := func(i string) (*vm.Program, error) {
		// we pass empty peer and message to enable type checking
		return expr.Compile(i, expr.Env(exprEnv(ctx, nil)), expr.AsKind(reflect.String))
	}

	// default
	if input == "" {
		return compile(`""`)
	}

	// file
	if exp, err := os.ReadFile(input); err == nil {
		return compile(string(exp))
	}

	// text
	return compile(input)
}

func exprEnv(ctx context.Context, file *File) Env {
	if file == nil {
		return Env{}
	}

	extension := filepath.Ext(file.File)
	filename := strings.TrimSuffix(filepath.Base(file.File), extension)
	mime, err := detectMIME(file)
	if err != nil {
		mime = &mimetype.MIME{}
		logctx.From(ctx).Error("detect file mime", zap.Error(err))
	}

	return Env{
		FilePath:  file.File,
		FileName:  filename,
		FileExt:   extension,
		ThumbPath: file.Thumb,
		MIME:      mime.String(),
		Part:      file.Part,
		Parts:     file.Parts,
	}
}
