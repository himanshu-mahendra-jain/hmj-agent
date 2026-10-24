package chat

import (
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Image is a picture a tool result carries for the model to see. Its bytes live in the image
// store under File, a name made of their SHA-256, so the conversation file holds only this
// reference and a picture read twice is stored once.
type Image struct {
	File      string `json:"file"`
	MediaType string `json:"media_type"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	// Source is where the picture came from, such as the path read_file was given; the note
	// that stands in for a picture that cannot be sent names it.
	Source string `json:"source,omitempty"`
}

// maxStoredImages and maxImageStoreBytes bound the image store; the oldest go first. A
// conversation whose pictures were pruned gets a note in their place. Tests lower them.
var (
	maxStoredImages    = 500
	maxImageStoreBytes = int64(1024 * 1024 * 1024)
)

var (
	imageStoreMu  sync.Mutex
	imageStoreDir string
)

// SetImageDir sets the directory that holds the images tool results carry. Without one,
// StoreImage fails and tools describe a picture in text instead.
func SetImageDir(dir string) {
	imageStoreMu.Lock()
	defer imageStoreMu.Unlock()
	imageStoreDir = dir
}

// imageExtensions maps the media types the providers accept to file extensions.
var imageExtensions = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}

// StoreImage keeps data, a picture of the given media type and size, in the image store and
// returns its reference.
func StoreImage(data []byte, mediaType string, width, height int, source string) (Image, error) {
	ext, ok := imageExtensions[mediaType]
	if !ok {
		return Image{}, fmt.Errorf("unsupported image type %s", mediaType)
	}
	imageStoreMu.Lock()
	defer imageStoreMu.Unlock()
	if imageStoreDir == "" {
		return Image{}, errors.New("no image store is set")
	}
	if err := os.MkdirAll(imageStoreDir, 0o700); err != nil {
		return Image{}, err
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:]) + ext
	path := filepath.Join(imageStoreDir, name)
	if _, err := os.Stat(path); err != nil {
		tmp, err := os.CreateTemp(imageStoreDir, ".tmp-*")
		if err != nil {
			return Image{}, err
		}
		_, werr := tmp.Write(data)
		cerr := tmp.Close()
		if err := cmp.Or(werr, cerr, os.Rename(tmp.Name(), path)); err != nil {
			return Image{}, errors.Join(err, removeIfPresent(tmp.Name()))
		}
		pruneImages(imageStoreDir, name)
	}
	return Image{File: name, MediaType: mediaType, Width: width, Height: height, Source: source}, nil
}

// pruneImages removes the oldest images past maxStoredImages or maxImageStoreBytes, keeping
// the one just stored.
func pruneImages(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type file struct {
		name string
		size int64
		mod  int64
	}
	var files []file
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		files = append(files, file{e.Name(), info.Size(), info.ModTime().UnixNano()})
		total += info.Size()
	}
	slices.SortFunc(files, func(a, b file) int { return cmp.Compare(a.mod, b.mod) })
	count := len(files)
	for _, f := range files {
		if count <= maxStoredImages && total <= maxImageStoreBytes {
			break
		}
		if f.name != keep && os.Remove(filepath.Join(dir, f.name)) == nil {
			count--
			total -= f.size
		}
	}
}

// load returns the picture's bytes from the image store.
func (img Image) load() ([]byte, error) {
	imageStoreMu.Lock()
	dir := imageStoreDir
	imageStoreMu.Unlock()
	if dir == "" || img.File == "" || filepath.Base(img.File) != img.File {
		return nil, errors.New("no image store is set")
	}
	return os.ReadFile(filepath.Join(dir, img.File))
}

// base64Data returns the picture's bytes as standard base64, or a note to send in its place.
func (img Image) base64Data() (string, string) {
	data, err := img.load()
	if err != nil {
		return "", img.Note("its file is gone from the image store")
	}
	return base64.StdEncoding.EncodeToString(data), ""
}

// Note is the text sent in place of a picture that cannot be sent, saying which picture it was
// and why.
func (img Image) Note(why string) string {
	return fmt.Sprintf("[image %s (%d×%d %s) not shown: %s]", cmp.Or(img.Source, img.File), img.Width, img.Height, img.MediaType, why)
}

// removeIfPresent removes path; one that is already gone is not an error.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
