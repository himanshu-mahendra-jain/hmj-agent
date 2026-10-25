package tools

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pngBytes encodes a w×h image as PNG.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestReadFileDescribesBinaryFiles checks that read_file answers a binary file with one line
// naming its kind, size and a way to look inside, rather than its bytes.
func TestReadFileDescribesBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	random := make([]byte, 4096)
	for i := range random {
		random[i] = byte(i*7 + i/3) // includes NUL bytes
	}
	for _, tc := range []struct {
		name    string
		content []byte
		want    []string
	}{
		{"shot.png", pngBytes(t, 30, 20), []string{"PNG image, 30×20, "}},
		{"photo.jpg", []byte("\xff\xd8\xff\xe0 not a full jpeg"), []string{": JPEG image, 20 bytes."}},
		{"app-debug.apk", []byte("PK\x03\x04\x14\x00rest of the archive"), []string{"Android APK (ZIP archive)", "unzip -l "}},
		{"lib.zip", []byte("PK\x03\x04\x14\x00"), []string{": ZIP archive, 6 bytes", "unzip -l "}},
		{"libnative.so", []byte("\x7fELF\x02\x01\x01\x00"), []string{"ELF binary", "readelf -h "}},
		{"spec.pdf", []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n"), []string{"PDF document", "pdftotext "}},
		{"app.db", []byte("SQLite format 3\x00\x10\x00"), []string{"SQLite database", "sqlite3 "}},
		{"Main.class", []byte("\xca\xfe\xba\xbe\x00\x00\x00\x41"), []string{"Java class file", "javap -c -p "}},
		{"notes.txt", []byte("\xff\xfeh\x00i\x00"), []string{"UTF-16 text (little-endian)", "iconv -f UTF-16"}},
		{"blob.dat", random, []string{"binary file, 4 KB", "xxd "}},
		{"it's here.bin", []byte("\x00\x01"), []string{`xxd 'it'\''s here.bin' | head -40`}},
		{"fold.png", append([]byte("[Warning] Multiple displays were found, but no display id was specified!\n\x00"), pngBytes(t, 4, 4)...), []string{
			`Its .png name says PNG image, but it starts with "[Warning] Multiple displays were found, …".`,
			"The PNG image starts at byte 74", "tail -c +75 fold.png > fold.fixed.png",
		}},
		{"noise.jpg", []byte("oops\x00\x01\x02"), []string{`Its .jpg name says JPEG image, but it starts with "oops".`, "read_file shows only text and pictures; ", "xxd "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(dir)
			if err := os.WriteFile(tc.name, tc.content, 0o644); err != nil {
				t.Fatal(err)
			}
			got := readArgs(t, map[string]any{"path": tc.name})
			if strings.Contains(got, "\n") || !strings.HasPrefix(got, tc.name+" is not a text file: ") {
				t.Errorf("not a one-line description: %q", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("no %q in %q", w, got)
				}
			}
		})
	}
}

// TestReadFileKeepsTextAsText checks files that must still read as text: UTF-8 with
// multi-byte characters, a character cut by the check's end, and Latin-1 text.
func TestReadFileKeepsTextAsText(t *testing.T) {
	dir := t.TempDir()
	cut := strings.Repeat("a", binarySniffBytes-1) + "é and more\n" // é straddles the checked bytes
	for name, content := range map[string]string{
		"utf8.kt":   "val greeting = \"héllo wörld 👋 — ✓\"\n\tprintln(greeting)\x1b[0m\n",
		"cut.txt":   cut,
		"latin1.c":  "/* caf\xe9 cr\xe8me */\nint main() { return 0; }\n",
		"ascii.txt": "plain\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := readArgs(t, map[string]any{"path": path, "numbered": false}); strings.Contains(got, "not a text file") {
			t.Errorf("%s read as binary: %q", name, got)
		}
	}
}

// TestReadFileDescribesALargeBinary checks that a binary file over read_file's size cap is
// still described, not refused as too large.
func TestReadFileDescribesALargeBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.apk")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("PK\x03\x04")
	if err := f.Truncate(MaxReadableFileSize * 3); err != nil { // sparse
		t.Fatal(err)
	}
	f.Close()
	if got := readArgs(t, map[string]any{"path": path}); !strings.Contains(got, "Android APK (ZIP archive), 30.0 MB") {
		t.Errorf("got %q", got)
	}
}
