package logdb

import (
	"bytes"
	"encoding/binary"
	"io"
	"io/ioutil"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// chunkSliceDB is the main LogDB instance, representing a database as
// a slice of memory-mapped files.
type chunkSliceDB struct {
	path string

	// Any number of goroutines can read simultaneously, but when
	// entries are added or removed, the write lock must be held.
	//
	// An alternative design point to consider is to have a
	// separate 'RWMutex' in each chunk, and hold only the
	// necessary write locks. This would complicate locking but
	// allow for more concurrent reading, and so may be better
	// under some work loads.
	rwlock sync.RWMutex

	chunkSize uint32

	chunks []*chunk

	oldest uint64

	next uint64

	syncEvery     int
	sinceLastSync uint64
	syncDirty     []int
}

// A chunk is one memory-mapped file.
type chunk struct {
	path string

	bytes []byte
	mmapf *os.File

	// One past the ending addresses of entries in the 'bytes'
	// slice.
	//
	// This choice is because starting addresses can always be
	// calculated from ending addresses, as the first entry starts
	// at offset 0 (and there are no gaps). Ending addresses
	// cannot be calculated from starting addresses, unless the
	// ending address of the final entry is stored as well.
	ends []int32

	oldest uint64

	next uint64

	dirty bool
}

// Delete the files associated with a chunk.
func (c *chunk) closeAndRemove() error {
	if err := closeAndRemove(c.mmapf); err != nil {
		return err
	}
	return os.Remove(metaFilePath(c))
}

const (
	chunkPrefix      = "chunk_"
	metaSuffix       = "_meta"
	initialChunkFile = chunkPrefix + "0"
)

// Get the meta file path associated with a chunk file path.
func metaFilePath(chunkFilePath interface{}) string {
	switch cfg := chunkFilePath.(type) {
	case chunk:
		return cfg.path + metaSuffix
	case *chunk:
		return cfg.path + metaSuffix
	case string:
		return cfg + metaSuffix
	default:
		panic("internal error: bad type in metaFilePath")
	}
}

// Check if a file is a chunk data file.
//
// A valid chunk filename consists of the chunkPrefix followed by one
// or more digits, with no leading zeroes.
func isChunkDataFile(fi os.FileInfo) bool {
	bits := strings.Split(fi.Name(), chunkPrefix)
	// In the form chunkPrefix[.+]
	if len(bits) != 2 || len(bits[0]) != 0 || len(bits[1]) == 0 {
		return false
	}
	var nozero bool
	for _, r := range []rune(bits[1]) {
		// Must be a digit
		if !(r >= '0' && r <= '9') {
			return false
		}
		// No leading zeroes
		if r != '0' {
			nozero = true
		} else if nozero {
			return false
		}
	}
	return true
}

// Given a chunk, get the filename of the next chunk.
//
// This function panics if the chunk path is invalid. This should
// never happen unless openChunkSliceDB or isChunkDataFile is broken.
func (c *chunk) nextDataFileName() string {
	bits := strings.Split(c.path, "/"+chunkPrefix)
	if len(bits) < 2 {
		panic("malformed chunk file name: " + c.path)
	}

	num, err := strconv.Atoi(bits[len(bits)-1])
	if err != nil {
		panic("malformed chunk file name: " + c.path)
	}

	return chunkPrefix + strconv.Itoa(num+1)
}

// fileInfoSlice implements nice sorting for 'os.FileInfo': first
// compare filenames by length, and then lexicographically.
type fileInfoSlice []os.FileInfo

func (fis fileInfoSlice) Len() int {
	return len(fis)
}

func (fis fileInfoSlice) Swap(i, j int) {
	fis[i], fis[j] = fis[j], fis[i]
}

func (fis fileInfoSlice) Less(i, j int) bool {
	ni := fis[i].Name()
	nj := fis[j].Name()
	return len(ni) < len(nj) || ni < nj
}

func createChunkSliceDB(path string, chunkSize uint32) (*chunkSliceDB, error) {
	// Write the "oldest" file.
	if err := writeFile(path+"/oldest", uint64(0)); err != nil {
		return nil, &WriteError{err}
	}

	return &chunkSliceDB{path: path, chunkSize: chunkSize, syncEvery: 100}, nil
}

func openChunkSliceDB(path string, chunkSize uint32) (*chunkSliceDB, error) {
	// Read the "oldest" file.
	var oldest uint64
	if err := readFile(path+"/oldest", &oldest); err != nil {
		return nil, &ReadError{err}
	}

	// Get all the chunk files.
	var chunkFiles []os.FileInfo
	fis, err := ioutil.ReadDir(path)
	if err != nil {
		return nil, &ReadError{err}
	}
	for _, fi := range fis {
		if !fi.IsDir() && isChunkDataFile(fi) {
			chunkFiles = append(chunkFiles, fi)
		}
	}
	sort.Sort(fileInfoSlice(chunkFiles))

	// Populate the chunk slice.
	chunks := make([]*chunk, len(chunkFiles))
	chunkNext := oldest
	done := false
	for i, fi := range chunkFiles {
		if done {
			// We ran out of strictly increasing ending
			// positions, which should ONLY happen in the
			// last chunk file.
			return nil, ErrCorrupt
		}

		var c chunk
		c, done, err = openChunkFile(path, fi)
		if err != nil {
			return nil, err
		}
		chunks[i] = &c
		chunkNext = c.oldest + uint64(len(chunks[i].ends))
	}

	return &chunkSliceDB{
		path:      path,
		chunkSize: chunkSize,
		chunks:    chunks,
		oldest:    oldest,
		next:      chunkNext,
		syncEvery: 100,
	}, nil
}

// Open a chunk file
func openChunkFile(basedir string, fi os.FileInfo) (chunk, bool, error) {
	chunk := chunk{path: basedir + "/" + fi.Name()}

	// If 'done' gets set, then it means that chunk ending offsets
	// have stopped strictly increasing. This happens if entries
	// are rolled back, but not enough to delete an entire chunk:
	// the ending offsets get reset to 0 in the metadata file in
	// that case. This should only happen in the final chunk, so
	// it is an error for 'done' to become true if there are
	// further chunk files.
	var done bool

	// mmap the data file
	mmapf, bytes, err := mmap(chunk.path)
	if err != nil {
		return chunk, done, &ReadError{err}
	}
	chunk.bytes = bytes
	chunk.mmapf = mmapf

	// read the ending address metadata
	mfile, err := os.Open(metaFilePath(chunk))
	if err != nil {
		return chunk, done, &ReadError{err}
	}
	prior := int32(-1)
	if err := binary.Read(mfile, binary.LittleEndian, &chunk.oldest); err != nil {
		return chunk, done, ErrCorrupt
	}
	for {
		var this int32
		if err := binary.Read(mfile, binary.LittleEndian, &this); err != nil {
			if err == io.EOF {
				break
			}
			return chunk, done, ErrCorrupt
		}
		if this <= prior {
			done = true
			break
		}
		chunk.ends = append(chunk.ends, this)
		prior = this
	}

	chunk.next = chunk.oldest + uint64(len(chunk.ends))
	return chunk, done, nil
}

func (db *chunkSliceDB) Append(entry []byte) error {
	return defaultAppend(db, entry)
}

func (db *chunkSliceDB) AppendValue(value interface{}) error {
	return defaultAppendValue(db, value)
}

func (db *chunkSliceDB) AppendEntries(entries [][]byte) error {
	db.rwlock.Lock()
	defer db.rwlock.Unlock()

	originalNext := db.NextID()
	originalSyncAfter := db.syncEvery

	// Disable periodic syncing while appending.
	if err := db.SetSync(-1); err != nil {
		return err
	}

	for _, entry := range entries {
		if err := db.append(entry); err != nil {
			// Rollback on error.
			if rerr := db.Rollback(originalNext); rerr != nil {
				return &AtomicityError{AppendErr: err, RollbackErr: rerr}
			}
			return err
		}
	}

	return db.SetSync(originalSyncAfter)
}

func (db *chunkSliceDB) AppendValues(values []interface{}) error {
	return defaultAppendValues(db, values)
}

func (db *chunkSliceDB) append(entry []byte) error {
	// If there are no chunks, create a new one.
	if len(db.chunks) == 0 {
		if err := db.newChunk(); err != nil {
			return &WriteError{err}
		}
	}

	lastChunk := db.chunks[len(db.chunks)-1]

	// If the last chunk doesn't have the space for this entry,
	// create a new one.
	if len(lastChunk.ends) > 0 {
		lastEnd := lastChunk.ends[len(lastChunk.ends)-1]
		if db.chunkSize-uint32(lastEnd) < uint32(len(entry)) {
			if err := db.newChunk(); err != nil {
				return &WriteError{err}
			}
			lastChunk = db.chunks[len(db.chunks)-1]
		}
	}

	// Add the entry to the last chunk
	var start int32
	if len(lastChunk.ends) > 0 {
		start = lastChunk.ends[len(lastChunk.ends)-1]
	}
	end := start + int32(len(entry))
	for i, b := range entry {
		lastChunk.bytes[start+int32(i)] = b
	}
	lastChunk.ends = append(lastChunk.ends, end)

	// Increment twice at the first entry, to go from 0 (nothing
	// in database) to 2 (ID after entry 1)
	if db.next == 0 {
		lastChunk.next++
		db.next++
	}
	lastChunk.next++
	db.next++

	// If this is the first entry ever, set the oldest ID to 1
	// (IDs start from 1, not 0)
	if db.oldest == 0 {
		db.oldest = 1
		lastChunk.oldest = 1
	}

	// Mark the current chunk as dirty and perform a periodic sync.
	db.sinceLastSync++
	if !lastChunk.dirty {
		lastChunk.dirty = true
		db.syncDirty = append(db.syncDirty, len(db.chunks)-1)
	}
	return db.periodicSync(false)
}

// Add a new chunk to the database.
//
// A chunk cannot be empty, so it is only valid to call this if an
// entry is going to be inserted into the chunk immediately.
func (db *chunkSliceDB) newChunk() error {
	chunkFile := db.path + "/" + initialChunkFile

	// Filename is "chunk-<1 + last chunk file name>"
	if len(db.chunks) > 0 {
		chunkFile = db.path + "/" + db.chunks[len(db.chunks)-1].nextDataFileName()
	}

	// Create the chunk files.
	if err := createFile(chunkFile, db.chunkSize); err != nil {
		return err
	}
	metaBuf := new(bytes.Buffer)
	if err := binary.Write(metaBuf, binary.LittleEndian, db.next); err != nil {
		return err
	}
	if err := writeFile(metaFilePath(chunkFile), metaBuf.Bytes()); err != nil {
		return err
	}

	// Open the newly-created chunk file.
	fi, err := os.Stat(chunkFile)
	if err != nil {
		return err
	}
	c, _, err := openChunkFile(db.path, fi)
	if err != nil {
		return err
	}
	db.chunks = append(db.chunks, &c)

	return nil
}

func (db *chunkSliceDB) Get(id uint64) ([]byte, error) {
	db.rwlock.RLock()
	defer db.rwlock.RUnlock()

	// Check ID is in range.
	if id < db.oldest || id >= db.next || len(db.chunks) == 0 {
		return nil, ErrIDOutOfRange
	}

	// Binary search through chunks for the one containing the ID.
	lo := 0
	hi := len(db.chunks)
	mid := hi / 2
	for ; !(db.chunks[mid].oldest <= id && id < db.chunks[mid].next); mid = (hi + lo) / 2 {
		if db.chunks[mid].next <= id {
			lo = mid + 1
		} else if db.chunks[mid].oldest > id {
			hi = mid - 1
		}
	}

	// Calculate the start and end offset, and return a copy of
	// the relevant byte slice.
	chunk := db.chunks[mid]
	off := id - chunk.oldest
	start := int32(0)
	if off > 0 {
		start = chunk.ends[off-1]
	}
	end := chunk.ends[off]
	out := make([]byte, end-start)
	for i := start; i < end; i++ {
		out[i-start] = chunk.bytes[i]
	}
	return out, nil
}

func (db *chunkSliceDB) GetValue(id uint64, data interface{}) error {
	return defaultGetValue(db, id, data)
}

func (db *chunkSliceDB) Forget(newOldestID uint64) error {
	return defaultForget(db, newOldestID)
}

func (db *chunkSliceDB) Rollback(newNextID uint64) error {
	return defaultRollback(db, newNextID)
}

func (db *chunkSliceDB) Truncate(newOldestID uint64, newNextID uint64) error {
	db.rwlock.Lock()
	defer db.rwlock.Unlock()

	if newOldestID < db.oldest || newNextID > db.next {
		return ErrIDOutOfRange
	}

	// Remove the metadata for any entries being rolled back.
	for i := len(db.chunks) - 1; i >= 0 && db.chunks[i].next >= newNextID; i-- {
		c := db.chunks[i]
		c.ends = c.ends[0 : uint64(len(c.ends))-(c.next-newNextID)]
		if !c.dirty {
			c.dirty = true
			db.syncDirty = append(db.syncDirty, i)
		}
	}

	db.sinceLastSync += newOldestID - db.oldest
	db.sinceLastSync += db.next - newNextID
	db.oldest = newOldestID
	db.next = newNextID

	// Check if this deleted any chunks
	first := 0
	last := len(db.chunks)
	for ; first < len(db.chunks) && db.chunks[first].next < newOldestID; first++ {
	}
	for ; last > 0 && db.chunks[last-1].oldest > newNextID; last-- {
	}

	if first > 0 || last < len(db.chunks) {
		// It did! Sync everything and then delete the files.
		if err := db.sync(false); err != nil {
			return err
		}
		for i, c := range db.chunks {
			if i >= first && i < last {
				continue
			}
			if err := c.closeAndRemove(); err != nil {
				return &DeleteError{err}
			}
		}
		db.chunks = db.chunks[first:last]
	}
	return db.periodicSync(false)
}

func (db *chunkSliceDB) Clone(path string, version uint16, chunkSize uint32) (LogDB, error) {
	db.rwlock.RLock()
	defer db.rwlock.RUnlock()

	panic("unimplemented")
}

func (db *chunkSliceDB) SetSync(every int) error {
	db.syncEvery = every

	// Immediately perform a periodic sync.
	return db.periodicSync(true)
}

func (db *chunkSliceDB) Sync() error {
	return db.sync(true)
}

// Perform a sync only if needed. This function is not safe to execute
// concurrently with a write, so the 'acquireLock' parameter MUST be
// true UNLESS the write lock is already held by this thread.
func (db *chunkSliceDB) periodicSync(acquireLock bool) error {
	// Sync if the number of unsynced entries is above the
	// threshold
	if db.syncEvery >= 0 && db.sinceLastSync > uint64(db.syncEvery) {
		return db.sync(acquireLock)
	}
	return nil
}

// This function is not safe to execute concurrently with a write, so
// the 'acquireLock' parameter MUST be true UNLESS the write lock is
// already held by this thread.
func (db *chunkSliceDB) sync(acquireLock bool) error {
	if acquireLock {
		db.rwlock.RLock()
		defer db.rwlock.RUnlock()
	}

	for _, i := range db.syncDirty {
		// To ensure ACID, sync the data first and only then
		// the metadata. This means that if there is a failure
		// between the two syncs, even if the newly-written
		// data is corrupt, there will be no metadata
		// referring to it, and so it will be invisible to the
		// database when next opened.
		if err := fsync(db.chunks[i].mmapf); err != nil {
			return &SyncError{err}
		}

		metaBuf := new(bytes.Buffer)
		if err := binary.Write(metaBuf, binary.LittleEndian, db.chunks[i].oldest); err != nil {
			return err
		}
		for _, end := range db.chunks[i].ends {
			if err := binary.Write(metaBuf, binary.LittleEndian, end); err != nil {
				return err
			}
		}
		if err := writeFile(metaFilePath(db.chunks[i]), metaBuf.Bytes()); err != nil {
			return &SyncError{err}
		}
		db.chunks[i].dirty = false
	}

	// Write the oldest entry ID.
	if err := writeFile(db.path+"/oldest", db.oldest); err != nil {
		return &SyncError{err}
	}

	db.syncDirty = nil
	db.sinceLastSync = 0

	return nil
}

func (db *chunkSliceDB) OldestID() uint64 {
	return db.oldest
}

func (db *chunkSliceDB) NextID() uint64 {
	return db.next
}
