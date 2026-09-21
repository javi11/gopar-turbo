package memfs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// RootDir returns a string representing a root directory. On
// Unix-like systems this is just /, but on Windows it may be C:\ or
// some other drive letter.
func RootDir() string {
	// This complexity is only for Windows, the only platform
	// which has the concept of a VolumeName, e.g. C:. We don't
	// care which drive the current working directory is on. On
	// all other platforms, volName is empty.
	wd, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	volName := filepath.VolumeName(filepath.Clean(wd))
	return volName + string(filepath.Separator)
}

func fileDataToAbsPaths(workingDir string, fileData map[string][]byte) map[string][]byte {
	newFileData := make(map[string][]byte)
	for path, data := range fileData {
		newFileData[toAbsPath(workingDir, path)] = data
	}
	return newFileData
}

func toAbsPath(workingDir, path string) string {
	if !filepath.IsAbs(workingDir) {
		panic("workingDir must be an absolute path")
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(workingDir, path)
}

// MemFS is a simple in-memory filesystem with a working
// directory. It's intended mainly for testing.
//
// A MemFS is safe for concurrent use: the file map is guarded by a mutex
// shared by every copy of the value, since repair commits and scans files
// from several goroutines.
type MemFS struct {
	workingDir string
	fileData   map[string][]byte
	mu         *sync.Mutex
}

// MakeMemFS makes a MemFS from the given working directory and file
// data.
func MakeMemFS(workingDir string, fileData map[string][]byte) MemFS {
	return MemFS{workingDir, fileDataToAbsPaths(workingDir, fileData), &sync.Mutex{}}
}

// ReadFile returns the data of the file at the given path, which may
// be absolute or relative (to the working directory). If the file
// doesn't exist, os.ErrNotExist is returned.
func (fs MemFS) ReadFile(path string) (data []byte, err error) {
	absPath := toAbsPath(fs.workingDir, path)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if data, ok := fs.fileData[absPath]; ok {
		return data, nil
	}
	return nil, os.ErrNotExist
}

// FindWithPrefixAndSuffix returns all files whose path matches the
// given prefix and suffix, in no particular order. The prefix may be
// absolute or relative (to the working directory).
func (fs MemFS) FindWithPrefixAndSuffix(prefix, suffix string) ([]string, error) {
	absPrefix := toAbsPath(fs.workingDir, prefix)
	var matches []string
	for _, filename := range fs.Paths() {
		if len(filename) >= len(absPrefix)+len(suffix) && strings.HasPrefix(filename, absPrefix) && strings.HasSuffix(filename, suffix) {
			matches = append(matches, filename)
		}
	}
	return matches, nil
}

// WriteFile sets the data of the file at the given path, which may be
// absolute or relative (to the working directory). The file may or
// may not already exist.
func (fs MemFS) WriteFile(path string, data []byte) error {
	absPath := toAbsPath(fs.workingDir, path)
	// Copy, so the caller may reuse its buffer afterwards exactly as it
	// could with os.WriteFile. Storing the caller's slice let a later
	// mutation silently rewrite an already-written file.
	stored := make([]byte, len(data))
	copy(stored, data)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.fileData[absPath] = stored
	return nil
}

// FileCount returns the total number of files.
func (fs MemFS) FileCount() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.fileData)
}

// Paths returns a list of absolute paths of files in fs in no particular order.
func (fs MemFS) Paths() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var paths []string
	for path := range fs.fileData {
		paths = append(paths, path)
	}
	return paths
}

// RemoveFile removes the file at the given path, which may be
// absolute or relative (to the working directory). The removed data
// is returned, or os.ErrNotExist if it doesn't exist.
func (fs MemFS) RemoveFile(path string) ([]byte, error) {
	absPath := toAbsPath(fs.workingDir, path)
	data, err := fs.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	delete(fs.fileData, absPath)
	return data, nil
}

// MoveFile moves the file at oldPath to newPath. oldPath and newPath
// may be either absolute or relative (to the working directory). If
// the file doesn't exist at oldPath, os.ErrNotExist is returned.
func (fs MemFS) MoveFile(oldPath, newPath string) error {
	data, err := fs.RemoveFile(oldPath)
	if err != nil {
		return err
	}
	// Shouldn't return an error.
	return fs.WriteFile(newPath, data)
}

// OpenRead returns a reader over the file at path, its size in bytes, and a
// close function. The close function is a no-op: nothing is held open.
func (fs MemFS) OpenRead(path string) (io.ReaderAt, int64, func() error, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		return nil, 0, nil, err
	}
	return bytes.NewReader(data), int64(len(data)), func() error { return nil }, nil
}

// memWriter writes into a fixed-size buffer that replaces the file's contents
// on close.
type memWriter struct {
	fs   MemFS
	path string
	data []byte
}

func (w *memWriter) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 || off+int64(len(p)) > int64(len(w.data)) {
		return 0, errors.New("memfs: write out of range")
	}
	copy(w.data[off:], p)
	return len(p), nil
}

// OpenPatch starts a size-byte rewrite of path from a copy of its current
// contents, mirroring the clone-based patch path of the real filesystem.
// It fails if path does not exist.
func (fs MemFS) OpenPatch(path string, size int64) (io.WriterAt, func(commit bool) error, error) {
	old, err := fs.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	data := make([]byte, size)
	copy(data, old)
	w := &memWriter{fs: fs, path: path, data: data}
	return w, func(commit bool) error {
		if !commit {
			return nil
		}
		return fs.WriteFile(w.path, w.data)
	}, nil
}

// OpenWrite starts a size-byte write of path. Writes land in a private buffer;
// close(true) installs it as the file's contents, close(false) discards it. A
// reader of the old contents is unaffected until the commit.
func (fs MemFS) OpenWrite(path string, size int64) (io.WriterAt, func(commit bool) error, error) {
	w := &memWriter{fs: fs, path: path, data: make([]byte, size)}
	return w, func(commit bool) error {
		if !commit {
			return nil
		}
		return fs.WriteFile(w.path, w.data)
	}, nil
}
