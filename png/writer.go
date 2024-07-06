// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package png

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"strconv"

	"github.com/rmamba/image"
	"github.com/rmamba/image/color"
)

// Encoder configures encoding PNG images.
type Encoder struct {
	CompressionLevel CompressionLevel

	// BufferPool optionally specifies a buffer pool to get temporary
	// EncoderBuffers when encoding an image.
	BufferPool EncoderBufferPool
}

// EncoderBufferPool is an interface for getting and returning temporary
// instances of the EncoderBuffer struct. This can be used to reuse buffers
// when encoding multiple images.
type EncoderBufferPool interface {
	Get() *EncoderBuffer
	Put(*EncoderBuffer)
}

// EncoderBuffer holds the buffers used for encoding PNG images.
type EncoderBuffer encoder

type encoder struct {
	enc     *Encoder
	w       io.Writer
	m       image.Image
	cb      int
	err     error
	header  [8]byte
	footer  [4]byte
	tmp     [4 * 256]byte
	cr      [nFilter][]uint8
	pr      []uint8
	zw      *zlib.Writer
	zwLevel int
	bw      *bufio.Writer
}

type CompressionLevel int

const (
	DefaultCompression CompressionLevel = 0
	NoCompression      CompressionLevel = -1
	BestSpeed          CompressionLevel = -2
	BestCompression    CompressionLevel = -3

	// Positive CompressionLevel values are reserved to mean a numeric zlib
	// compression level, although that is not implemented yet.
)

type opaquer interface {
	Opaque() bool
}

// Returns whether or not the image is fully opaque.
func opaque(m image.Image) bool {
	if o, ok := m.(opaquer); ok {
		return o.Opaque()
	}
	b := m.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			_, _, _, a := m.At(x, y).RGBA()
			if a != 0xffff {
				return false
			}
		}
	}
	return true
}

// The absolute value of a byte interpreted as a signed int8.
func abs8(d uint8) int {
	if d < 128 {
		return int(d)
	}
	return 256 - int(d)
}

func (e *encoder) writeChunk(b []byte, name string) {
	if e.err != nil {
		return
	}
	n := uint32(len(b))
	if int(n) != len(b) {
		e.err = UnsupportedError(name + " chunk is too large: " + strconv.Itoa(len(b)))
		return
	}
	binary.BigEndian.PutUint32(e.header[:4], n)
	e.header[4] = name[0]
	e.header[5] = name[1]
	e.header[6] = name[2]
	e.header[7] = name[3]
	crc := crc32.NewIEEE()
	crc.Write(e.header[4:8])
	crc.Write(b)
	binary.BigEndian.PutUint32(e.footer[:4], crc.Sum32())

	_, e.err = e.w.Write(e.header[:8])
	if e.err != nil {
		return
	}
	_, e.err = e.w.Write(b)
	if e.err != nil {
		return
	}
	_, e.err = e.w.Write(e.footer[:4])
}

func (e *encoder) writeIHDR() {
	b := e.m.Bounds()
	binary.BigEndian.PutUint32(e.tmp[0:4], uint32(b.Dx()))
	binary.BigEndian.PutUint32(e.tmp[4:8], uint32(b.Dy()))
	// Set bit depth and color type.
	switch e.cb {
	case cbG8:
		e.tmp[8] = 8
		e.tmp[9] = ctGrayscale
	case cbTC8:
		e.tmp[8] = 8
		e.tmp[9] = ctTrueColor
	case cbP8:
		e.tmp[8] = 8
		e.tmp[9] = ctPaletted
	case cbP4:
		e.tmp[8] = 4
		e.tmp[9] = ctPaletted
	case cbP2:
		e.tmp[8] = 2
		e.tmp[9] = ctPaletted
	case cbP1:
		e.tmp[8] = 1
		e.tmp[9] = ctPaletted
	case cbTCA8:
		e.tmp[8] = 8
		e.tmp[9] = ctTrueColorAlpha
	case cbG16:
		e.tmp[8] = 16
		e.tmp[9] = ctGrayscale
	case cbTC16:
		e.tmp[8] = 16
		e.tmp[9] = ctTrueColor
	case cbTCA16:
		e.tmp[8] = 16
		e.tmp[9] = ctTrueColorAlpha
	}
	e.tmp[10] = 0 // default compression method
	e.tmp[11] = 0 // default filter method
	e.tmp[12] = 0 // non-interlaced
	e.writeChunk(e.tmp[:13], "IHDR")
}

func (e *encoder) writePLTEAndTRNS(p color.Palette) {
	if len(p) < 1 || len(p) > 256 {
		e.err = FormatError("bad palette length: " + strconv.Itoa(len(p)))
		return
	}
	last := -1
	for i, c := range p {
		c1 := color.NRGBAModel.Convert(c).(color.NRGBA)
		e.tmp[3*i+0] = c1.R
		e.tmp[3*i+1] = c1.G
		e.tmp[3*i+2] = c1.B
		if c1.A != 0xff {
			last = i
		}
		e.tmp[3*256+i] = c1.A
	}
	e.writeChunk(e.tmp[:3*len(p)], "PLTE")
	if last != -1 {
		e.writeChunk(e.tmp[3*256:3*256+1+last], "tRNS")
	}
}

// An encoder is an io.Writer that satisfies writes by writing PNG IDAT chunks,
// including an 8-byte header and 4-byte CRC checksum per Write call. Such calls
// should be relatively infrequent, since writeIDATs uses a bufio.Writer.
//
// This method should only be called from writeIDATs (via writeImage).
// No other code should treat an encoder as an io.Writer.
func (e *encoder) Write(b []byte) (int, error) {
	e.writeChunk(b, "IDAT")
	if e.err != nil {
		return 0, e.err
	}
	return len(b), nil
}

// Chooses the filter to use for encoding the current row, and applies it.
// The return value is the index of the filter and also of the row in cr that has had it applied.
func filter(cr *[nFilter][]byte, pr []byte, bpp int) int {
	// We try all five filter types, and pick the one that minimizes the sum of absolute differences.
	// This is the same heuristic that libpng uses, although the filters are attempted in order of
	// estimated most likely to be minimal (ftUp, ftPaeth, ftNone, ftSub, ftAverage), rather than
	// in their enumeration order (ftNone, ftSub, ftUp, ftAverage, ftPaeth).
	cdat0 := cr[0][1:]
	cdat1 := cr[1][1:]
	cdat2 := cr[2][1:]
	cdat3 := cr[3][1:]
	cdat4 := cr[4][1:]
	pdat := pr[1:]
	n := len(cdat0)

	// The up filter.
	sum := 0
	for i := 0; i < n; i++ {
		cdat2[i] = cdat0[i] - pdat[i]
		sum += abs8(cdat2[i])
	}
	best := sum
	filter := ftUp

	// The Paeth filter.
	sum = 0
	for i := 0; i < bpp; i++ {
		cdat4[i] = cdat0[i] - pdat[i]
		sum += abs8(cdat4[i])
	}
	for i := bpp; i < n; i++ {
		cdat4[i] = cdat0[i] - paeth(cdat0[i-bpp], pdat[i], pdat[i-bpp])
		sum += abs8(cdat4[i])
		if sum >= best {
			break
		}
	}
	if sum < best {
		best = sum
		filter = ftPaeth
	}

	// The none filter.
	sum = 0
	for i := 0; i < n; i++ {
		sum += abs8(cdat0[i])
		if sum >= best {
			break
		}
	}
	if sum < best {
		best = sum
		filter = ftNone
	}

	// The sub filter.
	sum = 0
	for i := 0; i < bpp; i++ {
		cdat1[i] = cdat0[i]
		sum += abs8(cdat1[i])
	}
	for i := bpp; i < n; i++ {
		cdat1[i] = cdat0[i] - cdat0[i-bpp]
		sum += abs8(cdat1[i])
		if sum >= best {
			break
		}
	}
	if sum < best {
		best = sum
		filter = ftSub
	}

	// The average filter.
	sum = 0
	for i := 0; i < bpp; i++ {
		cdat3[i] = cdat0[i] - pdat[i]/2
		sum += abs8(cdat3[i])
	}
	for i := bpp; i < n; i++ {
		cdat3[i] = cdat0[i] - uint8((int(cdat0[i-bpp])+int(pdat[i]))/2)
		sum += abs8(cdat3[i])
		if sum >= best {
			break
		}
	}
	if sum < best {
		filter = ftAverage
	}

	return filter
}

func zeroMemory(v []uint8) {
	for i := range v {
		v[i] = 0
	}
}

func (e *encoder) writeImage(w io.Writer, m image.Image, cb int, level int) error {
	if e.zw == nil || e.zwLevel != level {
		zw, err := zlib.NewWriterLevel(w, level)
		if err != nil {
			return err
		}
		e.zw = zw
		e.zwLevel = level
	} else {
		e.zw.Reset(w)
	}
	defer e.zw.Close()

	bitsPerPixel := 0

	switch cb {
	case cbG8:
		bitsPerPixel = 8
	case cbTC8:
		bitsPerPixel = 24
	case cbP8:
		bitsPerPixel = 8
	case cbP4:
		bitsPerPixel = 4
	case cbP2:
		bitsPerPixel = 2
	case cbP1:
		bitsPerPixel = 1
	case cbTCA8:
		bitsPerPixel = 32
	case cbTC16:
		bitsPerPixel = 48
	case cbTCA16:
		bitsPerPixel = 64
	case cbG16:
		bitsPerPixel = 16
	}

	// cr[*] and pr are the bytes for the current and previous row.
	// cr[0] is unfiltered (or equivalently, filtered with the ftNone filter).
	// cr[ft], for non-zero filter types ft, are buffers for transforming cr[0] under the
	// other PNG filter types. These buffers are allocated once and re-used for each row.
	// The +1 is for the per-row filter type, which is at cr[*][0].
	b := m.Bounds()
	sz := 1 + (bitsPerPixel*b.Dx()+7)/8
	for i := range e.cr {
		if cap(e.cr[i]) < sz {
			e.cr[i] = make([]uint8, sz)
		} else {
			e.cr[i] = e.cr[i][:sz]
		}
		e.cr[i][0] = uint8(i)
	}
	cr := e.cr
	if cap(e.pr) < sz {
		e.pr = make([]uint8, sz)
	} else {
		e.pr = e.pr[:sz]
		zeroMemory(e.pr)
	}
	pr := e.pr

	gray, _ := m.(*image.Gray)
	rgba, _ := m.(*image.RGBA)
	paletted, _ := m.(*image.Paletted)
	nrgba, _ := m.(*image.NRGBA)

	for y := b.Min.Y; y < b.Max.Y; y++ {
		// Convert from colors to bytes.
		i := 1
		switch cb {
		case cbG8:
			if gray != nil {
				offset := (y - b.Min.Y) * gray.Stride
				copy(cr[0][1:], gray.Pix[offset:offset+b.Dx()])
			} else {
				for x := b.Min.X; x < b.Max.X; x++ {
					c := color.GrayModel.Convert(m.At(x, y)).(color.Gray)
					cr[0][i] = c.Y
					i++
				}
			}
		case cbTC8:
			// We have previously verified that the alpha value is fully opaque.
			cr0 := cr[0]
			stride, pix := 0, []byte(nil)
			if rgba != nil {
				stride, pix = rgba.Stride, rgba.Pix
			} else if nrgba != nil {
				stride, pix = nrgba.Stride, nrgba.Pix
			}
			if stride != 0 {
				j0 := (y - b.Min.Y) * stride
				j1 := j0 + b.Dx()*4
				for j := j0; j < j1; j += 4 {
					cr0[i+0] = pix[j+0]
					cr0[i+1] = pix[j+1]
					cr0[i+2] = pix[j+2]
					i += 3
				}
			} else {
				for x := b.Min.X; x < b.Max.X; x++ {
					r, g, b, _ := m.At(x, y).RGBA()
					cr0[i+0] = uint8(r >> 8)
					cr0[i+1] = uint8(g >> 8)
					cr0[i+2] = uint8(b >> 8)
					i += 3
				}
			}
		case cbP8:
			if paletted != nil {
				offset := (y - b.Min.Y) * paletted.Stride
				copy(cr[0][1:], paletted.Pix[offset:offset+b.Dx()])
			} else {
				pi := m.(image.PalettedImage)
				for x := b.Min.X; x < b.Max.X; x++ {
					cr[0][i] = pi.ColorIndexAt(x, y)
					i += 1
				}
			}

		case cbP4, cbP2, cbP1:
			pi := m.(image.PalettedImage)

			var a uint8
			var c int
			for x := b.Min.X; x < b.Max.X; x++ {
				a = a<<uint(bitsPerPixel) | pi.ColorIndexAt(x, y)
				c++
				if c == 8/bitsPerPixel {
					cr[0][i] = a
					i += 1
					a = 0
					c = 0
				}
			}
			if c != 0 {
				for c != 8/bitsPerPixel {
					a = a << uint(bitsPerPixel)
					c++
				}
				cr[0][i] = a
			}

		case cbTCA8:
			if nrgba != nil {
				offset := (y - b.Min.Y) * nrgba.Stride
				copy(cr[0][1:], nrgba.Pix[offset:offset+b.Dx()*4])
			} else {
				// Convert from image.Image (which is alpha-premultiplied) to PNG's non-alpha-premultiplied.
				for x := b.Min.X; x < b.Max.X; x++ {
					c := color.NRGBAModel.Convert(m.At(x, y)).(color.NRGBA)
					cr[0][i+0] = c.R
					cr[0][i+1] = c.G
					cr[0][i+2] = c.B
					cr[0][i+3] = c.A
					i += 4
				}
			}
		case cbG16:
			for x := b.Min.X; x < b.Max.X; x++ {
				c := color.Gray16Model.Convert(m.At(x, y)).(color.Gray16)
				cr[0][i+0] = uint8(c.Y >> 8)
				cr[0][i+1] = uint8(c.Y)
				i += 2
			}
		case cbTC16:
			// We have previously verified that the alpha value is fully opaque.
			for x := b.Min.X; x < b.Max.X; x++ {
				r, g, b, _ := m.At(x, y).RGBA()
				cr[0][i+0] = uint8(r >> 8)
				cr[0][i+1] = uint8(r)
				cr[0][i+2] = uint8(g >> 8)
				cr[0][i+3] = uint8(g)
				cr[0][i+4] = uint8(b >> 8)
				cr[0][i+5] = uint8(b)
				i += 6
			}
		case cbTCA16:
			// Convert from image.Image (which is alpha-premultiplied) to PNG's non-alpha-premultiplied.
			for x := b.Min.X; x < b.Max.X; x++ {
				c := color.NRGBA64Model.Convert(m.At(x, y)).(color.NRGBA64)
				cr[0][i+0] = uint8(c.R >> 8)
				cr[0][i+1] = uint8(c.R)
				cr[0][i+2] = uint8(c.G >> 8)
				cr[0][i+3] = uint8(c.G)
				cr[0][i+4] = uint8(c.B >> 8)
				cr[0][i+5] = uint8(c.B)
				cr[0][i+6] = uint8(c.A >> 8)
				cr[0][i+7] = uint8(c.A)
				i += 8
			}
		}

		// Apply the filter.
		// Skip filter for NoCompression and paletted images (cbP8) as
		// "filters are rarely useful on palette images" and will result
		// in larger files (see http://www.libpng.org/pub/png/book/chapter09.html).
		f := ftNone
		if level != zlib.NoCompression && cb != cbP8 && cb != cbP4 && cb != cbP2 && cb != cbP1 {
			// Since we skip paletted images we don't have to worry about
			// bitsPerPixel not being a multiple of 8
			bpp := bitsPerPixel / 8
			f = filter(&cr, pr, bpp)
		}

		// Write the compressed bytes.
		if _, err := e.zw.Write(cr[f]); err != nil {
			return err
		}

		// The current row for y is the previous row for y+1.
		pr, cr[0] = cr[0], pr
	}
	return nil
}

// maybeWriteGAMA will write out a gAMA chunk if the metadata has
// gamma information.
func (e *encoder) maybeWriteGAMA(m *Metadata) {
	// If we have no metadata, or we do but the gamma bit is empty, then
	// just bail.
	if m == nil || m.Gamma == nil {
		return
	}
	if e.err != nil {
		return
	}

	binary.BigEndian.PutUint32(e.tmp[:4], *m.Gamma)
	e.writeChunk(e.tmp[:4], "gAMA")
	return
}

// maybeWriteXMP will write out the XMP data if we have it. XMP data
// is just an xml-encoded string that goes out in an iTXt chunk.
func (e *encoder) maybeWriteXMP(ctx context.Context, m *Metadata, opts ...image.WriteOption) {
	// If we have no metadata, or we do but the gamma bit is empty, then
	// just bail.
	if m == nil || (m.rawXmp == nil && m.xmp == nil) {
		return
	}
	if e.err != nil {
		return
	}

	xmpText := ""
	if m.rawXmp != nil {
		xmpText = *m.rawXmp
	}
	if m.xmp != nil {
		var err error
		xmpText, err = m.xmp.Encode(ctx, opts...)
		if err != nil {
			e.err = err
			return
		}
	}

	v := &TextEntry{Key: xmpTextKey, Value: xmpText, EntryType: EtItext}
	e.maybeWriteITXT(v)
	return
}

// maybeWritePHYS will write out a pHYs chunk if the metadata has
// physical size information.
func (e *encoder) maybeWritePHYS(m *Metadata) {
	// If we have no metadata, or we do but the gamma bit is empty, then
	// just bail.
	if m == nil || m.Dimension == nil {
		return
	}
	if e.err != nil {
		return
	}

	binary.BigEndian.PutUint32(e.tmp[:4], uint32(m.Dimension.X))
	binary.BigEndian.PutUint32(e.tmp[4:8], uint32(m.Dimension.Y))
	e.tmp[8] = byte(m.Dimension.Unit)
	e.writeChunk(e.tmp[:9], "pHYs")
	return
}

// maybeWriteTIME will write out a tIME chunk if the metadata has time
// information. This is the last modified time, and per the spec isn't
// the image creation time. It feels like maybe this should be set on
// write if it doesn't exist, but we'll leave that decision for later.
func (e *encoder) maybeWriteTIME(m *Metadata) {
	if e.err != nil || m == nil || m.LastModified == nil {
		return
	}

	utc := m.LastModified.UTC()

	binary.BigEndian.PutUint16(e.tmp[:2], uint16(utc.Year()))
	e.tmp[2] = byte(utc.Month())
	e.tmp[3] = byte(utc.Day())
	e.tmp[4] = byte(utc.Hour())
	e.tmp[5] = byte(utc.Minute())
	e.tmp[6] = byte(utc.Second())

	e.writeChunk(e.tmp[:7], "tIME")
	return
}

// maybeWriteTEXT will write out a tEXt entry.
func (e *encoder) maybeWriteTEXT(t *TextEntry) {
	if e.err != nil {
		return
	}

	tl := len(t.Key) + len(t.Value) + 1
	buf := make([]byte, tl)
	copy(buf, []byte(t.Key))
	buf[len(t.Key)] = 0
	for i, b := range t.Value {
		buf[len(t.Key)+i+1] = byte(b)
	}

	e.writeChunk(buf, "tEXt")
	return
}

// maybeWriteZTXT will write out a zTXt entry.
func (e *encoder) maybeWriteZTXT(t *TextEntry) {
	if e.err != nil {
		return
	}

	val, method, err := e.pngCompress([]byte(t.Value))
	if err != nil {
		e.err = err
		return
	}

	copy(e.tmp[:len(t.Key)], []byte(t.Key))
	e.tmp[len(t.Key)] = 0
	e.tmp[len(t.Key)+1] = byte(method)
	length := len(t.Key) + 2
	if len(val) != 0 {
		length += len(val)
		copy(e.tmp[len(t.Key)+2:], []byte(val))
	}

	e.writeChunk(e.tmp[:length], "zTXt")
	return
}

// maybeWriteITXT will write out an iTXt entry.
func (e *encoder) maybeWriteITXT(t *TextEntry) {
	if e.err != nil {
		return
	}

	val, method, err := e.pngCompress([]byte(t.Value))
	if err != nil {
		e.err = err
		return
	}

	buf := make([]byte, len(t.Key)+len(val)+len(t.LanguageTag)+len(t.TranslatedKey)+5)

	// Add the key
	copy(buf[:len(t.Key)], []byte(t.Key))
	// Key null terminator
	buf[len(t.Key)] = 0
	// Compression flag. Which we always set, though it's optional and
	// we could stuff uncompressed text in here.
	//
	// TODO: Check and see if the compressed buffer is smaller than the
	// original text and choose compressed or uncompressed based on
	// that.
	buf[len(t.Key)+1] = 1
	// Compression method (which is always 0, but whatever)
	buf[len(t.Key)+2] = byte(method)
	length := len(t.Key) + 3

	// Language Tag, if we have one
	if len(t.LanguageTag) > 0 {
		copy(buf[length:], []byte(t.LanguageTag))
		length += len(t.LanguageTag)
	}
	// Language tag null terminator
	buf[length] = 0
	length++

	// Translated key, if we have one
	if len(t.TranslatedKey) > 0 {
		copy(buf[length:], []byte(t.TranslatedKey))
		length += len(t.TranslatedKey)
	}
	// Translated key null terminator
	buf[length] = 0
	length++

	// Tack on the compressed value
	copy(buf[length:], val)

	e.writeChunk(buf, "iTXt")
	return
}

func (e *encoder) maybeWriteHIST(m *Metadata) {
	if m == nil || m.Histogram == nil {
		return
	}
	if e.err != nil {
		return
	}
	b := make([]byte, len(m.Histogram)*2)
	for i, h := range m.Histogram {
		binary.BigEndian.PutUint16(b[i*2:i*2+2], h)
	}
	e.writeChunk(b, "hIST")
	return
}

// maybeWriteSRGB will write out a sRGB chunk if the metadata has
// sRGB information.
func (e *encoder) maybeWriteSRGB(m *Metadata) {
	// If we have no metadata, or we do but the sRGB intent bit is
	// empty, then just bail.
	if m == nil || m.SRGBIntent == nil {
		return
	}
	if e.err != nil {
		return
	}

	e.tmp[0] = byte(*m.SRGBIntent)
	e.writeChunk(e.tmp[:1], "sRGB")
	return
}

// pngCompress will compress a byte slice. Returns the compressed
// slice, the compression type (0==no compression, 1 == deflate) and
// an error if something went wrong.
func (e *encoder) pngCompress(input []byte) ([]byte, int, error) {
	var b bytes.Buffer

	// Set up a new zlib writer here with the appropriate compression
	// level. Theoretically we really ought to cache these for
	// performance reasons, but we can deal with that later after
	// everything actually works.
	level := levelToZlib(e.enc.CompressionLevel)
	zw, err := zlib.NewWriterLevel(&b, level)
	if err != nil {
		return nil, 0, err
	}

	// Write the input to the zlib writer and get it smushed.
	if _, err := zw.Write(input); err != nil {
		return nil, 0, err
	}
	// We're done so close the writer to flush out anything left and
	// make sure it's smushed properly.
	if err := zw.Close(); err != nil {
		return nil, 0, err
	}

	// Return the smushed bytes in our buffer and the compression
	// method. (Which is 0, or deflate, since that's all that's
	// supported)
	return b.Bytes(), 0, nil
}

// maybeWriteICCP will write out an ICCP chunk if the metadata has
// ICC information.
func (e *encoder) maybeWriteICCP(ctx context.Context, m *Metadata, opts ...image.WriteOption) {
	// If we have no metadata, or we do but the sRGB intent bit is
	// empty, then just bail.
	if m == nil || e.err != nil {
		return
	}

	// No encoded or decoded data so no ICC -- skip
	if m.icc == nil && len(m.rawIcc) == 0 {
		return
	}

	var chunk []byte
	// Do we have a raw ICCP chunk we read and presumably haven't
	// decoded? Then just use it.
	if len(m.rawIcc) != 0 {
		chunk = m.rawIcc
	}

	// Do we have an ICC object registered? If so, encode that and use
	// its encoded value.
	if m.icc != nil {
		var err error
		// Go encode the thing.
		chunk, err = m.icc.Encode(ctx, opts...)
		if err != nil {
			e.err = err
			return
		}

		chunk, compression, err := e.pngCompress(chunk)
		if err != nil {
			e.err = err
			return
		}

		var icc []byte
		icc = []byte(m.iccName)
		icc = append(icc, 0)
		icc = append(icc, byte(compression))
		icc = append(icc, chunk...)
		e.writeChunk(icc, "iCCP")
	}

	return
}

// maybeWriteCHRM will write out a cHRM chunk if the metadata has
// chroma information.
func (e *encoder) maybeWriteCHRM(m *Metadata) {
	// If we have no metadata, or we do but the gamma bit is empty, then
	// just bail.
	if m == nil || m.Chroma == nil {
		return
	}
	if e.err != nil {
		return
	}

	binary.BigEndian.PutUint32(e.tmp[:4], m.Chroma.WhiteX)
	binary.BigEndian.PutUint32(e.tmp[4:8], m.Chroma.WhiteY)
	binary.BigEndian.PutUint32(e.tmp[8:12], m.Chroma.RedX)
	binary.BigEndian.PutUint32(e.tmp[12:16], m.Chroma.RedY)
	binary.BigEndian.PutUint32(e.tmp[16:20], m.Chroma.GreenX)
	binary.BigEndian.PutUint32(e.tmp[20:24], m.Chroma.GreenY)
	binary.BigEndian.PutUint32(e.tmp[24:28], m.Chroma.BlueX)
	binary.BigEndian.PutUint32(e.tmp[28:32], m.Chroma.BlueY)
	e.writeChunk(e.tmp[:32], "cHRM")
	return
}

// Write the actual image data to one or more IDAT chunks.
func (e *encoder) writeIDATs() {
	if e.err != nil {
		return
	}
	if e.bw == nil {
		e.bw = bufio.NewWriterSize(e, 1<<15)
	} else {
		e.bw.Reset(e)
	}
	e.err = e.writeImage(e.bw, e.m, e.cb, levelToZlib(e.enc.CompressionLevel))
	if e.err != nil {
		return
	}
	e.err = e.bw.Flush()
}

// This function is required because we want the zero value of
// Encoder.CompressionLevel to map to zlib.DefaultCompression.
func levelToZlib(l CompressionLevel) int {
	switch l {
	case DefaultCompression:
		return zlib.DefaultCompression
	case NoCompression:
		return zlib.NoCompression
	case BestSpeed:
		return zlib.BestSpeed
	case BestCompression:
		return zlib.BestCompression
	default:
		return zlib.DefaultCompression
	}
}

func (e *encoder) writeIEND() { e.writeChunk(nil, "IEND") }

// Encode writes the Image m to w in PNG format. Any Image may be
// encoded, but images that are not image.NRGBA might be encoded lossily.
func Encode(w io.Writer, m image.Image) error {
	var e Encoder
	return e.Encode(w, m)
}

func EncodeExtended(ctx context.Context, w io.Writer, m image.Image, opts ...image.WriteOption) error {
	var e Encoder
	return e.EncodeExtended(ctx, w, m, opts...)
}

// Encode writes the Image m to w in PNG format.
func (enc *Encoder) Encode(w io.Writer, m image.Image) error {
	return enc.EncodeExtended(context.TODO(), w, m)
}

// EncodeExtended writes the Image m to w in PNG format
func (enc *Encoder) EncodeExtended(ctx context.Context, w io.Writer, m image.Image, opts ...image.WriteOption) error {
	var metadata *Metadata

	//  Run through all the opts.
	for _, o := range opts {
		switch lo := o.(type) {
		case *Metadata:
			if metadata != nil {
				return fmt.Errorf("Multiple metadata passed")
			}
			metadata = lo
			// Make sure the metadata is OK.
			if err := metadata.validate(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("Unknown write option of type %T given", o)
		}
	}

	// Check to see if we have a deferred image.
	di, deferred := m.(*Deferred)

	// If this isn't deferred then do some size validation. If it is
	// then we assume that the file's OK since we read it.
	if !deferred {
		// Obviously, negative widths and heights are
		// invalid. Furthermore, the PNG spec section 11.2.2 says that
		// zero is invalid. Excessively large images are also rejected.
		mw, mh := int64(m.Bounds().Dx()), int64(m.Bounds().Dy())
		if mw <= 0 || mh <= 0 || mw >= 1<<32 || mh >= 1<<32 {
			return FormatError("invalid image size: " + strconv.FormatInt(mw, 10) + "x" + strconv.FormatInt(mh, 10))
		}
	}

	var e *encoder
	if enc.BufferPool != nil {
		buffer := enc.BufferPool.Get()
		e = (*encoder)(buffer)

	}
	if e == nil {
		e = &encoder{}
	}
	if enc.BufferPool != nil {
		defer enc.BufferPool.Put((*EncoderBuffer)(e))
	}

	e.enc = enc
	e.w = w
	e.m = m

	var pal color.Palette

	// Skip palette checking if this is a deferred image, since we're
	// just splatting out whatever we read.
	if !deferred {
		// cbP8 encoding needs PalettedImage's ColorIndexAt method.
		if _, ok := m.(image.PalettedImage); ok {
			pal, _ = m.ColorModel().(color.Palette)
		}
		if pal != nil {
			if len(pal) <= 2 {
				e.cb = cbP1
			} else if len(pal) <= 4 {
				e.cb = cbP2
			} else if len(pal) <= 16 {
				e.cb = cbP4
			} else {
				e.cb = cbP8
			}
		} else {
			switch m.ColorModel() {
			case color.GrayModel:
				e.cb = cbG8
			case color.Gray16Model:
				e.cb = cbG16
			case color.RGBAModel, color.NRGBAModel, color.AlphaModel:
				if opaque(m) {
					e.cb = cbTC8
				} else {
					e.cb = cbTCA8
				}
			default:
				if opaque(m) {
					e.cb = cbTC16
				} else {
					e.cb = cbTCA16
				}
			}
		}
	}

	_, e.err = io.WriteString(w, pngHeader)
	switch deferred {
	case true:
		e.writeChunk(di.ihdr[:len(di.ihdr)-4], "IHDR")
	case false:
		e.writeIHDR()
	}

	// If we have a gAMA chunk then it needs to be written now.
	if metadata != nil {
		e.maybeWriteGAMA(metadata)
		e.maybeWriteCHRM(metadata)
		e.maybeWriteSRGB(metadata)
		e.maybeWriteTIME(metadata)
		e.maybeWriteICCP(ctx, metadata, opts...)
		e.maybeWritePHYS(metadata)

		e.maybeWriteXMP(ctx, metadata, opts...)
		for _, v := range metadata.Text {
			switch v.EntryType {
			case EtText:
				e.maybeWriteTEXT(v)
			case EtZtext:
				e.maybeWriteZTXT(v)
			case EtItext:
				e.maybeWriteITXT(v)
			}
		}
	}

	switch deferred {
	case true:
		if di.plte != nil {
			e.writeChunk(di.plte[:len(di.plte)-4], "PLTE")
		}
		if di.trns != nil {
			e.writeChunk(di.trns[:len(di.trns)-4], "tRNS")
		}
	case false:
		if pal != nil {
			e.writePLTEAndTRNS(pal)
		}
	}
	e.maybeWriteHIST(metadata)

	switch deferred {
	case true:
		if di.idat != nil {
			e.writeChunk(di.idat[:len(di.idat)-4], "IDAT")
		}
	case false:
		e.writeIDATs()
	}

	e.writeIEND()
	return e.err
}
