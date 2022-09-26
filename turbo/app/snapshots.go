package app

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/holiman/uint256"
	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/common/cmp"
	"github.com/ledgerwatch/erigon-lib/common/dir"
	"github.com/ledgerwatch/erigon-lib/compress"
	"github.com/ledgerwatch/erigon-lib/etl"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon-lib/kv/mdbx"
	libstate "github.com/ledgerwatch/erigon-lib/state"
	"github.com/ledgerwatch/erigon/cmd/hack/tool/fromdb"
	"github.com/ledgerwatch/erigon/cmd/utils"
	"github.com/ledgerwatch/erigon/core/rawdb"
	"github.com/ledgerwatch/erigon/eth/ethconfig"
	"github.com/ledgerwatch/erigon/internal/debug"
	"github.com/ledgerwatch/erigon/node/nodecfg/datadir"
	"github.com/ledgerwatch/erigon/params"
	"github.com/ledgerwatch/erigon/turbo/snapshotsync"
	"github.com/ledgerwatch/erigon/turbo/snapshotsync/snap"
	"github.com/ledgerwatch/log/v3"
	"github.com/urfave/cli"
)

const ASSERT = false

var snapshotCommand = cli.Command{
	Name:        "snapshots",
	Description: `Managing snapshots (historical data partitions)`,
	Subcommands: []cli.Command{
		{
			Name:   "create",
			Action: doSnapshotCommand,
			Usage:  "Create snapshots for given range of blocks",
			Before: func(ctx *cli.Context) error { return debug.Setup(ctx) },
			Flags: append([]cli.Flag{
				utils.DataDirFlag,
				SnapshotFromFlag,
				SnapshotToFlag,
				SnapshotSegmentSizeFlag,
			}, debug.Flags...),
		},
		{
			Name:   "index",
			Action: doIndicesCommand,
			Usage:  "Create all indices for snapshots",
			Before: func(ctx *cli.Context) error { return debug.Setup(ctx) },
			Flags: append([]cli.Flag{
				utils.DataDirFlag,
				SnapshotFromFlag,
				SnapshotRebuildFlag,
			}, debug.Flags...),
		},
		{
			Name:   "retire",
			Action: doRetireCommand,
			Usage:  "erigon snapshots uncompress a.seg | erigon snapshots compress b.seg",
			Before: func(ctx *cli.Context) error { return debug.Setup(ctx) },
			Flags: append([]cli.Flag{
				utils.DataDirFlag,
				SnapshotFromFlag,
				SnapshotToFlag,
				SnapshotEveryFlag,
			}, debug.Flags...),
		},
		{
			Name:   "uncompress",
			Action: doUncompress,
			Usage:  "erigon snapshots uncompress a.seg | erigon snapshots compress b.seg",
			Before: func(ctx *cli.Context) error { return debug.Setup(ctx) },
			Flags:  append([]cli.Flag{}, debug.Flags...),
		},
		{
			Name:   "compress",
			Action: doCompress,
			Before: func(ctx *cli.Context) error { return debug.Setup(ctx) },
			Flags:  append([]cli.Flag{utils.DataDirFlag}, debug.Flags...),
		},
		{
			Name:   "ram",
			Action: doRam,
			Before: func(ctx *cli.Context) error { return debug.Setup(ctx) },
			Flags:  append([]cli.Flag{utils.DataDirFlag}, debug.Flags...),
		},
		{
			Name:   "decompress_speed",
			Action: doDecompressSpeed,
			Before: func(ctx *cli.Context) error { return debug.Setup(ctx) },
			Flags:  append([]cli.Flag{utils.DataDirFlag}, debug.Flags...),
		},
	},
}

var (
	SnapshotFromFlag = cli.Uint64Flag{
		Name:  "from",
		Usage: "From block number",
		Value: 0,
	}
	SnapshotToFlag = cli.Uint64Flag{
		Name:  "to",
		Usage: "To block number. Zero - means unlimited.",
		Value: 0,
	}
	SnapshotEveryFlag = cli.Uint64Flag{
		Name:  "every",
		Usage: "Do operation every N blocks",
		Value: 1_000,
	}
	SnapshotSegmentSizeFlag = cli.Uint64Flag{
		Name:  "segment.size",
		Usage: "Amount of blocks in each segment",
		Value: snap.Erigon21SegmentSize,
	}
	SnapshotRebuildFlag = cli.BoolFlag{
		Name:  "rebuild",
		Usage: "Force rebuild",
	}
)

func preloadFileAsync(name string) {
	go func() {
		ff, _ := os.Open(name)
		_, _ = io.CopyBuffer(io.Discard, bufio.NewReaderSize(ff, 64*1024*1024), make([]byte, 64*1024*1024))
	}()
}

func doDecompressSpeed(cliCtx *cli.Context) error {
	args := cliCtx.Args()
	if len(args) != 1 {
		return fmt.Errorf("expecting .seg file path")
	}
	f := args[0]

	compress.SetDecompressionTableCondensity(9)

	preloadFileAsync(f)

	decompressor, err := compress.NewDecompressor(f)
	if err != nil {
		return err
	}
	defer decompressor.Close()
	t := time.Now()
	if err := decompressor.WithReadAhead(func() error {
		g := decompressor.MakeGetter()
		buf := make([]byte, 0, 16*etl.BufIOSize)
		for g.HasNext() {
			buf, _ = g.Next(buf[:0])
		}
		return nil
	}); err != nil {
		return err
	}
	log.Info("decompress speed", "took", time.Since(t))
	t = time.Now()
	if err := decompressor.WithReadAhead(func() error {
		g := decompressor.MakeGetter()
		for g.HasNext() {
			_ = g.Skip()
		}
		return nil
	}); err != nil {
		return err
	}
	log.Info("decompress skip speed", "took", time.Since(t))
	return nil
}
func doRam(cliCtx *cli.Context) error {
	args := cliCtx.Args()
	if len(args) != 1 {
		return fmt.Errorf("expecting .seg file path")
	}
	f := args[0]
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	runtime.ReadMemStats(&m)
	before := m.Alloc
	log.Info("RAM before open", "alloc", common.ByteCount(m.Alloc), "sys", common.ByteCount(m.Sys))
	decompressor, err := compress.NewDecompressor(f)
	if err != nil {
		return err
	}
	defer decompressor.Close()
	runtime.ReadMemStats(&m)
	log.Info("RAM after open", "alloc", common.ByteCount(m.Alloc), "sys", common.ByteCount(m.Sys), "diff", common.ByteCount(m.Alloc-before))
	return nil
}
func doIndicesCommand(cliCtx *cli.Context) error {
	ctx, cancel := common.RootContext()
	defer cancel()

	dirs := datadir.New(cliCtx.String(utils.DataDirFlag.Name))
	rebuild := cliCtx.Bool(SnapshotRebuildFlag.Name)
	from := cliCtx.Uint64(SnapshotFromFlag.Name)

	chainDB := mdbx.NewMDBX(log.New()).Path(dirs.Chaindata).Readonly().MustOpen()
	defer chainDB.Close()

	dir.MustExist(dirs.SnapHistory)

	workers := cmp.Max(1, runtime.GOMAXPROCS(-1)-1)
	if rebuild {
		cfg := ethconfig.NewSnapCfg(true, true, false)
		if err := rebuildIndices("Indexing", ctx, chainDB, cfg, dirs, from, workers); err != nil {
			log.Error("Error", "err", err)
		}
	}
	agg, err := libstate.NewAggregator22(dirs.SnapHistory, ethconfig.HistoryV3AggregationStep)
	if err != nil {
		return err
	}
	err = agg.ReopenFiles()
	if err != nil {
		return err
	}
	agg.SetWorkers(workers)
	err = agg.BuildMissedIndices()
	if err != nil {
		return err
	}
	return nil
}

func doUncompress(cliCtx *cli.Context) error {
	ctx, cancel := common.RootContext()
	defer cancel()
	args := cliCtx.Args()
	if len(args) != 1 {
		return fmt.Errorf("expecting .seg file path")
	}
	f := args[0]

	preloadFileAsync(f)

	decompressor, err := compress.NewDecompressor(f)
	if err != nil {
		return err
	}
	defer decompressor.Close()
	wr := bufio.NewWriterSize(os.Stdout, 512*1024*1024)
	defer wr.Flush()
	var numBuf [binary.MaxVarintLen64]byte
	if err := decompressor.WithReadAhead(func() error {
		g := decompressor.MakeGetter()
		buf := make([]byte, 0, 16*etl.BufIOSize)
		for g.HasNext() {
			buf, _ = g.Next(buf[:0])
			n := binary.PutUvarint(numBuf[:], uint64(len(buf)))
			if _, err := wr.Write(numBuf[:n]); err != nil {
				return err
			}
			if _, err := wr.Write(buf); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}
func doCompress(cliCtx *cli.Context) error {
	ctx, cancel := common.RootContext()
	defer cancel()
	args := cliCtx.Args()
	if len(args) != 1 {
		return fmt.Errorf("expecting .seg file path")
	}
	f := args[0]
	dirs := datadir.New(cliCtx.String(utils.DataDirFlag.Name))
	workers := runtime.GOMAXPROCS(-1) - 1
	if workers < 1 {
		workers = 1
	}
	c, err := compress.NewCompressor(ctx, "", f, dirs.Tmp, compress.MinPatternScore, workers, log.LvlInfo)
	if err != nil {
		return err
	}
	defer c.Close()

	r := bufio.NewReaderSize(os.Stdin, 512*1024*1024)
	buf := make([]byte, 0, 32*1024*1024)
	var l uint64
	for l, err = binary.ReadUvarint(r); err == nil; l, err = binary.ReadUvarint(r) {
		if cap(buf) < int(l) {
			buf = make([]byte, l)
		} else {
			buf = buf[:l]
		}
		if _, err = io.ReadFull(r, buf); err != nil {
			return err
		}
		if err = c.AddWord(buf); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if err := c.Compress(); err != nil {
		return err
	}

	return nil
}
func doRetireCommand(cliCtx *cli.Context) error {
	ctx, cancel := common.RootContext()
	defer cancel()

	dirs := datadir.New(cliCtx.String(utils.DataDirFlag.Name))
	from := cliCtx.Uint64(SnapshotFromFlag.Name)
	to := cliCtx.Uint64(SnapshotToFlag.Name)
	every := cliCtx.Uint64(SnapshotEveryFlag.Name)

	db := mdbx.NewMDBX(log.New()).Label(kv.ChainDB).Path(dirs.Chaindata).MustOpen()
	defer db.Close()

	cfg := ethconfig.NewSnapCfg(true, true, true)
	snapshots := snapshotsync.NewRoSnapshots(cfg, dirs.Snap)
	if err := snapshots.ReopenWithDB(db); err != nil {
		return err
	}

	workers := cmp.Max(1, runtime.GOMAXPROCS(-1)-1)
	br := snapshotsync.NewBlockRetire(workers, dirs.Tmp, snapshots, db, nil, nil)

	log.Info("Params", "from", from, "to", to, "every", every)
	for i := from; i < to; i += every {
		if err := br.RetireBlocks(ctx, i, i+every, log.LvlInfo); err != nil {
			panic(err)
		}
		if err := db.Update(ctx, func(tx kv.RwTx) error {
			if err := rawdb.WriteSnapshots(tx, br.Snapshots().Files()); err != nil {
				return err
			}
			if err := br.PruneAncientBlocks(tx); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return err
		}
	}

	return nil
}

func doSnapshotCommand(cliCtx *cli.Context) error {
	ctx, cancel := common.RootContext()
	defer cancel()

	fromBlock := cliCtx.Uint64(SnapshotFromFlag.Name)
	toBlock := cliCtx.Uint64(SnapshotToFlag.Name)
	segmentSize := cliCtx.Uint64(SnapshotSegmentSizeFlag.Name)
	if segmentSize < 1000 {
		return fmt.Errorf("too small --segment.size %d", segmentSize)
	}
	dirs := datadir.New(cliCtx.String(utils.DataDirFlag.Name))
	dir.MustExist(dirs.Snap)
	dir.MustExist(filepath.Join(dirs.Snap, "db")) // this folder will be checked on existance - to understand that snapshots are ready
	dir.MustExist(dirs.Tmp)

	db := mdbx.NewMDBX(log.New()).Label(kv.ChainDB).Path(dirs.Chaindata).MustOpen()
	defer db.Close()

	if err := snapshotBlocks(ctx, db, fromBlock, toBlock, segmentSize, dirs.Snap, dirs.Tmp); err != nil {
		log.Error("Error", "err", err)
	}

	allSnapshots := snapshotsync.NewRoSnapshots(ethconfig.NewSnapCfg(true, true, true), dirs.Snap)
	if err := allSnapshots.ReopenFolder(); err != nil {
		return err
	}
	if err := db.Update(ctx, func(tx kv.RwTx) error {
		return rawdb.WriteSnapshots(tx, allSnapshots.Files())
	}); err != nil {
		return err
	}
	return nil
}

func rebuildIndices(logPrefix string, ctx context.Context, db kv.RoDB, cfg ethconfig.Snapshot, dirs datadir.Dirs, from uint64, workers int) error {
	chainConfig := fromdb.ChainConfig(db)
	chainID, _ := uint256.FromBig(chainConfig.ChainID)

	allSnapshots := snapshotsync.NewRoSnapshots(cfg, dirs.Snap)
	if err := allSnapshots.ReopenFolder(); err != nil {
		return err
	}

	if err := snapshotsync.BuildMissedIndices(logPrefix, ctx, allSnapshots.Dir(), *chainID, dirs.Tmp, workers, log.LvlInfo); err != nil {
		return err
	}
	return nil
}

func snapshotBlocks(ctx context.Context, db kv.RoDB, fromBlock, toBlock, blocksPerFile uint64, snapDir, tmpDir string) error {
	var last uint64

	if toBlock > 0 {
		last = toBlock
	} else {
		lastChunk := func(tx kv.Tx, blocksPerFile uint64) (uint64, error) {
			c, err := tx.Cursor(kv.BlockBody)
			if err != nil {
				return 0, err
			}
			k, _, err := c.Last()
			if err != nil {
				return 0, err
			}
			last := binary.BigEndian.Uint64(k)
			if last > params.FullImmutabilityThreshold {
				last -= params.FullImmutabilityThreshold
			} else {
				last = 0
			}
			last = last - last%blocksPerFile
			return last, nil
		}

		if err := db.View(context.Background(), func(tx kv.Tx) (err error) {
			last, err = lastChunk(tx, blocksPerFile)
			return err
		}); err != nil {
			return err
		}
	}

	log.Info("Last body number", "last", last)
	workers := cmp.Max(1, runtime.GOMAXPROCS(-1)-1)
	if err := snapshotsync.DumpBlocks(ctx, fromBlock, last, blocksPerFile, tmpDir, snapDir, db, workers, log.LvlInfo); err != nil {
		return fmt.Errorf("DumpBlocks: %w", err)
	}
	return nil
}
