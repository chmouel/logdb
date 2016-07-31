// Package logdb provides an efficient log-structured database
// supporting efficient insertion of new entries, and efficient
// removal from either end of the log.
package logdb

import (
	"errors"
	"fmt"
	"os"
)

var (
	// ErrIDOutOfRange means that the requested ID is not present
	// in the log.
	ErrIDOutOfRange = errors.New("log ID out of range")

	// ErrUnknownVersion means that the disk format version of an
	// opened database is unknown.
	ErrUnknownVersion = errors.New("unknown disk format version")

	// ErrNotDirectory means that the path given to 'Open' exists
	// and is not a directory.
	ErrNotDirectory = errors.New("database path not a directory")

	// ErrPathDoesntExist means that the path given to 'Open' does
	// not exist and the 'create' flag was false.
	ErrPathDoesntExist = errors.New("database directory does not exist")

	// ErrCorrupt means that the database files are invalid.
	ErrCorrupt = errors.New("database corrupted")

	// ErrTooBig means that an entry could not be appended because
	// it is larger than the chunk size.
	ErrTooBig = errors.New("entry larger than chunksize")
)

const latestVersion = uint16(0)

// ReadError means that a read failed. It wraps the actual error.
type ReadError struct{ Err error }

func (e *ReadError) Error() string          { return e.Err.Error() }
func (e *ReadError) WrappedErrors() []error { return []error{e.Err} }

// WriteError means that a write failed. It wraps the actual error.
type WriteError struct{ Err error }

func (e *WriteError) Error() string          { return e.Err.Error() }
func (e *WriteError) WrappedErrors() []error { return []error{e.Err} }

// PathError means that a directory could not be created. It wraps the
// actual error.
type PathError struct{ Err error }

func (e *PathError) Error() string          { return e.Err.Error() }
func (e *PathError) WrappedErrors() []error { return []error{e.Err} }

// SyncError means that a file could not be synced to disk. It wraps
// the actual error.
type SyncError struct{ Err error }

func (e *SyncError) Error() string          { return e.Err.Error() }
func (e *SyncError) WrappedErrors() []error { return []error{e.Err} }

// DeleteError means that a file could not be deleted from disk. It
// wraps the actual error.
type DeleteError struct{ Err error }

func (e *DeleteError) Error() string          { return e.Err.Error() }
func (e *DeleteError) WrappedErrors() []error { return []error{e.Err} }

// LockError means that the database files could not be locked. It
// wraps the actual error.
type LockError struct{ Err error }

func (e *LockError) Error() string          { return e.Err.Error() }
func (e *LockError) WrappedErrors() []error { return []error{e.Err} }

// AtomicityError means that an error occurred while appending an
// entry in an 'AppendEntries' call, and attempting to rollback also
// gave an error. It wraps the actual errors.
type AtomicityError struct {
	AppendErr   error
	RollbackErr error
}

func (e *AtomicityError) Error() string {
	return fmt.Sprintf("error rolling back after append error: %s (%s)", e.RollbackErr.Error(), e.AppendErr.Error())
}

func (e *AtomicityError) WrappedErrors() []error {
	return []error{e.AppendErr, e.RollbackErr}
}

// A LogDB is a reference to an efficient log-structured database
// providing ACID consistency guarantees.
type LogDB interface {
	// Append writes a new entry to the log.
	//
	// Returns a 'WriteError' value if the database files could
	// not be written to, and a 'SyncError' value if a periodic
	// synchronisation failed.
	Append(entry []byte) error

	// Atomically write a collection of new entries to the log.
	//
	// Returns the same errors as 'Append', and an
	// 'AtomicityError' value if any entry fails to append and
	// rolling back the log failed.
	AppendEntries(entries [][]byte) error

	// Get looks up an entry by ID.
	//
	// Returns 'ErrIDOutOfRange' if the requested ID is lesser
	// than the oldest or greater than the newest.
	Get(id uint64) ([]byte, error)

	// Forget removes entries from the end of the log.
	//
	// Returns 'ErrIDOutOfRange' if the new "oldest" ID is lesser
	// than the current oldest, a 'DeleteError' if a chunk file
	// could not be deleted, and a 'SyncError' value if a periodic
	// synchronisation failed.
	Forget(newOldestID uint64) error

	// Rollback removes entries from the head of the log.
	//
	// Returns 'ErrIDOutOfRange' if the new "newest" ID is greater
	// than the current next, a 'DeleteError' if a chunk file
	// could not be deleted, and a 'SyncError' value if a periodic
	// synchronisation failed.
	Rollback(newNewestID uint64) error

	// Perform a combination 'Forget'/'Rollback' operation, this
	// is atomic.
	//
	// Returns the same errors as 'Forget' and 'Rollback'.
	Truncate(newOldestID, newNewestID uint64) error

	// Synchronise the data to disk after touching (appending,
	// forgetting, or rolling back) at most this many entries.
	// Data is always synced if an entire chunk is forgotten or
	// rolled back.
	//
	// <0 disables periodic syncing, and 'Sync' must be called
	// instead. The default value is 256.
	//
	// Returns a 'SyncError' value if this triggered an immediate
	// synchronisation, which failed.
	SetSync(every int) error

	// Synchronise the data to disk now.
	//
	// May return a SyncError value.
	Sync() error

	// OldestID gets the ID of the oldest log entry.
	//
	// For an empty database, this will return 0.
	OldestID() uint64

	// NewestID gets the ID of the newest log entry.
	//
	// For an empty database, this will return 0.
	NewestID() uint64

	// Sync the database and close any open files. It is an error
	// to try to use a database after closing it.
	//
	// May return a 'SyncError' value.
	Close() error
}

// Open a database.
//
// It is not safe to have multiple open references to the same
// database at the same time, across any number of processes.
// Concurrent usage of one open reference in a single process is safe.
//
// If the 'create' flag is true and the database doesn't already
// exist, the database is created using the given chunk size. If the
// database does exist, the chunk size is detected automatically.
//
// The log is stored on disk in fixed-size files, controlled by the
// 'chunkSize' parameter. If entries are a fixed size, the chunk size
// should be a multiple of that to avoid wasting space. There is a
// trade-off to be made: a chunk is only deleted when its entries do
// not overlap with the live entries at all (this happens through
// calls to 'Forget' and 'Rollback'), so a larger chunk size means
// fewer files, but longer persistence.
func Open(path string, chunkSize uint32, create bool) (LogDB, error) {
	// Check if it already exists.
	if stat, _ := os.Stat(path); stat != nil {
		if !stat.IsDir() {
			return nil, ErrNotDirectory
		}
		return opendb(path)
	}
	if create {
		return createdb(path, chunkSize)
	}
	return nil, ErrPathDoesntExist
}

// Create a database. It is an error to call this function if the
// database directory already exists.
func createdb(path string, chunkSize uint32) (LogDB, error) {
	// Create the directory.
	if err := os.MkdirAll(path, os.ModeDir|0755); err != nil {
		return nil, &PathError{err}
	}

	// Write the version file
	if err := writeFile(path+"/version", latestVersion); err != nil {
		return nil, &WriteError{err}
	}

	// Write the chunk size file
	if err := writeFile(path+"/chunk_size", chunkSize); err != nil {
		return nil, &WriteError{err}
	}

	return createChunkSliceDB(path, chunkSize)
}

// Open an existing database. It is an error to call this function if
// the database directory does not exist.
func opendb(path string) (LogDB, error) {
	// Read the "version" file.
	var version uint16
	if err := readFile(path+"/version", &version); err != nil {
		return nil, &ReadError{err}
	}

	// Read the "chunk_size" file.
	var chunkSize uint32
	if err := readFile(path+"/chunk_size", &chunkSize); err != nil {
		return nil, &ReadError{err}
	}

	// Open the database.
	switch version {
	case 0:
		return openChunkSliceDB(path, chunkSize)
	default:
		return nil, ErrUnknownVersion
	}
}
