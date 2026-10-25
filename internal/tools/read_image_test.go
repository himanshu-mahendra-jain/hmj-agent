package tools

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"hmj-agent/v2/internal/chat"
)

// useImageStore points the image store at a fresh directory and returns it.
func useImageStore(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	chat.SetImageDir(dir)
	t.Cleanup(func() { chat.SetImageDir("") })
	return dir
}

// readPicture writes data to name in a fresh directory and reads it with read_file.
func readPicture(t *testing.T, name string, data []byte) Result {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := (&ReadFileTool{}).ExecuteWithImages(context.Background(), mustJSON(t, map[string]any{"path": path}))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// stored returns the bytes the image store holds for img.
func stored(t *testing.T, dir string, img chat.Image) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, img.File))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// webpHeader returns the start of an extended (VP8X) WebP file of the given size.
func webpHeader(w, h int) []byte {
	d := make([]byte, 30)
	copy(d, "RIFF\x16\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00")
	d[24], d[25], d[26] = byte(w-1), byte((w-1)>>8), byte((w-1)>>16)
	d[27], d[28], d[29] = byte(h-1), byte((h-1)>>8), byte((h-1)>>16)
	return d
}

// TestReadFileSendsASmallPictureAsItIs checks that a picture within the limits is sent
// byte for byte, with a line naming its kind and size.
func TestReadFileSendsASmallPictureAsItIs(t *testing.T) {
	dir := useImageStore(t)
	var gifBuf bytes.Buffer
	gif.Encode(&gifBuf, image.NewPaletted(image.Rect(0, 0, 4, 3), color.Palette{color.Black, color.White}), nil)
	for _, tc := range []struct {
		name, mediaType, about string
		data                   []byte
	}{
		{"shot.png", "image/png", "PNG image, 30×20", pngBytes(t, 30, 20)},
		{"anim.gif", "image/gif", "GIF image, 4×3", gifBuf.Bytes()},
		{"photo.webp", "image/webp", "WebP image, 300×200", webpHeader(300, 200)},
	} {
		res := readPicture(t, tc.name, tc.data)
		if len(res.Images) != 1 {
			t.Fatalf("%s: %d images, text %q", tc.name, len(res.Images), res.Text)
		}
		img := res.Images[0]
		if img.MediaType != tc.mediaType || !bytes.Equal(stored(t, dir, img), tc.data) || !strings.Contains(res.Text, tc.name+": "+tc.about+", ") || strings.Contains(res.Text, "scaled") {
			t.Errorf("%s: got %+v, text %q", tc.name, img, res.Text)
		}
	}
}

// TestReadFileScalesALargePicture checks that a picture over maxImageSide is scaled to fit,
// keeping its aspect ratio and, for a photo, its format.
func TestReadFileScalesALargePicture(t *testing.T) {
	dir := useImageStore(t)
	photo := image.NewRGBA(image.Rect(0, 0, 3000, 1500))
	var jpg bytes.Buffer
	jpeg.Encode(&jpg, photo, nil)
	for _, tc := range []struct {
		name          string
		data          []byte
		mediaType     string
		width, height int
		about         string
	}{
		{"phone.png", pngBytes(t, 1080, 2400), "image/png", 900, 2000, "PNG image, 1080×2400, "},
		{"wide.jpg", jpg.Bytes(), "image/jpeg", 2000, 1000, "JPEG image, 3000×1500, "},
	} {
		res := readPicture(t, tc.name, tc.data)
		if len(res.Images) != 1 {
			t.Fatalf("%s: no image, text %q", tc.name, res.Text)
		}
		img := res.Images[0]
		cfg, format, err := image.DecodeConfig(bytes.NewReader(stored(t, dir, img)))
		if err != nil || "image/"+format != tc.mediaType || cfg.Width != tc.width || cfg.Height != tc.height ||
			img.Width != tc.width || img.Height != tc.height || img.MediaType != tc.mediaType {
			t.Errorf("%s: sent %s %d×%d (%v), recorded %+v; want %s %d×%d", tc.name, format, cfg.Width, cfg.Height, err, img, tc.mediaType, tc.width, tc.height)
		}
		if !strings.Contains(res.Text, tc.about) || !strings.Contains(res.Text, "shown scaled to ") {
			t.Errorf("%s: text %q", tc.name, res.Text)
		}
	}
}

// TestDownscaleAveragesPixels checks the scaler: each output pixel is the average of the
// source pixels it covers.
func TestDownscaleAveragesPixels(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 2))
	for x := range 4 {
		for y := range 2 {
			v := uint8(0)
			if x%2 == 1 {
				v = 200
			}
			src.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	dst := downscale(src, 2).(*image.RGBA)
	if b := dst.Bounds(); b.Dx() != 2 || b.Dy() != 1 {
		t.Fatalf("size %v, want 2×1", b)
	}
	for x := range 2 {
		if c := dst.RGBAAt(x, 0); c != (color.RGBA{100, 100, 100, 255}) {
			t.Errorf("pixel %d = %v, want the average 100", x, c)
		}
	}
	if downscale(src, 4) != image.Image(src) {
		t.Error("a picture that fits was copied")
	}
}

// TestReadFileDescribesAPictureItCannotSend checks the fallback to the one-line description,
// with the reason, when there is no image store, the picture is corrupt, or it is a large WebP.
func TestReadFileDescribesAPictureItCannotSend(t *testing.T) {
	res := readPicture(t, "a.png", pngBytes(t, 3, 3))
	if len(res.Images) != 0 || !strings.Contains(res.Text, "PNG image, 3×3") || !strings.HasSuffix(res.Text, "It cannot be shown: no image store is set.") {
		t.Errorf("no store: %+v", res)
	}
	useImageStore(t)
	res = readPicture(t, "broken.png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 40)...))
	if len(res.Images) != 0 || !strings.Contains(res.Text, "It cannot be shown: reading its size: ") {
		t.Errorf("corrupt: %+v", res)
	}
	res = readPicture(t, "big.webp", webpHeader(4000, 3000))
	if len(res.Images) != 0 || !strings.Contains(res.Text, "WebP image, 4000×3000") || !strings.Contains(res.Text, "dwebp") {
		t.Errorf("large WebP: %+v", res)
	}
	// Only the header: the size check comes before the file is read or decoded.
	ihdr := binary.BigEndian.AppendUint32([]byte("IHDR"), 20000)
	ihdr = append(binary.BigEndian.AppendUint32(ihdr, 20000), 8, 6, 0, 0, 0)
	huge := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0d"), ihdr...)
	huge = binary.BigEndian.AppendUint32(huge, crc32.ChecksumIEEE(ihdr))
	res = readPicture(t, "huge.png", huge)
	if len(res.Images) != 0 || !strings.Contains(res.Text, "PNG image, 20000×20000") || !strings.Contains(res.Text, "over 50 megapixels") {
		t.Errorf("huge: %+v", res)
	}
}

// TestWebpSize checks the size read from each kind of WebP header.
func TestWebpSize(t *testing.T) {
	lossless := make([]byte, 25)
	copy(lossless, "RIFF\x00\x00\x00\x00WEBPVP8L")
	lossless[20] = 0x2f
	binary.LittleEndian.PutUint32(lossless[21:], uint32(640-1)|uint32(480-1)<<14)
	lossy := make([]byte, 30)
	copy(lossy, "RIFF\x00\x00\x00\x00WEBPVP8 ")
	copy(lossy[23:], "\x9d\x01\x2a")
	binary.LittleEndian.PutUint16(lossy[26:], 1920)
	binary.LittleEndian.PutUint16(lossy[28:], 1080)
	for name, tc := range map[string]struct {
		data []byte
		w, h int
	}{"extended": {webpHeader(70000, 2), 70000, 2}, "extended, low bytes 0xff": {webpHeader(1024, 2048), 1024, 2048}, "lossless": {lossless, 640, 480}, "lossy": {lossy, 1920, 1080}} {
		if w, h, err := webpSize(tc.data); err != nil || w != tc.w || h != tc.h {
			t.Errorf("%s: %d×%d, %v; want %d×%d", name, w, h, err, tc.w, tc.h)
		}
	}
	if _, _, err := webpSize([]byte("RIFF\x00\x00\x00\x00WEBPXXXX")); err == nil {
		t.Error("an unknown chunk gave a size")
	}
}
