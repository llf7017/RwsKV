/*
 * Copyright 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package badger

import (
	"bytes"
	"context"
	"encoding/binary"
	"expvar"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/badger/v3/options"
	"github.com/dgraph-io/badger/v3/pb"
	"github.com/dgraph-io/badger/v3/skl"
	"github.com/dgraph-io/badger/v3/table"
	"github.com/dgraph-io/badger/v3/y"
	"github.com/dgraph-io/ristretto"
	"github.com/dgraph-io/ristretto/z"
	humanize "github.com/dustin/go-humanize"
	"github.com/pkg/errors"
)

var (
	badgerPrefix = []byte("!badger!")       // Prefix for internal keys used by badger.
	txnKey       = []byte("!badger!txn")    // For indicating end of entries in txn.用于指示txn 中条目的结束。
	bannedNsKey  = []byte("!badger!banned") // For storing the banned namespaces.
)

const (
	maxNumSplits = 128
)

type closers struct {
	updateSize  *z.Closer
	compactors  *z.Closer
	memtable    *z.Closer
	writes      *z.Closer
	valueGC     *z.Closer
	pub         *z.Closer
	cacheHealth *z.Closer
}

type lockedKeys struct {
	sync.RWMutex
	keys map[uint64]struct{}
}

//增加keys中的kv对
func (lk *lockedKeys) add(key uint64) {
	lk.Lock()
	defer lk.Unlock()
	lk.keys[key] = struct{}{}
}

//判断keys中是否有对应key和value
func (lk *lockedKeys) has(key uint64) bool {
	lk.RLock()
	defer lk.RUnlock()
	_, ok := lk.keys[key]
	return ok
}

//返回的是 map 的 所有key。即返回lockedkeys中的key而不是value。
func (lk *lockedKeys) all() []uint64 {
	lk.RLock()
	defer lk.RUnlock()
	keys := make([]uint64, 0, len(lk.keys))
	for key := range lk.keys {
		keys = append(keys, key)
	}
	return keys
}

// DB provides the various functions required to interact with Badger.
// DB is thread-safe.
type DB struct {
	lock sync.RWMutex // Guards list of inmemory tables, not individual reads and writes.对内存中的列表加锁，而不是单独的读写。

	dirLockGuard *directoryLockGuard
	// nil if Dir and ValueDir are the same
	valueDirGuard *directoryLockGuard

	closers closers

	mt  *memTable   // Our latest (actively written) in-memory table
	imm []*memTable // Add here only AFTER pushing to flushChan.

	// Initialized via openMemTables.
	nextMemFid int

	opt       Options
	manifest  *manifestFile
	lc        *levelsController
	vlog      valueLog
	writeCh   chan *request
	sklCh     chan *handoverRequest
	flushChan chan flushTask // For flushing memtables.
	closeOnce sync.Once      // For closing DB only once.

	blockWrites int32
	isClosed    uint32

	orc              *oracle
	bannedNamespaces *lockedKeys
	threshold        *vlogThreshold

	pub        *publisher
	registry   *KeyRegistry
	blockCache *ristretto.Cache
	indexCache *ristretto.Cache
	allocPool  *z.AllocatorPool
}

const (
	kvWriteChCapacity = 1000
)

//设置option参数
func checkAndSetOptions(opt *Options) error {
	// It's okay to have zero compactors which will disable all compactions but
	// we cannot have just one compactor otherwise we will end up with all data
	// on level 2.
	if opt.NumCompactors == 1 {
		return errors.New("Cannot have 1 compactor. Need at least 2")
	}

	if opt.InMemory && (opt.Dir != "" || opt.ValueDir != "") {
		return errors.New("Cannot use badger in Disk-less mode with Dir or ValueDir set")
	}
	opt.maxBatchSize = (15 * opt.MemTableSize) / 100
	opt.maxBatchCount = opt.maxBatchSize / int64(skl.MaxNodeSize)

	// This is the maximum value, vlogThreshold can have if dynamic thresholding is enabled.
	opt.maxValueThreshold = math.Min(maxValueThreshold, float64(opt.maxBatchSize))
	if opt.VLogPercentile < 0.0 || opt.VLogPercentile > 1.0 {
		return errors.New("vlogPercentile must be within range of 0.0-1.0")
	}

	// We are limiting opt.ValueThreshold to maxValueThreshold for now.
	if opt.ValueThreshold > maxValueThreshold {
		return errors.Errorf("Invalid ValueThreshold, must be less or equal to %d",
			maxValueThreshold)
	}

	// If ValueThreshold is greater than opt.maxBatchSize, we won't be able to push any data using
	// the transaction APIs. Transaction batches entries into batches of size opt.maxBatchSize.
	if opt.ValueThreshold > opt.maxBatchSize {
		return errors.Errorf("Valuethreshold %d greater than max batch size of %d. Either "+
			"reduce opt.ValueThreshold or increase opt.MaxTableSize.",
			opt.ValueThreshold, opt.maxBatchSize)
	}
	// ValueLogFileSize should be stricly LESS than 2<<30 otherwise we will
	// overflow the uint32 when we mmap it in OpenMemtable.
	if !(opt.ValueLogFileSize < 2<<30 && opt.ValueLogFileSize >= 1<<20) {
		return ErrValueLogSize
	}

	if opt.ReadOnly {
		// Do not perform compaction in read only mode.
		opt.CompactL0OnClose = false
	}

	needCache := (opt.Compression != options.None) || (len(opt.EncryptionKey) > 0)
	if needCache && opt.BlockCacheSize == 0 {
		panic("BlockCacheSize should be set since compression/encryption are enabled")
	}
	return nil
}

// Open returns a new DB object.
func Open(opt Options) (*DB, error) {
	//1. 构造配置参数对象
	if err := checkAndSetOptions(&opt); err != nil {
		return nil, err
	}
	var dirLockGuard, valueDirLockGuard *directoryLockGuard

	// Create directories and acquire lock on it only if badger is not running in InMemory mode.
	// We don't have any directories/files in InMemory mode so we don't need to acquire
	// any locks on them.
	//只有当 badger 没有在 InMemory 模式下运行时，才创建目录并获取对它的锁定。
	// 我们在 InMemory 模式下没有任何目录/文件，所以我们不需要获取
	// 它们上的任何锁。
	//2. 打开或创建工作目录并上锁
	if !opt.InMemory {
		if err := createDirs(opt); err != nil {
			return nil, err
		}
		var err error
		if !opt.BypassLockGuard {
			dirLockGuard, err = acquireDirectoryLock(opt.Dir, lockFile, opt.ReadOnly)
			if err != nil {
				return nil, err
			}
			defer func() {
				if dirLockGuard != nil {
					_ = dirLockGuard.release()
				}
			}()
			absDir, err := filepath.Abs(opt.Dir)
			if err != nil {
				return nil, err
			}
			absValueDir, err := filepath.Abs(opt.ValueDir)
			if err != nil {
				return nil, err
			}
			if absValueDir != absDir {
				valueDirLockGuard, err = acquireDirectoryLock(opt.ValueDir, lockFile, opt.ReadOnly)
				if err != nil {
					return nil, err
				}
				defer func() {
					if valueDirLockGuard != nil {
						_ = valueDirLockGuard.release()
					}
				}()
			}
		}
	}
	//3. 打开或创建ManifestFile文件
	manifestFile, manifest, err := openOrCreateManifestFile(opt)
	if err != nil {
		return nil, err
	}
	defer func() {
		if manifestFile != nil {
			_ = manifestFile.close()
		}
	}()
	//4. 创建DB对象
	db := &DB{
		imm:              make([]*memTable, 0, opt.NumMemtables), //  创建内存表列表,用于预写日志
		flushChan:        make(chan flushTask, opt.NumMemtables), //  创建通知刷新任务的channel
		writeCh:          make(chan *request, kvWriteChCapacity), //  3. 创建写入任务的channel
		sklCh:            make(chan *handoverRequest),
		opt:              opt,               //  4. 配置对象
		manifest:         manifestFile,      //  5. manifest文件对象
		dirLockGuard:     dirLockGuard,      //  6. 目录锁
		valueDirGuard:    valueDirLockGuard, //  7. value目录锁
		orc:              newOracle(opt),    //  8. 创建oracle对象，用于并发事务的控制
		pub:              newPublisher(),
		allocPool:        z.NewAllocatorPool(8),
		bannedNamespaces: &lockedKeys{keys: make(map[uint64]struct{})},
		threshold:        initVlogThreshold(&opt),
	}
	// Cleanup all the goroutines started by badger in case of an error.
	defer func() {
		if err != nil {
			opt.Errorf("Received err: %v. Cleaning up...", err)
			db.cleanup()
			db = nil
		}
	}()
	//5. 创建一个块缓存
	if opt.BlockCacheSize > 0 {
		numInCache := opt.BlockCacheSize / int64(opt.BlockSize)
		if numInCache == 0 {
			// Make the value of this variable at least one since the cache requires
			// the number of counters to be greater than zero.
			numInCache = 1
		}

		config := ristretto.Config{
			NumCounters: numInCache * 8,
			MaxCost:     opt.BlockCacheSize,
			BufferItems: 64,
			Metrics:     true,
			OnExit:      table.BlockEvictHandler,
		}
		db.blockCache, err = ristretto.NewCache(&config)
		if err != nil {
			return nil, y.Wrap(err, "failed to create data cache")
		}
	}
	//6. 创建一个索引缓存
	if opt.IndexCacheSize > 0 {
		// Index size is around 5% of the table size.
		indexSz := int64(float64(opt.MemTableSize) * 0.05)
		numInCache := opt.IndexCacheSize / indexSz
		if numInCache == 0 {
			// Make the value of this variable at least one since the cache requires
			// the number of counters to be greater than zero.
			numInCache = 1
		}

		config := ristretto.Config{
			NumCounters: numInCache * 8,
			MaxCost:     opt.IndexCacheSize,
			BufferItems: 64,
			Metrics:     true,
		}
		db.indexCache, err = ristretto.NewCache(&config)
		if err != nil {
			return nil, y.Wrap(err, "failed to create bf cache")
		}
	}

	db.closers.cacheHealth = z.NewCloser(1)
	go db.monitorCache(db.closers.cacheHealth)

	if db.opt.InMemory {
		db.opt.SyncWrites = false
		// If badger is running in memory mode, push everything into the LSM Tree.
		db.opt.ValueThreshold = math.MaxInt32
	}
	//	7. 开启key注册器
	krOpt := KeyRegistryOptions{
		ReadOnly:                      opt.ReadOnly,
		Dir:                           opt.Dir,
		EncryptionKey:                 opt.EncryptionKey,
		EncryptionKeyRotationDuration: opt.EncryptionKeyRotationDuration,
		InMemory:                      opt.InMemory,
	}

	if db.registry, err = OpenKeyRegistry(krOpt); err != nil {
		return db, err
	}
	db.calculateSize()
	db.closers.updateSize = z.NewCloser(1)
	go db.updateSize(db.closers.updateSize)
	//8. 打开memtable 并全部追加到 imm 的列表中
	if err := db.openMemTables(db.opt); err != nil {
		return nil, y.Wrapf(err, "while opening memtables")
	}

	if !db.opt.ReadOnly {
		if db.mt, err = db.newMemTable(); err != nil {
			return nil, y.Wrapf(err, "cannot create memtable")
		}
	}

	// newLevelsController potentially loads files in directory.
	if db.lc, err = newLevelsController(db, &manifest); err != nil {
		return db, err
	}

	// Initialize vlog struct.
	db.vlog.init(db)

	if !opt.ReadOnly {
		db.closers.compactors = z.NewCloser(1)
		db.lc.startCompact(db.closers.compactors)

		db.closers.memtable = z.NewCloser(1)
		go func() {
			_ = db.flushMemtable(db.closers.memtable) // Need levels controller to be up.
		}()
		// Flush them to disk asap.
		for _, mt := range db.imm {
			db.flushChan <- flushTask{mt: mt}
		}
	}
	// We do increment nextTxnTs below. So, no need to do it here.
	db.orc.nextTxnTs = db.MaxVersion()
	db.opt.Infof("Set nextTxnTs to %d", db.orc.nextTxnTs)

	if err = db.vlog.open(db); err != nil {
		return db, y.Wrapf(err, "During db.vlog.open")
	}

	// Let's advance nextTxnTs to one more than whatever we observed via
	// replaying the logs.
	db.orc.txnMark.Done(db.orc.nextTxnTs)
	// In normal mode, we must update readMark so older versions of keys can be removed during
	// compaction when run in offline mode via the flatten tool.
	db.orc.readMark.Done(db.orc.nextTxnTs)
	db.orc.incrementNextTs()

	go db.threshold.listenForValueThresholdUpdate()

	if err := db.initBannedNamespaces(); err != nil {
		return db, errors.Wrapf(err, "While setting banned keys")
	}

	db.closers.writes = z.NewCloser(2)
	go db.doWrites(db.closers.writes)
	go db.handleHandovers(db.closers.writes)

	if !db.opt.InMemory {
		db.closers.valueGC = z.NewCloser(1)
		go db.vlog.waitOnGC(db.closers.valueGC)
	}

	db.closers.pub = z.NewCloser(1)
	go db.pub.listenForUpdates(db.closers.pub)

	valueDirLockGuard = nil
	dirLockGuard = nil
	manifestFile = nil
	return db, nil
}

// initBannedNamespaces retrieves the banned namepsaces from the DB and updates in-memory structure.
func (db *DB) initBannedNamespaces() error {
	if db.opt.NamespaceOffset < 0 {
		return nil
	}
	return db.View(func(txn *Txn) error {
		iopts := DefaultIteratorOptions
		iopts.Prefix = bannedNsKey
		iopts.PrefetchValues = false
		iopts.InternalAccess = true
		itr := txn.NewIterator(iopts)
		defer itr.Close()
		for itr.Rewind(); itr.Valid(); itr.Next() {
			key := y.BytesToU64(itr.Item().Key()[len(bannedNsKey):])
			db.bannedNamespaces.add(key)
		}
		return nil
	})
}

func (db *DB) MaxVersion() uint64 {
	var maxVersion uint64
	update := func(a uint64) {
		if a > maxVersion {
			maxVersion = a
		}
	}
	db.lock.Lock()
	// In read only mode, we do not create new mem table.
	if !db.opt.ReadOnly {
		update(db.mt.maxVersion)
	}
	for _, mt := range db.imm {
		update(mt.maxVersion)
	}
	db.lock.Unlock()
	for _, ti := range db.Tables() {
		update(ti.MaxVersion)
	}
	return maxVersion
}

func (db *DB) monitorCache(c *z.Closer) {
	defer c.Done()
	count := 0
	analyze := func(name string, metrics *ristretto.Metrics) {
		// If the mean life expectancy is less than 10 seconds, the cache
		// might be too small.
		le := metrics.LifeExpectancySeconds()
		if le == nil {
			return
		}
		lifeTooShort := le.Count > 0 && float64(le.Sum)/float64(le.Count) < 10
		hitRatioTooLow := metrics.Ratio() > 0 && metrics.Ratio() < 0.4
		if lifeTooShort && hitRatioTooLow {
			db.opt.Warningf("%s might be too small. Metrics: %s\n", name, metrics)
			db.opt.Warningf("Cache life expectancy (in seconds): %+v\n", le)

		} else if le.Count > 1000 && count%5 == 0 {
			db.opt.Infof("%s metrics: %s\n", name, metrics)
		}
	}

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-c.HasBeenClosed():
			return
		case <-ticker.C:
		}

		analyze("Block cache", db.BlockCacheMetrics())
		analyze("Index cache", db.IndexCacheMetrics())
		count++
	}
}

// cleanup stops all the goroutines started by badger. This is used in open to
// cleanup goroutines in case of an error.
func (db *DB) cleanup() {
	db.stopMemoryFlush()
	db.stopCompactions()

	db.blockCache.Close()
	db.indexCache.Close()
	if db.closers.updateSize != nil {
		db.closers.updateSize.Signal()
	}
	if db.closers.valueGC != nil {
		db.closers.valueGC.Signal()
	}
	if db.closers.writes != nil {
		db.closers.writes.Signal()
	}
	if db.closers.pub != nil {
		db.closers.pub.Signal()
	}

	db.orc.Stop()

	// Do not use vlog.Close() here. vlog.Close truncates the files. We don't
	// want to truncate files unless the user has specified the truncate flag.
}

// BlockCacheMetrics returns the metrics for the underlying block cache.
func (db *DB) BlockCacheMetrics() *ristretto.Metrics {
	if db.blockCache != nil {
		return db.blockCache.Metrics
	}
	return nil
}

// IndexCacheMetrics returns the metrics for the underlying index cache.
func (db *DB) IndexCacheMetrics() *ristretto.Metrics {
	if db.indexCache != nil {
		return db.indexCache.Metrics
	}
	return nil
}

// Close closes a DB. It's crucial to call it to ensure all the pending updates make their way to
// disk. Calling DB.Close() multiple times would still only close the DB once.
func (db *DB) Close() error {
	var err error
	db.closeOnce.Do(func() {
		err = db.close()
	})
	return err
}

// IsClosed denotes if the badger DB is closed or not. A DB instance should not
// be used after closing it.
func (db *DB) IsClosed() bool {
	return atomic.LoadUint32(&db.isClosed) == 1
}

func (db *DB) close() (err error) {
	defer db.allocPool.Release()

	db.opt.Debugf("Closing database")
	db.opt.Infof("Lifetime L0 stalled for: %s\n", time.Duration(atomic.LoadInt64(&db.lc.l0stallsMs)))

	atomic.StoreInt32(&db.blockWrites, 1)

	if !db.opt.InMemory {
		// Stop value GC first.
		db.closers.valueGC.SignalAndWait()
	}

	// Stop writes next.
	db.closers.writes.SignalAndWait()

	// Don't accept any more write.
	close(db.writeCh)
	close(db.sklCh)

	db.closers.pub.SignalAndWait()
	db.closers.cacheHealth.Signal()

	// Make sure that block writer is done pushing stuff into memtable!
	// Otherwise, you will have a race condition: we are trying to flush memtables
	// and remove them completely, while the block / memtable writer is still
	// trying to push stuff into the memtable. This will also resolve the value
	// offset problem: as we push into memtable, we update value offsets there.
	if db.mt != nil {
		if db.mt.sl.Empty() {
			// Remove the memtable if empty.
			db.mt.DecrRef()
		} else {
			db.opt.Debugf("Flushing memtable")
			for {
				pushedFlushTask := func() bool {
					db.lock.Lock()
					defer db.lock.Unlock()
					y.AssertTrue(db.mt != nil)
					select {
					case db.flushChan <- flushTask{mt: db.mt}:
						db.imm = append(db.imm, db.mt) // Flusher will attempt to remove this from s.imm.
						db.mt = nil                    // Will segfault if we try writing!
						db.opt.Debugf("pushed to flush chan\n")
						return true
					default:
						// If we fail to push, we need to unlock and wait for a short while.
						// The flushing operation needs to update s.imm. Otherwise, we have a
						// deadlock.
						// TODO: Think about how to do this more cleanly, maybe without any locks.
					}
					return false
				}()
				if pushedFlushTask {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}
	db.stopMemoryFlush()
	db.stopCompactions()

	// Force Compact L0
	// We don't need to care about cstatus since no parallel compaction is running.
	if db.opt.CompactL0OnClose {
		err := db.lc.doCompact(173, compactionPriority{level: 0, score: 1.73})
		switch err {
		case errFillTables:
			// This error only means that there might be enough tables to do a compaction. So, we
			// should not report it to the end user to avoid confusing them.
		case nil:
			db.opt.Debugf("Force compaction on level 0 done")
		default:
			db.opt.Warningf("While forcing compaction on level 0: %v", err)
		}
	}

	// Now close the value log.
	if vlogErr := db.vlog.Close(); vlogErr != nil {
		err = y.Wrap(vlogErr, "DB.Close")
	}

	db.opt.Infof(db.LevelsToString())
	if lcErr := db.lc.close(); err == nil {
		err = y.Wrap(lcErr, "DB.Close")
	}
	db.opt.Debugf("Waiting for closer")
	db.closers.updateSize.SignalAndWait()
	db.orc.Stop()
	db.blockCache.Close()
	db.indexCache.Close()

	atomic.StoreUint32(&db.isClosed, 1)
	db.threshold.close()

	if db.opt.InMemory {
		return
	}

	if db.dirLockGuard != nil {
		if guardErr := db.dirLockGuard.release(); err == nil {
			err = y.Wrap(guardErr, "DB.Close")
		}
	}
	if db.valueDirGuard != nil {
		if guardErr := db.valueDirGuard.release(); err == nil {
			err = y.Wrap(guardErr, "DB.Close")
		}
	}
	if manifestErr := db.manifest.close(); err == nil {
		err = y.Wrap(manifestErr, "DB.Close")
	}
	if registryErr := db.registry.Close(); err == nil {
		err = y.Wrap(registryErr, "DB.Close")
	}

	// Fsync directories to ensure that lock file, and any other removed files whose directory
	// we haven't specifically fsynced, are guaranteed to have their directory entry removal
	// persisted to disk.
	if syncErr := db.syncDir(db.opt.Dir); err == nil {
		err = y.Wrap(syncErr, "DB.Close")
	}
	if syncErr := db.syncDir(db.opt.ValueDir); err == nil {
		err = y.Wrap(syncErr, "DB.Close")
	}

	return err
}

// VerifyChecksum verifies checksum for all tables on all levels.
// This method can be used to verify checksum, if opt.ChecksumVerificationMode is NoVerification.
func (db *DB) VerifyChecksum() error {
	return db.lc.verifyChecksum()
}

const (
	lockFile = "LOCK"
)

// Sync syncs database content to disk. This function provides
// more control to user to sync data whenever required.
func (db *DB) Sync() error {
	return db.vlog.sync()
}

// getMemtables returns the current memtables and get references.
//获取内存表列表
func (db *DB) getMemTables() ([]*memTable, func()) {
	db.lock.RLock()
	defer db.lock.RUnlock()

	var tables []*memTable

	// Mutable memtable does not exist in read-only mode.
	if !db.opt.ReadOnly {
		// Get mutable memtable.
		tables = append(tables, db.mt)
		db.mt.IncrRef()
	}

	// Get immutable memtables.
	last := len(db.imm) - 1
	for i := range db.imm {
		tables = append(tables, db.imm[last-i])
		db.imm[last-i].IncrRef()
	}
	return tables, func() {
		for _, tbl := range tables {
			tbl.DecrRef()
		}
	}
}

// get returns the value in memtable or disk for given key.
// Note that value will include meta byte.
//
// IMPORTANT: We should never write an entry with an older timestamp for the same key, We need to
// maintain this invariant to search for the latest value of a key, or else we need to search in all
// tables and find the max version among them.  To maintain this invariant, we also need to ensure
// that all versions of a key are always present in the same table from level 1, because compaction
// can push any table down.
//
// Update(23/09/2020) - We have dropped the move key implementation. Earlier we
// were inserting move keys to fix the invalid value pointers but we no longer
// do that. For every get("fooX") call where X is the version, we will search
// for "fooX" in all the levels of the LSM tree. This is expensive but it
// removes the overhead of handling move keys completely.

// get 返回给定键的内存表或磁盘中的值。
// 请注意，该值将包括元字节。
//
// 重要提示：我们永远不应该为同一个键写一个带有较旧时间戳的条目，我们需要维护这个不变量来搜索一个键的最新值，
// 否则我们需要在所有表中搜索并找到其中的最大版本 他们。 为了保持这种不变性，我们还需要确保从级别 1 开始，
// 一个键的所有版本始终存在于同一个表中，因为压缩可以将任何表下推。
//
// 更新(23/09/2020) - 我们已经删除了移动键的实现。 早些时候我们插入了移动键来修复无效的值指针，但我们不再这样做了。
// 对于每个 X 是版本的 get("fooX") 调用，我们将在 LSM 树的所有级别中搜索“fooX”。 这很昂贵，但它完全消除了处理移动键的开销。
func (db *DB) get(key []byte) (y.ValueStruct, error) {
	if db.IsClosed() {
		return y.ValueStruct{}, ErrDBClosed
	}
	//  获取内存表列表
	tables, decr := db.getMemTables() // Lock should be released.
	defer decr()

	var maxVs y.ValueStruct
	//获取时间戳
	version := y.ParseTs(key)

	y.NumGetsAdd(db.opt.MetricsEnabled, 1)
	// 遍历内存表从跳表中get key信息，并且其最大版本号小于等于查询版本时直接返回
	for i := 0; i < len(tables); i++ {
		vs := tables[i].sl.Get(key)
		y.NumMemtableGetsAdd(db.opt.MetricsEnabled, 1)
		if vs.Meta == 0 && vs.Value == nil {
			continue
		}
		// Found the required version of the key, return immediately.
		if vs.Version == version {
			return vs, nil
		}
		if maxVs.Version < vs.Version {
			maxVs = vs
		}
	}
	//8. 如果没有则从level中查询
	return db.lc.get(key, maxVs, 0)
}

var requestPool = sync.Pool{
	New: func() interface{} {
		return new(request)
	},
}

func (db *DB) writeToLSM(b *request) error {
	db.lock.RLock()
	defer db.lock.RUnlock()
	for i, entry := range b.Entries {
		var err error
		if db.opt.managedTxns || entry.skipVlogAndSetThreshold(db.valueThreshold()) {
			// Will include deletion / tombstone case.
			// 没有超过Value的阈值，则直接将值存储到mt中
			err = db.mt.Put(entry.Key,
				y.ValueStruct{
					Value: entry.Value,
					// Ensure value pointer flag is removed. Otherwise, the value will fail
					// to be retrieved during iterator prefetch. `bitValuePointer` is only
					// known to be set in write to LSM when the entry is loaded from a backup
					// with lower ValueThreshold and its value was stored in the value log.
					Meta:      entry.meta &^ bitValuePointer,
					UserMeta:  entry.UserMeta,
					ExpiresAt: entry.ExpiresAt,
				})
		} else {
			// 超过则将值的指针存储到mt中 指针指向vlog中的位置
			// Write pointer to Memtable.
			err = db.mt.Put(entry.Key,
				y.ValueStruct{
					Value:     b.Ptrs[i].Encode(),
					Meta:      entry.meta | bitValuePointer,
					UserMeta:  entry.UserMeta,
					ExpiresAt: entry.ExpiresAt,
				})
		}
		if err != nil {
			return y.Wrapf(err, "while writing to memTable")
		}
	}
	if db.opt.SyncWrites {
		return db.mt.SyncWAL()
	}
	return nil
}

// writeRequests is called serially by only one goroutine.
// 处理写请求
func (db *DB) writeRequests(reqs []*request) error {
	if len(reqs) == 0 {
		return nil
	}

	done := func(err error) {
		for _, r := range reqs {
			r.Err = err
			r.Wg.Done()
		}
	}
	db.opt.Debugf("writeRequests called. Writing to value log")
	// 整个写入vlog文件
	err := db.vlog.write(reqs)
	if err != nil {
		done(err)
		return err
	}

	db.opt.Debugf("Sending updates to subscribers")
	db.pub.sendUpdates(reqs)
	db.opt.Debugf("Writing to memtable")
	var count int
	//  2. 分req写入LSM中
	for _, b := range reqs {
		if len(b.Entries) == 0 {
			continue
		}
		count += len(b.Entries)
		var i uint64
		var err error
		for err = db.ensureRoomForWrite(); err == errNoRoom; err = db.ensureRoomForWrite() {
			i++
			if i%100 == 0 {
				db.opt.Debugf("Making room for writes")
			}
			// We need to poll a bit because both hasRoomForWrite and the flusher need access to s.imm.
			// When flushChan is full and you are blocked there, and the flusher is trying to update s.imm,
			// you will get a deadlock.
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			done(err)
			return y.Wrap(err, "writeRequests")
		}
		if err := db.writeToLSM(b); err != nil {
			done(err)
			return y.Wrap(err, "writeRequests")
		}
	}
	done(nil)
	db.opt.Debugf("%d entries written", count)
	return nil
}

//将需要写入的kv对写入数据库中
func (db *DB) sendToWriteCh(entries []*Entry) (*request, error) {
	if atomic.LoadInt32(&db.blockWrites) == 1 {
		return nil, ErrBlockedWrites
	}
	var count, size int64
	for _, e := range entries {
		size += e.estimateSizeAndSetThreshold(db.valueThreshold())
		count++
	}
	if count >= db.opt.maxBatchCount || size >= db.opt.maxBatchSize {
		return nil, ErrTxnTooBig
	}

	// We can only service one request because we need each txn to be stored in a contiguous section.
	// Txns should not interleave among other txns or rewrites.
	// 我们只能服务一个请求，因为我们需要将每个 txn 存储在一个连续的部分中。Txns 不应与其他 txns 交错或重写。
	//为sync.Pool，可以反复使用，减少内存申请的开销
	req := requestPool.Get().(*request)
	//初始化
	req.reset()
	req.Entries = entries
	req.Wg.Add(1)
	req.IncrRef()     // for db write
	db.writeCh <- req // Handled in doWrites.
	y.NumPutsAdd(db.opt.MetricsEnabled, int64(len(entries)))

	return req, nil
}

func (db *DB) handleHandovers(lc *z.Closer) {
	defer lc.Done()
	for {
		select {
		case r := <-db.sklCh:
			r.err = db.handoverSkiplist(r)
			r.wg.Done()
		case <-lc.HasBeenClosed():
			return
		}
	}
}

//将txn中想要写入的数据实例化到数据库中
func (db *DB) doWrites(lc *z.Closer) {
	defer lc.Done()
	pendingCh := make(chan struct{}, 1)

	writeRequests := func(reqs []*request) {
		if err := db.writeRequests(reqs); err != nil {
			db.opt.Errorf("writeRequests: %v", err)
		}
		<-pendingCh
	}

	// This variable tracks the number of pending writes.
	//此变量跟踪挂起的写入次数。
	reqLen := new(expvar.Int)
	y.PendingWritesSet(db.opt.MetricsEnabled, db.opt.Dir, reqLen)

	reqs := make([]*request, 0, 10)
	for {
		// Select writeCh 批量拼装 reqs
		var r *request
		select {
		case r = <-db.writeCh:
		case <-lc.HasBeenClosed():
			goto closedCase
		}

		for {
			reqs = append(reqs, r)
			reqLen.Set(int64(len(reqs)))

			if len(reqs) >= 3*kvWriteChCapacity {
				pendingCh <- struct{}{} // blocking.
				goto writeCase
			}

			select {
			// Either push to pending, or continue to pick from writeCh.
			case r = <-db.writeCh:
			case pendingCh <- struct{}{}:
				goto writeCase
			case <-lc.HasBeenClosed():
				goto closedCase
			}
		}

	closedCase:
		// All the pending request are drained.
		// Don't close the writeCh, because it has be used in several places.
		// 所有挂起的请求都被清空。
		// 不要关闭 writeCh，因为它已经在多个地方使用过。
		for {
			select {
			case r = <-db.writeCh:
				reqs = append(reqs, r)
			default:
				// 当该doWrite关闭后，先读取所有writeCh中的请求后最后处理一次写入请求

				pendingCh <- struct{}{} // Push to pending before doing a write.
				writeRequests(reqs)
				return
			}
		}

	writeCase:
		//	4. 开启一个协程处理此次写请求
		go writeRequests(reqs)
		reqs = make([]*request, 0, 10)
		reqLen.Set(0)
	}
}

// batchSet applies a list of badger.Entry. If a request level error occurs it
// will be returned.
//   Check(kv.BatchSet(entries))
func (db *DB) batchSet(entries []*Entry) error {
	req, err := db.sendToWriteCh(entries)
	if err != nil {
		return err
	}

	return req.Wait()
}

// batchSetAsync is the asynchronous version of batchSet. It accepts a callback
// function which is called when all the sets are complete. If a request level
// error occurs, it will be passed back via the callback.
//   err := kv.BatchSetAsync(entries, func(err error)) {
//      Check(err)
//   }
func (db *DB) batchSetAsync(entries []*Entry, f func(error)) error {
	req, err := db.sendToWriteCh(entries)
	if err != nil {
		return err
	}
	go func() {
		err := req.Wait()
		// Write is complete. Let's call the callback function now.
		f(err)
	}()
	return nil
}

var errNoRoom = errors.New("No room for write")

// ensureRoomForWrite is always called serially.
//// ensureRoomForWrite总是被连续地调用。
func (db *DB) ensureRoomForWrite() error {
	var err error
	db.lock.Lock()
	defer db.lock.Unlock()

	y.AssertTrue(db.mt != nil) // A nil mt indicates that DB is being closed.
	if !db.mt.isFull() {
		return nil
	}

	select {
	case db.flushChan <- flushTask{mt: db.mt}:
		db.opt.Debugf("Flushing memtable, mt.size=%d size of flushChan: %d\n",
			db.mt.sl.MemSize(), len(db.flushChan))
		// We manage to push this task. Let's modify imm.
		db.imm = append(db.imm, db.mt)
		db.mt, err = db.newMemTable()
		if err != nil {
			return y.Wrapf(err, "cannot create new mem table")
		}
		// New memtable is empty. We certainly have room.
		return nil
	default:
		// We need to do this to unlock and allow the flusher to modify imm.
		return errNoRoom
	}
}

func (db *DB) handoverSkiplist(r *handoverRequest) error {
	skl, callback := r.skl, r.callback
	// If we have some data in db.mt, we should push that first, so the ordering of writes is
	// maintained.
	if !db.mt.sl.Empty() {
		sz := db.mt.sl.MemSize()
		db.opt.Infof("Handover found %d B data in current memtable. Pushing to flushChan.", sz)
		var err error
		select {
		case db.flushChan <- flushTask{mt: db.mt}:
			db.imm = append(db.imm, db.mt)
			db.mt, err = db.newMemTable()
			if err != nil {
				return y.Wrapf(err, "cannot push current memtable")
			}
		default:
			return errNoRoom
		}
	}

	mt := &memTable{sl: skl}

	// Iterate over the skiplist and send the entries to the publisher.
	it := skl.NewIterator()

	var entries []*Entry
	for it.SeekToFirst(); it.Valid(); it.Next() {
		v := it.Value()
		e := &Entry{
			Key:       it.Key(),
			Value:     v.Value,
			ExpiresAt: v.ExpiresAt,
			UserMeta:  v.UserMeta,
		}
		entries = append(entries, e)
	}
	req := &request{
		Entries: entries,
	}
	reqs := []*request{req}
	db.pub.sendUpdates(reqs)

	select {
	case db.flushChan <- flushTask{mt: mt, cb: callback}:
		db.imm = append(db.imm, mt)
		return nil
	default:
		return errNoRoom
	}
}

func (db *DB) HandoverSkiplist(skl *skl.Skiplist, callback func()) error {
	if !db.opt.managedTxns {
		panic("Handover Skiplist is only available in managed mode.")
	}

	if atomic.LoadInt32(&db.blockWrites) == 1 {
		return ErrBlockedWrites
	}

	db.lock.Lock()
	defer db.lock.Unlock()

	req := &handoverRequest{skl: skl, callback: callback}
	req.wg.Add(1)
	db.sklCh <- req
	req.wg.Wait()
	return req.err
}

func arenaSize(opt Options) int64 {
	return opt.MemTableSize + opt.maxBatchSize + opt.maxBatchCount*int64(skl.MaxNodeSize)
}

func (db *DB) NewSkiplist() *skl.Skiplist {
	return skl.NewSkiplist(arenaSize(db.opt))
}

// buildL0Table builds a new table from the memtable.
func buildL0Table(ft flushTask, bopts table.Options) *table.Builder {
	var iter y.Iterator
	if ft.itr != nil {
		iter = ft.itr
	} else {
		iter = ft.mt.sl.NewUniIterator(false)
	}
	defer iter.Close()

	b := table.NewTableBuilder(bopts)
	for iter.Rewind(); iter.Valid(); iter.Next() {
		if len(ft.dropPrefixes) > 0 && hasAnyPrefixes(iter.Key(), ft.dropPrefixes) {
			continue
		}
		vs := iter.Value()
		var vp valuePointer
		if vs.Meta&bitValuePointer > 0 {
			vp.Decode(vs.Value)
		}
		b.Add(iter.Key(), iter.Value(), vp.Len)
	}
	return b
}

type flushTask struct {
	mt           *memTable
	cb           func()
	itr          y.Iterator
	dropPrefixes [][]byte
}

// handleFlushTask must be run serially.
//
func (db *DB) handleFlushTask(ft flushTask) error {
	// ft.mt could be nil with ft.itr being the valid field.
	bopts := buildTableOptions(db)
	builder := buildL0Table(ft, bopts)
	defer builder.Close()

	// buildL0Table can return nil if the none of the items in the skiplist are
	// added to the builder. This can happen when drop prefix is set and all
	// the items are skipped.
	if builder.Empty() {
		builder.Finish()
		return nil
	}

	fileID := db.lc.reserveFileID()
	var tbl *table.Table
	var err error
	if db.opt.InMemory {
		data := builder.Finish()
		tbl, err = table.OpenInMemoryTable(data, fileID, &bopts)
	} else {
		tbl, err = table.CreateTable(table.NewFilename(fileID, db.opt.Dir), builder)
	}
	if err != nil {
		return y.Wrap(err, "error while creating table")
	}
	// We own a ref on tbl.
	err = db.lc.addLevel0Table(tbl) // This will incrRef
	_ = tbl.DecrRef()               // Releases our ref.
	return err
}

// flushMemtable must keep running until we send it an empty flushTask. If there
// are errors during handling the flush task, we'll retry indefinitely.
func (db *DB) flushMemtable(lc *z.Closer) error {
	defer lc.Done()

	var sz int64
	var itrs []y.Iterator
	var mts []*memTable
	var cbs []func()
	slurp := func() {
		for {
			select {
			case more := <-db.flushChan:
				if more.mt == nil {
					return
				}
				sl := more.mt.sl
				itrs = append(itrs, sl.NewUniIterator(false))
				mts = append(mts, more.mt)
				cbs = append(cbs, more.cb)

				sz += sl.MemSize()
				if sz > db.opt.MemTableSize {
					return
				}
			default:
				return
			}
		}
	}

	for ft := range db.flushChan {
		if ft.mt == nil {
			// We close db.flushChan now, instead of sending a nil ft.mt.
			continue
		}
		sz = ft.mt.sl.MemSize()
		// Reset of itrs, mts etc. is being done below.
		y.AssertTrue(len(itrs) == 0 && len(mts) == 0 && len(cbs) == 0)
		itrs = append(itrs, ft.mt.sl.NewUniIterator(false))
		mts = append(mts, ft.mt)
		cbs = append(cbs, ft.cb)

		// Pick more memtables, so we can really fill up the L0 table.
		slurp()

		// db.opt.Infof("Picked %d memtables. Size: %d\n", len(itrs), sz)
		ft.mt = nil
		ft.itr = table.NewMergeIterator(itrs, false)
		ft.cb = nil

		for {
			err := db.handleFlushTask(ft)
			if err == nil {
				// Update s.imm. Need a lock.
				db.lock.Lock()
				// This is a single-threaded operation. ft.mt corresponds to the head of
				// db.imm list. Once we flush it, we advance db.imm. The next ft.mt
				// which would arrive here would match db.imm[0], because we acquire a
				// lock over DB when pushing to flushChan.
				// TODO: This logic is dirty AF. Any change and this could easily break.
				for _, mt := range mts {
					y.AssertTrue(mt == db.imm[0])
					db.imm = db.imm[1:]
					mt.DecrRef() // Return memory.
				}
				db.lock.Unlock()

				for _, cb := range cbs {
					if cb != nil {
						cb()
					}
				}
				break
			}
			// Encountered error. Retry indefinitely.
			db.opt.Errorf("Failure while flushing memtable to disk: %v. Retrying...\n", err)
			time.Sleep(time.Second)
		}
		// Reset everything.
		itrs, mts, cbs, sz = itrs[:0], mts[:0], cbs[:0], 0
	}
	return nil
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

// This function does a filewalk, calculates the size of vlog and sst files and stores it in
// y.LSMSize and y.VlogSize.
func (db *DB) calculateSize() {
	if db.opt.InMemory {
		return
	}
	newInt := func(val int64) *expvar.Int {
		v := new(expvar.Int)
		v.Add(val)
		return v
	}

	totalSize := func(dir string) (int64, int64) {
		var lsmSize, vlogSize int64
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			ext := filepath.Ext(path)
			switch ext {
			case ".sst":
				lsmSize += info.Size()
			case ".vlog":
				vlogSize += info.Size()
			}
			return nil
		})
		if err != nil {
			db.opt.Debugf("Got error while calculating total size of directory: %s", dir)
		}
		return lsmSize, vlogSize
	}

	lsmSize, vlogSize := totalSize(db.opt.Dir)
	y.LSMSizeSet(db.opt.MetricsEnabled, db.opt.Dir, newInt(lsmSize))
	// If valueDir is different from dir, we'd have to do another walk.
	if db.opt.ValueDir != db.opt.Dir {
		_, vlogSize = totalSize(db.opt.ValueDir)
	}
	y.VlogSizeSet(db.opt.MetricsEnabled, db.opt.ValueDir, newInt(vlogSize))
}

func (db *DB) updateSize(lc *z.Closer) {
	defer lc.Done()
	if db.opt.InMemory {
		return
	}

	metricsTicker := time.NewTicker(time.Minute)
	defer metricsTicker.Stop()

	for {
		select {
		case <-metricsTicker.C:
			db.calculateSize()
		case <-lc.HasBeenClosed():
			return
		}
	}
}

// RunValueLogGC triggers a value log garbage collection.
//
// It picks value log files to perform GC based on statistics that are collected
// during compactions.  If no such statistics are available, then log files are
// picked in random order. The process stops as soon as the first log file is
// encountered which does not result in garbage collection.
//
// When a log file is picked, it is first sampled. If the sample shows that we
// can discard at least discardRatio space of that file, it would be rewritten.
//
// If a call to RunValueLogGC results in no rewrites, then an ErrNoRewrite is
// thrown indicating that the call resulted in no file rewrites.
//
// We recommend setting discardRatio to 0.5, thus indicating that a file be
// rewritten if half the space can be discarded.  This results in a lifetime
// value log write amplification of 2 (1 from original write + 0.5 rewrite +
// 0.25 + 0.125 + ... = 2). Setting it to higher value would result in fewer
// space reclaims, while setting it to a lower value would result in more space
// reclaims at the cost of increased activity on the LSM tree. discardRatio
// must be in the range (0.0, 1.0), both endpoints excluded, otherwise an
// ErrInvalidRequest is returned.
//
// Only one GC is allowed at a time. If another value log GC is running, or DB
// has been closed, this would return an ErrRejected.
//
// Note: Every time GC is run, it would produce a spike of activity on the LSM
// tree.
//// RunValueLogGC 触发了一个Value log的GC
//
// 它根据在compaction过程中收集到的统计数据，挑选valuelog文件来执行GC。
// 如果没有这样的统计数据，那么日志文件就会被以随机顺序挑选。这个过程在遇到第一个日志文件时立即停止
//
// 当一个日志文件被选中时，它首先被抽样。如果采样结果显示，我们
// 可以至少丢弃该文件的discardRatio空间，它将被重写。
//
// 如果对RunValueLogGC的调用没有导致任何重写，那么就会抛出一个ErrNoRewrite，表明调用的结果是
// 抛出，表明该调用没有导致文件的重写。
//
// 我们建议将丢弃率设置为0.5，这样就表明如果文件有一半的空间被重写，那么文件就会被重写。
// 如果有一半的空间可以被丢弃的话，就应该重写。 这将导致寿命
//值日志写入放大率为2（1来自原始写入+0.5重写 +
// 0.25 + 0.125 + ... = 2). 将其设置为更高的值会导致更少的
// 而将其设置为较低的值则会导致更多的空间
//回收更多空间，代价是增加LSM树上的活动。 丢弃率
// 必须在(0.0, 1.0)范围内，两个端点都不包括在内，否则会出现
// 否则将返回ErrInvalidRequest。
//
// 每次只允许一个GC。如果另一个值日志GC正在运行，或者DB
// 已经被关闭，这将返回一个ErrRejected。
//
// 注意：每次运行GC，它都会在LSM树上产生一个活动的峰值
func (db *DB) RunValueLogGC(discardRatio float64) error {
	if db.opt.InMemory {
		return ErrGCInMemoryMode
	}
	if discardRatio >= 1.0 || discardRatio <= 0.0 {
		return ErrInvalidRequest
	}

	// Pick a log file and run GC
	//1. 获取一个vlog文件并执行GC
	return db.vlog.runGC(discardRatio)
}

// Size returns the size of lsm and value log files in bytes. It can be used to decide how often to
// call RunValueLogGC.
func (db *DB) Size() (lsm, vlog int64) {
	if y.LSMSizeGet(db.opt.MetricsEnabled, db.opt.Dir) == nil {
		lsm, vlog = 0, 0
		return
	}
	lsm = y.LSMSizeGet(db.opt.MetricsEnabled, db.opt.Dir).(*expvar.Int).Value()
	vlog = y.VlogSizeGet(db.opt.MetricsEnabled, db.opt.ValueDir).(*expvar.Int).Value()
	return
}

// Sequence represents a Badger sequence.
type Sequence struct {
	lock      sync.Mutex
	db        *DB
	key       []byte
	next      uint64
	leased    uint64
	bandwidth uint64
}

// Next would return the next integer in the sequence, updating the lease by running a transaction
// if needed.
func (seq *Sequence) Next() (uint64, error) {
	seq.lock.Lock()
	defer seq.lock.Unlock()
	if seq.next >= seq.leased {
		if err := seq.updateLease(); err != nil {
			return 0, err
		}
	}
	val := seq.next
	seq.next++
	return val, nil
}

// Release the leased sequence to avoid wasted integers. This should be done right
// before closing the associated DB. However it is valid to use the sequence after
// it was released, causing a new lease with full bandwidth.
func (seq *Sequence) Release() error {
	seq.lock.Lock()
	defer seq.lock.Unlock()
	err := seq.db.Update(func(txn *Txn) error {
		item, err := txn.Get(seq.key)
		if err != nil {
			return err
		}

		var num uint64
		if err := item.Value(func(v []byte) error {
			num = binary.BigEndian.Uint64(v)
			return nil
		}); err != nil {
			return err
		}

		if num == seq.leased {
			var buf [8]byte
			binary.BigEndian.PutUint64(buf[:], seq.next)
			return txn.SetEntry(NewEntry(seq.key, buf[:]))
		}

		return nil
	})
	if err != nil {
		return err
	}
	seq.leased = seq.next
	return nil
}

func (seq *Sequence) updateLease() error {
	return seq.db.Update(func(txn *Txn) error {
		item, err := txn.Get(seq.key)
		switch {
		case err == ErrKeyNotFound:
			seq.next = 0
		case err != nil:
			return err
		default:
			var num uint64
			if err := item.Value(func(v []byte) error {
				num = binary.BigEndian.Uint64(v)
				return nil
			}); err != nil {
				return err
			}
			seq.next = num
		}

		lease := seq.next + seq.bandwidth
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], lease)
		if err = txn.SetEntry(NewEntry(seq.key, buf[:])); err != nil {
			return err
		}
		seq.leased = lease
		return nil
	})
}

// GetSequence would initiate a new sequence object, generating it from the stored lease, if
// available, in the database. Sequence can be used to get a list of monotonically increasing
// integers. Multiple sequences can be created by providing different keys. Bandwidth sets the
// size of the lease, determining how many Next() requests can be served from memory.
//
// GetSequence is not supported on ManagedDB. Calling this would result in a panic.
func (db *DB) GetSequence(key []byte, bandwidth uint64) (*Sequence, error) {
	if db.opt.managedTxns {
		panic("Cannot use GetSequence with managedDB=true.")
	}

	switch {
	case len(key) == 0:
		return nil, ErrEmptyKey
	case bandwidth == 0:
		return nil, ErrZeroBandwidth
	}
	seq := &Sequence{
		db:        db,
		key:       key,
		next:      0,
		leased:    0,
		bandwidth: bandwidth,
	}
	err := seq.updateLease()
	return seq, err
}

// Tables gets the TableInfo objects from the level controller. If withKeysCount
// is true, TableInfo objects also contain counts of keys for the tables.
func (db *DB) Tables() []TableInfo {
	return db.lc.getTableInfo()
}

// Levels gets the LevelInfo.
func (db *DB) Levels() []LevelInfo {
	return db.lc.getLevelInfo()
}

// EstimateSize can be used to get rough estimate of data size for a given prefix.
func (db *DB) EstimateSize(prefix []byte) (uint64, uint64) {
	var onDiskSize, uncompressedSize uint64
	tables := db.Tables()
	for _, ti := range tables {
		if bytes.HasPrefix(ti.Left, prefix) && bytes.HasPrefix(ti.Right, prefix) {
			onDiskSize += uint64(ti.OnDiskSize)
			uncompressedSize += uint64(ti.UncompressedSize)
		}
	}
	return onDiskSize, uncompressedSize
}

// Ranges can be used to get rough key ranges to divide up iteration over the DB. The ranges here
// would consider the prefix, but would not necessarily start or end with the prefix. In fact, the
// first range would have nil as left key, and the last range would have nil as the right key.
func (db *DB) Ranges(prefix []byte, numRanges int) []*keyRange {
	var splits []string
	tables := db.Tables()

	// We just want table ranges here and not keys count.
	for _, ti := range tables {
		// We don't use ti.Left, because that has a tendency to store !badger keys. Skip over tables
		// at upper levels. Only choose tables from the last level.
		if ti.Level != db.opt.MaxLevels-1 {
			continue
		}
		if bytes.HasPrefix(ti.Right, prefix) {
			splits = append(splits, string(ti.Right))
		}
	}

	// If the number of splits is low, look at the offsets inside the
	// tables to generate more splits.
	if len(splits) < 32 {
		numTables := len(tables)
		if numTables == 0 {
			numTables = 1
		}
		numPerTable := 32 / numTables
		if numPerTable == 0 {
			numPerTable = 1
		}
		splits = db.lc.keySplits(numPerTable, prefix)
	}

	// If the number of splits is still < 32, then look at the memtables.
	if len(splits) < 32 {
		maxPerSplit := 10000
		mtSplits := func(mt *memTable) {
			if mt == nil {
				return
			}
			count := 0
			iter := mt.sl.NewIterator()
			for iter.SeekToFirst(); iter.Valid(); iter.Next() {
				if count%maxPerSplit == 0 {
					// Add a split every maxPerSplit keys.
					if bytes.HasPrefix(iter.Key(), prefix) {
						splits = append(splits, string(iter.Key()))
					}
				}
				count += 1
			}
			_ = iter.Close()
		}

		db.lock.Lock()
		defer db.lock.Unlock()
		var memTables []*memTable
		memTables = append(memTables, db.imm...)
		for _, mt := range memTables {
			mtSplits(mt)
		}
		mtSplits(db.mt)
	}

	// We have our splits now. Let's convert them to ranges.
	sort.Strings(splits)
	var ranges []*keyRange
	var start []byte
	for _, key := range splits {
		ranges = append(ranges, &keyRange{left: start, right: y.SafeCopy(nil, []byte(key))})
		start = y.SafeCopy(nil, []byte(key))
	}
	ranges = append(ranges, &keyRange{left: start})

	// Figure out the approximate table size this range has to deal with.
	for _, t := range tables {
		tr := keyRange{left: t.Left, right: t.Right}
		for _, r := range ranges {
			if len(r.left) == 0 || len(r.right) == 0 {
				continue
			}
			if r.overlapsWith(tr) {
				r.size += int64(t.UncompressedSize)
			}
		}
	}

	var total int64
	for _, r := range ranges {
		total += r.size
	}
	if total == 0 {
		return ranges
	}
	// Figure out the average size, so we know how to bin the ranges together.
	avg := total / int64(numRanges)

	var out []*keyRange
	var i int
	for i < len(ranges) {
		r := ranges[i]
		cur := &keyRange{left: r.left, size: r.size, right: r.right}
		i++
		for ; i < len(ranges); i++ {
			next := ranges[i]
			if cur.size+next.size > avg {
				break
			}
			cur.right = next.right
			cur.size += next.size
		}
		out = append(out, cur)
	}
	return out
}

// MaxBatchCount returns max possible entries in batch
func (db *DB) MaxBatchCount() int64 {
	return db.opt.maxBatchCount
}

// MaxBatchSize returns max possible batch size
func (db *DB) MaxBatchSize() int64 {
	return db.opt.maxBatchSize
}

func (db *DB) stopMemoryFlush() {
	// Stop memtable flushes.
	if db.closers.memtable != nil {
		close(db.flushChan)
		db.closers.memtable.SignalAndWait()
	}
}

func (db *DB) stopCompactions() {
	// Stop compactions.
	if db.closers.compactors != nil {
		db.closers.compactors.SignalAndWait()
	}
}

func (db *DB) startCompactions() {
	// Resume compactions.
	if db.closers.compactors != nil {
		db.closers.compactors = z.NewCloser(1)
		db.lc.startCompact(db.closers.compactors)
	}
}

func (db *DB) startMemoryFlush() {
	// Start memory fluhser.
	if db.closers.memtable != nil {
		db.flushChan = make(chan flushTask, db.opt.NumMemtables)
		db.closers.memtable = z.NewCloser(1)
		go func() {
			_ = db.flushMemtable(db.closers.memtable)
		}()
	}
}

// Flatten can be used to force compactions on the LSM tree so all the tables fall on the same
// level. This ensures that all the versions of keys are colocated and not split across multiple
// levels, which is necessary after a restore from backup. During Flatten, live compactions are
// stopped. Ideally, no writes are going on during Flatten. Otherwise, it would create competition
// between flattening the tree and new tables being created at level zero.
func (db *DB) Flatten(workers int) error {

	db.stopCompactions()
	defer db.startCompactions()

	compactAway := func(cp compactionPriority) error {
		db.opt.Infof("Attempting to compact with %+v\n", cp)
		errCh := make(chan error, 1)
		for i := 0; i < workers; i++ {
			go func() {
				errCh <- db.lc.doCompact(175, cp)
			}()
		}
		var success int
		var rerr error
		for i := 0; i < workers; i++ {
			err := <-errCh
			if err != nil {
				rerr = err
				db.opt.Warningf("While running doCompact with %+v. Error: %v\n", cp, err)
			} else {
				success++
			}
		}
		if success == 0 {
			return rerr
		}
		// We could do at least one successful compaction. So, we'll consider this a success.
		db.opt.Infof("%d compactor(s) succeeded. One or more tables from level %d compacted.\n",
			success, cp.level)
		return nil
	}

	hbytes := func(sz int64) string {
		return humanize.IBytes(uint64(sz))
	}

	t := db.lc.levelTargets()
	for {
		db.opt.Infof("\n")
		var levels []int
		for i, l := range db.lc.levels {
			sz := l.getTotalSize()
			db.opt.Infof("Level: %d. %8s Size. %8s Max.\n",
				i, hbytes(l.getTotalSize()), hbytes(t.targetSz[i]))
			if sz > 0 {
				levels = append(levels, i)
			}
		}
		if len(levels) <= 1 {
			prios := db.lc.pickCompactLevels()
			if len(prios) == 0 || prios[0].score <= 1.0 {
				db.opt.Infof("All tables consolidated into one level. Flattening done.\n")
				return nil
			}
			if err := compactAway(prios[0]); err != nil {
				return err
			}
			continue
		}
		// Create an artificial compaction priority, to ensure that we compact the level.
		cp := compactionPriority{level: levels[0], score: 1.71}
		if err := compactAway(cp); err != nil {
			return err
		}
	}
}

func (db *DB) blockWrite() error {
	// Stop accepting new writes.
	if !atomic.CompareAndSwapInt32(&db.blockWrites, 0, 1) {
		return ErrBlockedWrites
	}

	// Make all pending writes finish. The following will also close writeCh.
	db.closers.writes.SignalAndWait()
	db.opt.Infof("Writes flushed. Stopping compactions now...")
	return nil
}

func (db *DB) unblockWrite() {
	db.closers.writes = z.NewCloser(2)
	go db.doWrites(db.closers.writes)
	go db.handleHandovers(db.closers.writes)

	// Resume writes.
	atomic.StoreInt32(&db.blockWrites, 0)
}

func (db *DB) prepareToDrop() (func(), error) {
	if db.opt.ReadOnly {
		panic("Attempting to drop data in read-only mode.")
	}
	// In order prepare for drop, we need to block the incoming writes and
	// write it to db. Then, flush all the pending flushtask. So that, we
	// don't miss any entries.
	if err := db.blockWrite(); err != nil {
		return func() {}, err
	}
	reqs := make([]*request, 0, 10)
	skls := make([]*handoverRequest, 0, 5)
	for {
		select {
		case r := <-db.writeCh:
			reqs = append(reqs, r)
		case skl := <-db.sklCh:
			skls = append(skls, skl)
		default:
			if err := db.writeRequests(reqs); err != nil {
				db.opt.Errorf("writeRequests: %v", err)
			}
			for _, skl := range skls {
				skl.err = db.handoverSkiplist(skl)
				skl.wg.Done()
				if skl.err != nil {
					db.opt.Errorf("handoverSkiplists: %v", skl.err)
				}
			}
			db.stopMemoryFlush()
			return func() {
				db.opt.Infof("Resuming writes")
				db.startMemoryFlush()
				db.unblockWrite()
			}, nil
		}
	}
}

// DropAll would drop all the data stored in Badger. It does this in the following way.
// - Stop accepting new writes.
// - Pause memtable flushes and compactions.
// - Pick all tables from all levels, create a changeset to delete all these
// tables and apply it to manifest.
// - Pick all log files from value log, and delete all of them. Restart value log files from zero.
// - Resume memtable flushes and compactions.
//
// NOTE: DropAll is resilient to concurrent writes, but not to reads. It is up to the user to not do
// any reads while DropAll is going on, otherwise they may result in panics. Ideally, both reads and
// writes are paused before running DropAll, and resumed after it is finished.
func (db *DB) DropAll() error {
	f, err := db.dropAll()
	if f != nil {
		f()
	}
	return err
}

func (db *DB) dropAll() (func(), error) {
	db.opt.Infof("DropAll called. Blocking writes...")
	f, err := db.prepareToDrop()
	if err != nil {
		return f, err
	}
	// prepareToDrop will stop all the incomming write and flushes any pending flush tasks.
	// Before we drop, we'll stop the compaction because anyways all the datas are going to
	// be deleted.
	db.stopCompactions()
	resume := func() {
		db.startCompactions()
		f()
	}
	// Block all foreign interactions with memory tables.
	db.lock.Lock()
	defer db.lock.Unlock()

	// Remove inmemory tables. Calling DecrRef for safety. Not sure if they're absolutely needed.
	db.mt.DecrRef()
	for _, mt := range db.imm {
		mt.DecrRef()
	}
	db.imm = db.imm[:0]
	db.mt, err = db.newMemTable() // Set it up for future writes.
	if err != nil {
		return resume, y.Wrapf(err, "cannot open new memtable")
	}

	num, err := db.lc.dropTree()
	if err != nil {
		return resume, err
	}
	db.opt.Infof("Deleted %d SSTables. Now deleting value logs...\n", num)

	num, err = db.vlog.dropAll()
	if err != nil {
		return resume, err
	}
	db.lc.nextFileID = 1
	db.opt.Infof("Deleted %d value log files. DropAll done.\n", num)
	db.blockCache.Clear()
	db.indexCache.Clear()
	db.threshold.Clear(db.opt)
	return resume, nil
}

// DropPrefixNonBlocking would logically drop all the keys with the provided prefix. The data would
// not be cleared from LSM tree immediately. It would be deleted eventually through compactions.
// This operation is useful when we don't want to block writes while we delete the prefixes.
// It does this in the following way:
// - Stream the given prefixes at a given ts.
// - Write them to skiplist at the specified ts and handover that skiplist to DB.
func (db *DB) DropPrefixNonBlocking(prefixes ...[]byte) error {
	if db.opt.ReadOnly {
		return errors.New("Attempting to drop data in read-only mode.")
	}

	if len(prefixes) == 0 {
		return nil
	}
	db.opt.Infof("Non-blocking DropPrefix called for %s", prefixes)

	cbuf := z.NewBuffer(int(db.opt.MemTableSize), "DropPrefixNonBlocking")
	defer cbuf.Release()

	var wg sync.WaitGroup
	handover := func(force bool) error {
		if !force && int64(cbuf.LenNoPadding()) < db.opt.MemTableSize {
			return nil
		}

		// Sort the kvs, add them to the builder, and hand it over to DB.
		cbuf.SortSlice(func(left, right []byte) bool {
			return y.CompareKeys(left, right) < 0
		})

		b := skl.NewBuilder(db.opt.MemTableSize)
		err := cbuf.SliceIterate(func(s []byte) error {
			b.Add(s, y.ValueStruct{Meta: bitDelete})
			return nil
		})
		if err != nil {
			return err
		}
		cbuf.Reset()
		wg.Add(1)
		return db.HandoverSkiplist(b.Skiplist(), wg.Done)
	}

	dropPrefix := func(prefix []byte) error {
		stream := db.NewStreamAt(math.MaxUint64)
		stream.LogPrefix = fmt.Sprintf("Dropping prefix: %#x", prefix)
		stream.Prefix = prefix
		// We don't need anything except key and version.
		stream.KeyToList = func(key []byte, itr *Iterator) (*pb.KVList, error) {
			if !itr.Valid() {
				return nil, nil
			}
			item := itr.Item()
			if item.IsDeletedOrExpired() {
				return nil, nil
			}
			if !bytes.Equal(key, item.Key()) {
				// Return on the encounter with another key.
				return nil, nil
			}

			a := itr.Alloc
			ka := a.Copy(key)
			list := &pb.KVList{}
			// We need to generate only a single delete marker per key. All the versions for this
			// key will be considered deleted, if we delete the one at highest version.
			kv := y.NewKV(a)
			kv.Key = y.KeyWithTs(ka, item.Version())
			list.Kv = append(list.Kv, kv)
			itr.Next()
			return list, nil
		}

		stream.Send = func(buf *z.Buffer) error {
			kv := pb.KV{}
			err := buf.SliceIterate(func(s []byte) error {
				kv.Reset()
				if err := kv.Unmarshal(s); err != nil {
					return err
				}
				cbuf.WriteSlice(kv.Key)
				return nil
			})
			if err != nil {
				return err
			}
			return handover(false)
		}
		if err := stream.Orchestrate(context.Background()); err != nil {
			return err
		}
		// Flush the remaining skiplists if any.
		return handover(true)
	}

	// Iterate over all the prefixes and logically drop them.
	for _, prefix := range prefixes {
		if err := dropPrefix(prefix); err != nil {
			return errors.Wrapf(err, "While dropping prefix: %#x", prefix)
		}
	}

	wg.Wait()
	return nil
}

// DropPrefix would drop all the keys with the provided prefix. Based on DB options, it either drops
// the prefixes by blocking the writes or doing a logical drop.
// See DropPrefixBlocking and DropPrefixNonBlocking for more information.
func (db *DB) DropPrefix(prefixes ...[]byte) error {
	if db.opt.AllowStopTheWorld {
		return db.DropPrefixBlocking(prefixes...)
	}
	return db.DropPrefixNonBlocking(prefixes...)
}

// DropPrefix would drop all the keys with the provided prefix. It does this in the following way:
// - Stop accepting new writes.
// - Stop memtable flushes before acquiring lock. Because we're acquring lock here
//   and memtable flush stalls for lock, which leads to deadlock
// - Flush out all memtables, skipping over keys with the given prefix, Kp.
// - Write out the value log header to memtables when flushing, so we don't accidentally bring Kp
//   back after a restart.
// - Stop compaction.
// - Compact L0->L1, skipping over Kp.
// - Compact rest of the levels, Li->Li, picking tables which have Kp.
// - Resume memtable flushes, compactions and writes.
func (db *DB) DropPrefixBlocking(prefixes ...[]byte) error {
	if len(prefixes) == 0 {
		return nil
	}
	db.opt.Infof("DropPrefix called for %s", prefixes)
	f, err := db.prepareToDrop()
	if err != nil {
		return err
	}
	defer f()

	var filtered [][]byte
	if filtered, err = db.filterPrefixesToDrop(prefixes); err != nil {
		return err
	}
	// If there is no prefix for which the data already exist, do not do anything.
	if len(filtered) == 0 {
		db.opt.Infof("No prefixes to drop")
		return nil
	}
	// Block all foreign interactions with memory tables.
	db.lock.Lock()
	defer db.lock.Unlock()

	db.imm = append(db.imm, db.mt)
	for _, memtable := range db.imm {
		if memtable.sl.Empty() {
			memtable.DecrRef()
			continue
		}
		task := flushTask{
			mt: memtable,
			// Ensure that the head of value log gets persisted to disk.
			dropPrefixes: filtered,
		}
		db.opt.Debugf("Flushing memtable")
		if err := db.handleFlushTask(task); err != nil {
			db.opt.Errorf("While trying to flush memtable: %v", err)
			return err
		}
		memtable.DecrRef()
	}
	db.stopCompactions()
	defer db.startCompactions()
	db.imm = db.imm[:0]
	db.mt, err = db.newMemTable()
	if err != nil {
		return y.Wrapf(err, "cannot create new mem table")
	}

	// Drop prefixes from the levels.
	if err := db.lc.dropPrefixes(filtered); err != nil {
		return err
	}
	db.opt.Infof("DropPrefix done")
	return nil
}

func (db *DB) filterPrefixesToDrop(prefixes [][]byte) ([][]byte, error) {
	var filtered [][]byte
	for _, prefix := range prefixes {
		err := db.View(func(txn *Txn) error {
			iopts := DefaultIteratorOptions
			iopts.Prefix = prefix
			iopts.PrefetchValues = false
			itr := txn.NewIterator(iopts)
			defer itr.Close()
			itr.Rewind()
			if itr.ValidForPrefix(prefix) {
				filtered = append(filtered, prefix)
			}
			return nil
		})
		if err != nil {
			return filtered, err
		}
	}
	return filtered, nil
}

// Checks if the key is banned. Returns the respective error if the key belongs to any of the banned
// namepspaces. Else it returns nil.
//检查key的有效性
func (db *DB) isBanned(key []byte) error {
	if db.opt.NamespaceOffset < 0 {
		return nil
	}
	if len(key) <= db.opt.NamespaceOffset+8 {
		return nil
	}
	if db.bannedNamespaces.has(y.BytesToU64(key[db.opt.NamespaceOffset:])) {
		return ErrBannedKey
	}
	return nil
}

// BanNamespace bans a namespace. Read/write to keys belonging to any of such namespace is denied.
func (db *DB) BanNamespace(ns uint64) error {
	if db.opt.NamespaceOffset < 0 {
		return ErrNamespaceMode
	}
	db.opt.Infof("Banning namespace: %d", ns)
	// First set the banned namespaces in DB and then update the in-memory structure.
	key := y.KeyWithTs(append(bannedNsKey, y.U64ToBytes(ns)...), 1)
	entry := []*Entry{{
		Key:   key,
		Value: nil,
	}}
	req, err := db.sendToWriteCh(entry)
	if err != nil {
		return err
	}
	if err := req.Wait(); err != nil {
		return err
	}
	db.bannedNamespaces.add(ns)
	return nil
}

// BannedNamespaces returns the list of prefixes banned for DB.
func (db *DB) BannedNamespaces() []uint64 {
	return db.bannedNamespaces.all()
}

// KVList contains a list of key-value pairs.
type KVList = pb.KVList

// Subscribe can be used to watch key changes for the given key prefixes and the ignore string.
// At least one prefix should be passed, or an error will be returned.
// You can use an empty prefix to monitor all changes to the DB.
// Ignore string is the byte ranges for which prefix matching will be ignored.
// For example: ignore = "2-3", and prefix = "abc" will match for keys "abxxc", "abdfc" etc.
// This function blocks until the given context is done or an error occurs.
// The given function will be called with a new KVList containing the modified keys and the
// corresponding values.
func (db *DB) Subscribe(ctx context.Context, cb func(kv *KVList) error, matches []pb.Match) error {
	if cb == nil {
		return ErrNilCallback
	}

	c := z.NewCloser(1)
	s := db.pub.newSubscriber(c, matches)
	slurp := func(batch *pb.KVList) error {
		for {
			select {
			case kvs := <-s.sendCh:
				batch.Kv = append(batch.Kv, kvs.Kv...)
			default:
				if len(batch.GetKv()) > 0 {
					return cb(batch)
				}
				return nil
			}
		}
	}

	drain := func() {
		for {
			select {
			case <-s.sendCh:
			default:
				return
			}
		}
	}
	for {
		select {
		case <-c.HasBeenClosed():
			// No need to delete here. Closer will be called only while
			// closing DB. Subscriber will be deleted by cleanSubscribers.
			err := slurp(new(pb.KVList))
			// Drain if any pending updates.
			c.Done()
			return err
		case <-ctx.Done():
			c.Done()
			atomic.StoreUint64(s.active, 0)
			drain()
			db.pub.deleteSubscriber(s.id)
			// Delete the subscriber to avoid further updates.
			return ctx.Err()
		case batch := <-s.sendCh:
			err := slurp(batch)
			if err != nil {
				c.Done()
				atomic.StoreUint64(s.active, 0)
				drain()
				// Delete the subscriber if there is an error by the callback.
				db.pub.deleteSubscriber(s.id)
				return err
			}
		}
	}
}

// shouldEncrypt returns bool, which tells whether to encrypt or not.
func (db *DB) shouldEncrypt() bool {
	return len(db.opt.EncryptionKey) > 0
}

func (db *DB) syncDir(dir string) error {
	if db.opt.InMemory {
		return nil
	}
	return syncDir(dir)
}

func createDirs(opt Options) error {
	for _, path := range []string{opt.Dir, opt.ValueDir} {
		dirExists, err := exists(path)
		if err != nil {
			return y.Wrapf(err, "Invalid Dir: %q", path)
		}
		if !dirExists {
			if opt.ReadOnly {
				return errors.Errorf("Cannot find directory %q for read-only open", path)
			}
			// Try to create the directory
			err = os.MkdirAll(path, 0700)
			if err != nil {
				return y.Wrapf(err, "Error Creating Dir: %q", path)
			}
		}
	}
	return nil
}

// Stream the contents of this DB to a new DB with options outOptions that will be
// created in outDir.
func (db *DB) StreamDB(outOptions Options) error {
	outDir := outOptions.Dir

	// Open output DB.
	outDB, err := OpenManaged(outOptions)
	if err != nil {
		return y.Wrapf(err, "cannot open out DB at %s", outDir)
	}
	defer outDB.Close()
	writer := outDB.NewStreamWriter()
	if err := writer.Prepare(); err != nil {
		return y.Wrapf(err, "cannot create stream writer in out DB at %s", outDir)
	}

	// Stream contents of DB to the output DB.
	stream := db.NewStreamAt(math.MaxUint64)
	stream.LogPrefix = fmt.Sprintf("Streaming DB to new DB at %s", outDir)
	stream.FullCopy = true

	stream.Send = func(buf *z.Buffer) error {
		return writer.Write(buf)
	}
	if err := stream.Orchestrate(context.Background()); err != nil {
		return y.Wrapf(err, "cannot stream DB to out DB at %s", outDir)
	}
	if err := writer.Flush(); err != nil {
		return y.Wrapf(err, "cannot flush writer")
	}
	return nil
}

// Opts returns a copy of the DB options.
func (db *DB) Opts() Options {
	return db.opt
}

type CacheType int

const (
	BlockCache CacheType = iota
	IndexCache
)

// CacheMaxCost updates the max cost of the given cache (either block or index cache).
// The call will have an effect only if the DB was created with the cache. Otherwise it is
// a no-op. If you pass a negative value, the function will return the current value
// without updating it.
func (db *DB) CacheMaxCost(cache CacheType, maxCost int64) (int64, error) {
	if db == nil {
		return 0, nil
	}

	if maxCost < 0 {
		switch cache {
		case BlockCache:
			return db.blockCache.MaxCost(), nil
		case IndexCache:
			return db.indexCache.MaxCost(), nil
		default:
			return 0, errors.Errorf("invalid cache type")
		}
	}

	switch cache {
	case BlockCache:
		db.blockCache.UpdateMaxCost(maxCost)
		return maxCost, nil
	case IndexCache:
		db.indexCache.UpdateMaxCost(maxCost)
		return maxCost, nil
	default:
		return 0, errors.Errorf("invalid cache type")
	}
}

func (db *DB) LevelsToString() string {
	levels := db.Levels()
	h := func(sz int64) string {
		return humanize.IBytes(uint64(sz))
	}
	base := func(b bool) string {
		if b {
			return "B"
		}
		return " "
	}

	var b strings.Builder
	b.WriteRune('\n')
	for _, li := range levels {
		b.WriteString(fmt.Sprintf(
			"Level %d [%s]: NumTables: %02d. Size: %s of %s. Score: %.2f->%.2f"+
				" StaleData: %s Target FileSize: %s\n",
			li.Level, base(li.IsBaseLevel), li.NumTables,
			h(li.Size), h(li.TargetSize), li.Score, li.Adjusted, h(li.StaleDatSize),
			h(li.TargetFileSize)))
	}
	b.WriteString("Level Done\n")
	return b.String()
}