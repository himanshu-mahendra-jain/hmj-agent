package tools

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"os"

	"hmj-agent/internal/chat"
)

const (
	// maxImageSide is the longest side of a picture read_file sends; a larger one is scaled
	// down. Anthropic accepts at most 2000px a side once a request holds over 20 pictures.
	maxImageSide = 2000
	// maxImageBytes is the largest picture read_file sends: 5MB once base64-encoded, the limit
	// on Bedrock and Vertex (the Anthropic API takes 10MB).
	maxImageBytes = 3_750_000
	// maxImagePixels bounds the pictures read_file decodes to scale down; decoding takes 4
	// bytes a pixel.
	maxImagePixels = 50_000_000
	// maxImageFileBytes is the largest picture file read_file reads into memory.
	maxImageFileBytes = 64 * 1024 * 1024
)

// imageMediaTypes maps the image kinds read_file can send to their media types.
var imageMediaTypes = map[string]string{"PNG image": "image/png", "JPEG image": "image/jpeg", "GIF image": "image/gif", "WebP image": "image/webp"}

// readImage turns the picture at absPath, of the given kind, into what read_file sends: the
// stored picture and a line saying what it is and how it was sent. It fails when the picture
// cannot be read, scaled or stored; read_file then describes the file instead. The size limits
// are checked from the file's header before the whole file is read.
func readImage(path, absPath, kind string, size int64) (Result, error) {
	mediaType := imageMediaTypes[kind]
	head, err := readHead(absPath, 256*1024) // the header, and any EXIF before it
	if err != nil {
		return Result{}, err
	}
	w, h, err := imageSize(head, mediaType)
	if err != nil {
		return Result{}, fmt.Errorf("reading its size: %w", err)
	}
	about := fmt.Sprintf("%s: %s, %d×%d, %s", path, kind, w, h, formatSize(int(size)))

	fits := w <= maxImageSide && h <= maxImageSide && size <= maxImageBytes
	switch {
	case fits:
	case mediaType == "image/webp":
		return Result{}, fmt.Errorf("a WebP picture over %dpx a side or %s is not scaled (no WebP decoder); convert it with run_command, e.g. dwebp", maxImageSide, formatSize(maxImageBytes))
	case w*h > maxImagePixels || size > maxImageFileBytes:
		return Result{}, fmt.Errorf("it is over %d megapixels or %s; scale it down with run_command first", maxImagePixels/1_000_000, formatSize(maxImageFileBytes))
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return Result{}, err
	}
	sendW, sendH := w, h
	if !fits {
		data, mediaType, sendW, sendH, err = scaleImage(data, mediaType)
		if err != nil {
			return Result{}, err
		}
		about += fmt.Sprintf("; shown scaled to %d×%d as %s (%s)", sendW, sendH, imageFormatName(mediaType), formatSize(len(data)))
	}
	img, err := chat.StoreImage(data, mediaType, sendW, sendH, path)
	if err != nil {
		return Result{}, err
	}
	return Result{Text: about + ".", Images: []chat.Image{img}}, nil
}

// imageSize returns a picture's width and height from its header.
func imageSize(data []byte, mediaType string) (int, int, error) {
	if mediaType == "image/webp" {
		return webpSize(data)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	return cfg.Width, cfg.Height, err
}

// webpSize reads a WebP picture's size from its first chunk: VP8X (extended), VP8L
// (lossless) or "VP8 " (lossy).
func webpSize(d []byte) (int, int, error) {
	if len(d) >= 30 && string(d[12:16]) == "VP8X" {
		return 1 + (int(d[24]) | int(d[25])<<8 | int(d[26])<<16), 1 + (int(d[27]) | int(d[28])<<8 | int(d[29])<<16), nil
	}
	if len(d) >= 25 && string(d[12:16]) == "VP8L" && d[20] == 0x2f {
		b := binary.LittleEndian.Uint32(d[21:25])
		return int(b&0x3fff) + 1, int(b>>14&0x3fff) + 1, nil
	}
	if len(d) >= 30 && string(d[12:16]) == "VP8 " && d[23] == 0x9d && d[24] == 0x01 && d[25] == 0x2a {
		return int(binary.LittleEndian.Uint16(d[26:28]) & 0x3fff), int(binary.LittleEndian.Uint16(d[28:30]) & 0x3fff), nil
	}
	return 0, 0, fmt.Errorf("unrecognised WebP header")
}

// scaleImage decodes a picture and scales it to fit maxImageSide, keeping its aspect ratio. A
// photo (JPEG) stays JPEG; anything else becomes PNG, or JPEG when the PNG would be over
// maxImageBytes.
func scaleImage(data []byte, mediaType string) ([]byte, string, int, int, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("decoding it: %w", err)
	}
	dst := downscale(src, maxImageSide)
	var buf bytes.Buffer
	if mediaType != "image/jpeg" {
		if err := png.Encode(&buf, dst); err != nil {
			return nil, "", 0, 0, err
		}
		if buf.Len() <= maxImageBytes {
			return buf.Bytes(), "image/png", dst.Bounds().Dx(), dst.Bounds().Dy(), nil
		}
		buf.Reset()
	}
	for _, quality := range []int{90, 75, 60} {
		buf.Reset()
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: quality}); err != nil {
			return nil, "", 0, 0, err
		}
		if buf.Len() <= maxImageBytes {
			break
		}
	}
	if buf.Len() > maxImageBytes {
		return nil, "", 0, 0, fmt.Errorf("it is still over %s after scaling", formatSize(maxImageBytes))
	}
	return buf.Bytes(), "image/jpeg", dst.Bounds().Dx(), dst.Bounds().Dy(), nil
}

// downscale returns src scaled so neither side exceeds maxSide, each output pixel the average
// of the source pixels it covers, or src itself when it already fits.
func downscale(src image.Image, maxSide int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= maxSide && sh <= maxSide {
		return src
	}
	dw, dh := maxSide, max(1, (sh*maxSide+sw/2)/sw)
	if sh > sw {
		dw, dh = max(1, (sw*maxSide+sh/2)/sh), maxSide
	}
	rgba := image.NewRGBA(image.Rect(0, 0, sw, sh))
	draw.Draw(rgba, rgba.Bounds(), src, b.Min, draw.Src)
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range dh {
		y0, y1 := y*sh/dh, max((y+1)*sh/dh, y*sh/dh+1)
		for x := range dw {
			x0, x1 := x*sw/dw, max((x+1)*sw/dw, x*sw/dw+1)
			var r, g, bl, a, n uint32
			for sy := y0; sy < y1; sy++ {
				row := rgba.Pix[sy*rgba.Stride+x0*4 : sy*rgba.Stride+x1*4]
				for i := 0; i < len(row); i += 4 {
					r += uint32(row[i])
					g += uint32(row[i+1])
					bl += uint32(row[i+2])
					a += uint32(row[i+3])
					n++
				}
			}
			o := y*dst.Stride + x*4
			dst.Pix[o], dst.Pix[o+1], dst.Pix[o+2], dst.Pix[o+3] = uint8((r+n/2)/n), uint8((g+n/2)/n), uint8((bl+n/2)/n), uint8((a+n/2)/n)
		}
	}
	return dst
}

// imageFormatName names a media type for the line read_file returns.
func imageFormatName(mediaType string) string {
	if mediaType == "image/jpeg" {
		return "JPEG"
	}
	return "PNG"
}
