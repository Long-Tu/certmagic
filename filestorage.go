// Copyright 2015 Matthew Holt
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package certmagic

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// FileStorage facilitates forming file paths derived from a root
// directory. It is used to get file paths in a consistent,
// cross-platform way or persisting ACME assets on the file system.
type FileStorage struct {
	Path string

	fileStorageNameLocks   map[string]*fileStorageWaiter
	fileStorageNameLocksMu sync.Mutex
}

// Exists returns true if key exists in fs.
func (fs *FileStorage) Exists(key string) bool {
	_, err := os.Stat(fs.Filename(key))
	return !os.IsNotExist(err)
}

// Store saves value at key.
func (fs *FileStorage) Store(key string, value []byte) error {
	filename := fs.Filename(key)
	err := os.MkdirAll(filepath.Dir(filename), 0700)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(filename, value, 0600)
}

// Load retrieves the value at key.
func (fs *FileStorage) Load(key string) ([]byte, error) {
	contents, err := ioutil.ReadFile(fs.Filename(key))
	if os.IsNotExist(err) {
		return nil, ErrNotExist(err)
	}
	return contents, nil
}

// Delete deletes the value at key.
// TODO: Delete any empty folders caused by this operation
func (fs *FileStorage) Delete(key string) error {
	err := os.Remove(fs.Filename(key))
	if os.IsNotExist(err) {
		return ErrNotExist(err)
	}
	return err
}

// List returns all keys that match prefix.
func (fs *FileStorage) List(prefix string, recursive bool) ([]string, error) {
	var keys []string
	walkPrefix := fs.Filename(prefix)

	err := filepath.Walk(walkPrefix, func(fpath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info == nil {
			return fmt.Errorf("%s: file info is nil", fpath)
		}
		if fpath == walkPrefix {
			return nil
		}

		suffix, err := filepath.Rel(walkPrefix, fpath)
		if err != nil {
			return fmt.Errorf("%s: could not make path relative: %v", fpath, err)
		}
		keys = append(keys, path.Join(prefix, suffix))

		if !recursive && info.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})

	return keys, err
}

// Stat returns information about key.
func (fs *FileStorage) Stat(key string) (KeyInfo, error) {
	fi, err := os.Stat(fs.Filename(key))
	if os.IsNotExist(err) {
		return KeyInfo{}, ErrNotExist(err)
	}
	if err != nil {
		return KeyInfo{}, err
	}
	return KeyInfo{
		Key:        key,
		Modified:   fi.ModTime(),
		Size:       fi.Size(),
		IsTerminal: !fi.IsDir(),
	}, nil
}

// Filename returns the key as a path on the file
// system prefixed by fs.Path.
func (fs *FileStorage) Filename(key string) string {
	return filepath.Join(fs.Path, filepath.FromSlash(key))
}

// homeDir returns the best guess of the current user's home
// directory from environment variables. If unknown, "." (the
// current directory) is returned instead.
func homeDir() string {
	home := os.Getenv("HOME")
	if home == "" && runtime.GOOS == "windows" {
		drive := os.Getenv("HOMEDRIVE")
		path := os.Getenv("HOMEPATH")
		home = drive + path
		if drive == "" || path == "" {
			home = os.Getenv("USERPROFILE")
		}
	}
	if home == "" {
		home = "."
	}
	return home
}

func dataDir() string {
	baseDir := filepath.Join(homeDir(), ".local", "share")
	if xdgData := os.Getenv("XDG_DATA_HOME"); xdgData != "" {
		baseDir = xdgData
	}
	return filepath.Join(baseDir, "certmagic")
}

// Lock obtains a lock named by the given key. It blocks
// until the lock can be obtained or an error is returned.
func (fs *FileStorage) Lock(key string) error {
	// can't defer the unlock because we might have
	// to Wait() for a while before returning, so we're
	// careful to unlock at all the right places
	fs.fileStorageNameLocksMu.Lock()

	if fs.fileStorageNameLocks == nil {
		fs.fileStorageNameLocks = make(map[string]*fileStorageWaiter)
	}

	// see if lock already exists within this process - allows
	// for faster unlocking since we don't have to poll the disk
	fw, ok := fs.fileStorageNameLocks[key]
	if ok {
		// lock already created within process, let caller wait on it
		fs.fileStorageNameLocksMu.Unlock()
		fw.Wait()
		return nil
	}

	// attempt to persist lock to disk by creating lock file

	lockDir := fs.lockDir()
	// since there isn't already a waiter for the lock, make one
	fw = &fileStorageWaiter{
		key:      key,
		filename: filepath.Join(lockDir, StorageKeys.safe(key)+".lock"),
		wg:       new(sync.WaitGroup),
	}
	fw.wg.Add(1)
	fs.fileStorageNameLocks[key] = fw
	fs.fileStorageNameLocksMu.Unlock()

	for {
		// parent dir must exist
		if err := os.MkdirAll(lockDir, 0700); err != nil {
			return err
		}

		// create the file in a special mode such that an
		// error is returned if it already exists
		lf, err := os.OpenFile(fw.filename, os.O_CREATE|os.O_EXCL, 0644)
		switch {
		case err == nil:
			// cool, we got the lock right away
			lf.Close()
			return nil
		case os.IsExist(err):
			// another process has the lock
			info, err := os.Stat(fw.filename)
			switch {
			case err != nil:
			case fileLockIsStale(info):
				log.Printf("[INFO][%s] Lock for '%s' is stale; removing then retrying: %s",
					fs, key, fw.filename)
				os.Remove(fw.filename)
			default:
				time.Sleep(1 * time.Second)
			}
		default:
			// otherwise, this was some unexpected error

			// we called wg.Add(1) above but didn't actually acquire the lock
			fw.wg.Done()
			return err
		}
	}
}

// Unlock releases the lock for name.
func (fs *FileStorage) Unlock(key string) error {
	fs.fileStorageNameLocksMu.Lock()
	defer fs.fileStorageNameLocksMu.Unlock()

	fw, ok := fs.fileStorageNameLocks[key]
	if !ok {
		return fmt.Errorf("FileStorage: no lock to release for %s", key)
	}

	// remove lock file
	os.Remove(fw.filename)

	// clean up in memory
	fw.wg.Done()
	delete(fs.fileStorageNameLocks, key)

	return nil
}

// UnlockAllObtained removes all locks obtained
// by this instance of fs.
func (fs *FileStorage) UnlockAllObtained() {
	if fs.fileStorageNameLocks == nil {
		fs.fileStorageNameLocks = make(map[string]*fileStorageWaiter)
	}
	for key, fw := range fs.fileStorageNameLocks {
		err := fs.Unlock(fw.key)
		if err != nil {
			log.Printf("[ERROR][%s] Releasing obtained lock for %s: %v", fs, key, err)
		}
	}
}

func (fs *FileStorage) lockFileStale(filename string) bool {
	info, err := os.Stat(filename)
	if err != nil {
		return true // no good way to handle this, really...
	}
	return fileLockIsStale(info)
}

func (fs *FileStorage) lockDir() string {
	return filepath.Join(fs.Path, "locks")
}

func (fs *FileStorage) String() string {
	return "FileStorage:" + fs.Path
}

// fileStorageWaiter waits for a file to disappear; it
// polls the file system to check for the existence of
// a file. It also uses a WaitGroup to optimize the
// polling in the case when this process is the only
// one waiting. (Other processes that are waiting for
// the lock will still block, but must wait for the
// polling to get their answer.)
type fileStorageWaiter struct {
	key      string
	filename string
	wg       *sync.WaitGroup
}

// Wait waits until the lock at fw.filename is
// released or until it becomes stale.
func (fw *fileStorageWaiter) Wait() {
	start := time.Now()
	fw.wg.Wait()
	for time.Since(start) < staleLockDuration {
		info, err := os.Stat(fw.filename)
		if err != nil {
			return
		}
		if fileLockIsStale(info) {
			return
		}
		time.Sleep(1 * time.Second)
	}
}

func fileLockIsStale(info os.FileInfo) bool {
	if info == nil {
		return true
	}
	return time.Since(info.ModTime()) > staleLockDuration
}

var _ Storage = (*FileStorage)(nil)

// staleLockDuration is the length of time
// before considering a lock to be stale.
const staleLockDuration = 2 * time.Hour
