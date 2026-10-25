package tools

import (
	"bytes"
	"cmp"
	"fmt"
	_ "image/gif" // registers GIF for image.DecodeConfig
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// binarySniffBytes is how much of a file's start read_file checks for binary content.
const binarySniffBytes = 8 * 1024

// binaryKind is a kind of file read_file recognises by its first bytes.
type binaryKind struct {
	magic string
	name  string
	// hint says how to look inside, with %s for the file's path.
	hint string
}

// binaryKinds are checked in order against a file's start.
var binaryKinds = []binaryKind{
	{"\x89PNG\r\n\x1a\n", "PNG image", ""},
	{"\xff\xd8\xff", "JPEG image", ""},
	{"GIF87a", "GIF image", ""},
	{"GIF89a", "GIF image", ""},
	{"PK\x03\x04", "ZIP archive", "list its files with run_command: unzip -l %s"},
	{"PK\x05\x06", "empty ZIP archive", ""},
	{"\x7fELF", "ELF binary (executable or shared library)", "inspect it with run_command: file %s; readelf -h %s; nm -D --defined-only %s"},
	{"%PDF-", "PDF document", "extract its text with run_command, if poppler is installed: pdftotext %s - | head -200"},
	{"SQLite format 3\x00", "SQLite database", "query it with run_command: sqlite3 %s .tables"},
	{"\xca\xfe\xba\xbe", "Java class file", "disassemble it with run_command: javap -c -p %s"},
	{"dex\n", "Android DEX file", "dump it with run_command: dexdump -h %s (Android build tools)"},
	{"\x1f\x8b", "gzip archive", "list or read it with run_command: tar -tzf %s, or zcat %s | head"},
	{"\xfd7zXZ\x00", "xz archive", "list it with run_command: tar -tJf %s"},
	{"7z\xbc\xaf\x27\x1c", "7-Zip archive", ""},
	{"MZ", "Windows executable", "inspect it with run_command: file %s"},
	{"\xff\xfe", "UTF-16 text (little-endian)", "convert it with run_command: iconv -f UTF-16 -t UTF-8 %s | head -200"},
	{"\xfe\xff", "UTF-16 text (big-endian)", "convert it with run_command: iconv -f UTF-16 -t UTF-8 %s | head -200"},
}

// zipKinds names ZIP archives by extension, since APKs, JARs and Office files are all ZIPs.
var zipKinds = map[string]string{
	".apk": "Android APK (ZIP archive)", ".aab": "Android App Bundle (ZIP archive)", ".aar": "Android library (ZIP archive)",
	".jar": "Java archive (ZIP)", ".war": "Java web archive (ZIP)", ".whl": "Python wheel (ZIP archive)",
	".docx": "Word document (ZIP archive)", ".xlsx": "Excel workbook (ZIP archive)", ".pptx": "PowerPoint deck (ZIP archive)",
}

// describeBinary returns read_file's answer for a file that is not text: what it is, its size
// and how to look inside, judged from head, the file's first bytes. It returns false for text.
func describeBinary(path, absPath string, head []byte, size int64) (string, bool) {
	kind, ok := detectBinary(head)
	if !ok {
		return "", false
	}
	name := kind.name
	if z, ok := zipKinds[strings.ToLower(filepath.Ext(path))]; ok && strings.HasPrefix(kind.magic, "PK") {
		name = z
	}
	details := []string{name}
	if strings.HasSuffix(kind.name, " image") {
		if data, err := readHead(absPath, 256*1024); err == nil { // the header, and any EXIF before it
			if w, h, err := imageSize(data, imageMediaTypes[kind.name]); err == nil {
				details = append(details, fmt.Sprintf("%d×%d", w, h))
			}
		}
	}
	details = append(details, formatSize(int(size)))
	desc := fmt.Sprintf("%s is not a text file: %s.", path, strings.Join(details, ", "))
	if strings.HasSuffix(kind.name, " image") {
		return desc, true // its bytes would tell the model nothing more
	}
	if note, fixable := misnamedImageNote(path, head); note != "" {
		desc += " " + note
		if fixable {
			return desc, true
		}
	}
	hint := cmp.Or(kind.hint, "dump its start with run_command: xxd %s | head -40")
	return desc + " read_file shows only text and pictures; " + strings.ReplaceAll(hint, "%s", shellQuote(path)), true
}

// pictureExts maps a picture's file extension to its kind and the signatures it starts with.
var pictureExts = map[string]struct {
	kind  string
	magic []string
}{
	".png":  {"PNG image", []string{"\x89PNG\r\n\x1a\n"}},
	".jpg":  {"JPEG image", []string{"\xff\xd8\xff"}},
	".jpeg": {"JPEG image", []string{"\xff\xd8\xff"}},
	".gif":  {"GIF image", []string{"GIF87a", "GIF89a"}},
	".webp": {"WebP image", []string{"RIFF"}},
}

// misnamedImageNote explains a file named as a picture that does not start like one, as when
// a command's warning went into the file ahead of the picture (adb's screencap on a device with
// several displays). When the picture's signature follows in head, the note says where and
// how to cut what comes before it, and fixable is true.
func misnamedImageNote(path string, head []byte) (note string, fixable bool) {
	ext := strings.ToLower(filepath.Ext(path))
	pic, ok := pictureExts[ext]
	if !ok {
		return "", false
	}
	note = fmt.Sprintf("Its %s name says %s, but it starts with %s.", ext, pic.kind, quoteStart(head))
	for _, m := range pic.magic {
		if at := bytes.Index(head, []byte(m)); at > 0 {
			fixed := strings.TrimSuffix(path, filepath.Ext(path)) + ".fixed" + filepath.Ext(path)
			return note + fmt.Sprintf(" The %s starts at byte %d, after what looks like a command's message written into the file; cut that with run_command: tail -c +%d %s > %s, then read_file the new file.",
				pic.kind, at, at+1, shellQuote(path), shellQuote(fixed)), true
		}
	}
	return note, false
}

// quoteStart quotes the printable text head starts with, up to 40 characters.
func quoteStart(head []byte) string {
	end, n := 0, 0
	for end < len(head) && n < 40 {
		r, size := utf8.DecodeRune(head[end:])
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			break
		}
		end += size
		n++
	}
	if end == 0 {
		return "no text"
	}
	if n == 40 {
		return strconv.Quote(string(head[:end]) + "…")
	}
	return strconv.Quote(string(head[:end]))
}

// detectBinary reports whether head is the start of a binary file, and which kind when known.
// A file is binary when it starts with a known signature, holds a NUL byte, or is not UTF-8
// and is mostly control characters; text in a legacy 8-bit encoding stays text.
func detectBinary(head []byte) (binaryKind, bool) {
	for _, k := range binaryKinds {
		if bytes.HasPrefix(head, []byte(k.magic)) {
			return k, true
		}
	}
	if len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP" {
		return binaryKind{magic: "RIFF", name: "WebP image"}, true
	}
	generic := binaryKind{name: "binary file"}
	if bytes.IndexByte(head, 0) >= 0 {
		return generic, true
	}
	valid := head
	for i := 0; i < utf8.UTFMax && len(valid) > 0 && !utf8.Valid(valid); i++ {
		valid = valid[:len(valid)-1] // a character cut at the end of head
	}
	if utf8.Valid(valid) {
		return binaryKind{}, false
	}
	control := 0
	for _, b := range head {
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' && b != '\f' && b != 0x1b {
			control++
		}
	}
	return generic, control*10 > len(head)
}

// readHead returns up to n bytes from the start of the file at absPath.
func readHead(absPath string, n int) ([]byte, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, n)
	m, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	return head[:m], nil
}

// shellQuote quotes s for a POSIX shell when it holds anything but safe characters.
func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+:@~", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
