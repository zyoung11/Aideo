package image

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"fmt"
	"image"
	"image/draw"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"os"
)

var kittyImageID uint32 = uint32(os.Getpid()<<16) + uint32(time.Now().UnixMicro()&0xFFFF)

var kittyZlibPool = sync.Pool{
	New: func() interface{} {
		w, _ := zlib.NewWriterLevel(nil, zlib.BestSpeed)
		return w
	},
}

var kittyCompressPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

var kittyBase64Pool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 256*1024)
		return buf
	},
}

var kittyRGBPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 512*1024)
	},
}

func renderKitty(img image.Image, widthChars, heightChars int) error {
	EncodeKittyFrame(os.Stdout, img, widthChars, heightChars)
	return nil
}

func EncodeKittyFrame(w io.Writer, img image.Image, c, r int) uint32 {
	bounds := img.Bounds()
	pixelW := bounds.Dx()
	pixelH := bounds.Dy()

	imageID := atomic.AddUint32(&kittyImageID, 1)

	rgba := image.NewRGBA(bounds)
	draw.Draw(rgba, rgba.Bounds(), img, bounds.Min, draw.Src)
	data := rgba.Pix

	var compressed bool
	var compData []byte
	if len(data) > 1024 {
		buf := kittyCompressPool.Get().(*bytes.Buffer)
		buf.Reset()
		zw := kittyZlibPool.Get().(*zlib.Writer)
		zw.Reset(buf)
		zw.Write(data)
		zw.Close()
		compData = buf.Bytes()
		compressed = true
		kittyZlibPool.Put(zw)
		kittyCompressPool.Put(buf)
	} else {
		compData = data
	}

	encLen := base64.StdEncoding.EncodedLen(len(compData))
	base64Raw := kittyBase64Pool.Get().([]byte)
	if cap(base64Raw) < encLen {
		base64Raw = make([]byte, encLen)
	}
	base64Buf := base64Raw[:encLen]
	base64.StdEncoding.Encode(base64Buf, compData)

	if compressed {
		fmt.Fprintf(w, "\x1b_Ga=T,f=32,i=%d,s=%d,v=%d,c=%d,r=%d,q=2,o=z",
			imageID, pixelW, pixelH, c, r)
	} else {
		fmt.Fprintf(w, "\x1b_Ga=T,f=32,i=%d,s=%d,v=%d,c=%d,r=%d,q=2",
			imageID, pixelW, pixelH, c, r)
	}

	for i := 0; i < encLen; i += 4096 {
		end := i + 4096
		if end > encLen {
			end = encLen
		}
		chunk := base64Buf[i:end]

		if i == 0 {
			if i+4096 < encLen {
				fmt.Fprintf(w, ",m=1;%s\x1b\\", chunk)
			} else {
				fmt.Fprintf(w, ";%s\x1b\\", chunk)
			}
		} else {
			if i+4096 < encLen {
				fmt.Fprintf(w, "\x1b_Gm=1,q=2;%s\x1b\\", chunk)
			} else {
				fmt.Fprintf(w, "\x1b_Gm=0,q=2;%s\x1b\\", chunk)
			}
		}
	}

	kittyBase64Pool.Put(base64Raw[:0])
	return imageID
}

func EncodeKittyFrameRaw(w io.Writer, data []byte, pixelW, pixelH, c, r int) uint32 {
	imageID := atomic.AddUint32(&kittyImageID, 1)

	rgbLen := pixelW * pixelH * 3
	rgbRaw := kittyRGBPool.Get().([]byte)
	if cap(rgbRaw) < rgbLen {
		rgbRaw = make([]byte, rgbLen)
	}
	rgbBuf := rgbRaw[:rgbLen]

	blocks := len(data) / 32
	j := 0
	for b := 0; b < blocks; b++ {
		s := b * 32
		rgbBuf[j] = data[s]
		rgbBuf[j+1] = data[s+1]
		rgbBuf[j+2] = data[s+2]
		rgbBuf[j+3] = data[s+4]
		rgbBuf[j+4] = data[s+5]
		rgbBuf[j+5] = data[s+6]
		rgbBuf[j+6] = data[s+8]
		rgbBuf[j+7] = data[s+9]
		rgbBuf[j+8] = data[s+10]
		rgbBuf[j+9] = data[s+12]
		rgbBuf[j+10] = data[s+13]
		rgbBuf[j+11] = data[s+14]
		rgbBuf[j+12] = data[s+16]
		rgbBuf[j+13] = data[s+17]
		rgbBuf[j+14] = data[s+18]
		rgbBuf[j+15] = data[s+20]
		rgbBuf[j+16] = data[s+21]
		rgbBuf[j+17] = data[s+22]
		rgbBuf[j+18] = data[s+24]
		rgbBuf[j+19] = data[s+25]
		rgbBuf[j+20] = data[s+26]
		rgbBuf[j+21] = data[s+28]
		rgbBuf[j+22] = data[s+29]
		rgbBuf[j+23] = data[s+30]
		j += 24
	}
	remain := len(data) % 32
	for i := 0; i < remain; i += 4 {
		rgbBuf[j] = data[blocks*32+i]
		rgbBuf[j+1] = data[blocks*32+i+1]
		rgbBuf[j+2] = data[blocks*32+i+2]
		j += 3
	}

	encLen := base64.StdEncoding.EncodedLen(rgbLen)
	base64Raw := kittyBase64Pool.Get().([]byte)
	if cap(base64Raw) < encLen {
		base64Raw = make([]byte, encLen)
	}
	base64Buf := base64Raw[:encLen]
	base64.StdEncoding.Encode(base64Buf, rgbBuf)

	kittyRGBPool.Put(rgbRaw[:0])

	bw, ok := w.(*bytes.Buffer)
	if ok {
		bw.WriteString("\x1b_Ga=T,f=24,i=")
		writeUint32(bw, imageID)
		bw.WriteString(",s=")
		writeUint32(bw, uint32(pixelW))
		bw.WriteString(",v=")
		writeUint32(bw, uint32(pixelH))
		bw.WriteString(",c=")
		writeUint32(bw, uint32(c))
		bw.WriteString(",r=")
		writeUint32(bw, uint32(r))
		bw.WriteString(",q=2")
	} else {
		fmt.Fprintf(w, "\x1b_Ga=T,f=24,i=%d,s=%d,v=%d,c=%d,r=%d,q=2",
			imageID, pixelW, pixelH, c, r)
	}

	const chunkSize = 524288
	if ok {
		for i := 0; i < encLen; i += chunkSize {
			end := i + chunkSize
			if end > encLen {
				end = encLen
			}
			chunk := base64Buf[i:end]
			if i == 0 {
				if i+chunkSize < encLen {
					bw.WriteString(",m=1;")
				} else {
					bw.WriteString(";")
				}
				bw.Write(chunk)
				bw.WriteString("\x1b\\")
			} else {
				bw.WriteString("\x1b_Gm=")
				if i+chunkSize < encLen {
					bw.WriteByte('1')
				} else {
					bw.WriteByte('0')
				}
				bw.WriteString(",q=2;")
				bw.Write(chunk)
				bw.WriteString("\x1b\\")
			}
		}
	} else {
		for i := 0; i < encLen; i += chunkSize {
			end := i + chunkSize
			if end > encLen {
				end = encLen
			}
			chunk := base64Buf[i:end]
			if i == 0 {
				if i+chunkSize < encLen {
					fmt.Fprintf(w, ",m=1;%s\x1b\\", chunk)
				} else {
					fmt.Fprintf(w, ";%s\x1b\\", chunk)
				}
			} else {
				if i+chunkSize < encLen {
					fmt.Fprintf(w, "\x1b_Gm=1,q=2;%s\x1b\\", chunk)
				} else {
					fmt.Fprintf(w, "\x1b_Gm=0,q=2;%s\x1b\\", chunk)
				}
			}
		}
	}

	kittyBase64Pool.Put(base64Raw[:0])
	return imageID
}

func writeUint32(b *bytes.Buffer, n uint32) {
	if n >= 1000000000 {
		b.WriteByte(byte('0' + n/1000000000%10))
		b.WriteByte(byte('0' + n/100000000%10))
		b.WriteByte(byte('0' + n/10000000%10))
		b.WriteByte(byte('0' + n/1000000%10))
		b.WriteByte(byte('0' + n/100000%10))
		b.WriteByte(byte('0' + n/10000%10))
		b.WriteByte(byte('0' + n/1000%10))
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 100000000 {
		b.WriteByte(byte('0' + n/100000000%10))
		b.WriteByte(byte('0' + n/10000000%10))
		b.WriteByte(byte('0' + n/1000000%10))
		b.WriteByte(byte('0' + n/100000%10))
		b.WriteByte(byte('0' + n/10000%10))
		b.WriteByte(byte('0' + n/1000%10))
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 10000000 {
		b.WriteByte(byte('0' + n/10000000%10))
		b.WriteByte(byte('0' + n/1000000%10))
		b.WriteByte(byte('0' + n/100000%10))
		b.WriteByte(byte('0' + n/10000%10))
		b.WriteByte(byte('0' + n/1000%10))
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 1000000 {
		b.WriteByte(byte('0' + n/1000000%10))
		b.WriteByte(byte('0' + n/100000%10))
		b.WriteByte(byte('0' + n/10000%10))
		b.WriteByte(byte('0' + n/1000%10))
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 100000 {
		b.WriteByte(byte('0' + n/100000%10))
		b.WriteByte(byte('0' + n/10000%10))
		b.WriteByte(byte('0' + n/1000%10))
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 10000 {
		b.WriteByte(byte('0' + n/10000%10))
		b.WriteByte(byte('0' + n/1000%10))
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 1000 {
		b.WriteByte(byte('0' + n/1000%10))
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 100 {
		b.WriteByte(byte('0' + n/100%10))
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else if n >= 10 {
		b.WriteByte(byte('0' + n/10%10))
		b.WriteByte(byte('0' + n%10))
	} else {
		b.WriteByte(byte('0' + n))
	}
}

func DeleteKittyFrame(w io.Writer, id uint32) {
	fmt.Fprintf(w, "\x1b_Ga=d,d=i,i=%d\x1b\\", id)
}

func clearKittyAll() {
	fmt.Print("\x1b_Ga=d\x1b\\")
}
