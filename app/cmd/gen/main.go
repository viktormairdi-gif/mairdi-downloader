// Генератор значков (PNG/ICO) и постоянного ключа расширения Chrome.
// Запуск из папки app: go run ./cmd/gen
package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

func inRoundRect(x, y, x0, y0, x1, y1, r float64) bool {
	if x < x0 || x > x1 || y < y0 || y > y1 {
		return false
	}
	cx := math.Max(x0+r, math.Min(x, x1-r))
	cy := math.Max(y0+r, math.Min(y, y1-r))
	return (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r
}

func inTri(px, py, ax, ay, bx, by, cx, cy float64) bool {
	s := func(x1, y1, x2, y2, x3, y3 float64) float64 { return (x1-x3)*(y2-y3) - (x2-x3)*(y1-y3) }
	d1, d2, d3 := s(px, py, ax, ay, bx, by), s(px, py, bx, by, cx, cy), s(px, py, cx, cy, ax, ay)
	neg := d1 < 0 || d2 < 0 || d3 < 0
	pos := d1 > 0 || d2 > 0 || d3 > 0
	return !(neg && pos)
}

// Координаты в системе 0..256.
func shape(x, y float64) (bg, fg bool) {
	bg = inRoundRect(x, y, 8, 8, 248, 248, 58)
	if !bg {
		return
	}
	arrow := inRoundRect(x, y, 108, 40, 148, 140, 10) || inTri(x, y, 62, 118, 194, 118, 128, 186)
	tray := inRoundRect(x, y, 52, 202, 204, 224, 11)
	fg = arrow || tray
	return
}

func render(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var nb, nf int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) * 256 / float64(size)
					y := (float64(py) + (float64(sy)+0.5)/ss) * 256 / float64(size)
					b, f := shape(x, y)
					if b {
						nb++
					}
					if f {
						nf++
					}
				}
			}
			if nb == 0 {
				continue
			}
			t := float64(py) / float64(size)
			// оранжевый градиент #F9A35A → #F58735 → #E0661B
			r := 249 - t*25
			g := 163 - t*61
			bl := 90 - t*63
			ff := float64(nf) / float64(nb)
			r = r*(1-ff) + 255*ff
			g = g*(1-ff) + 255*ff
			bl = bl*(1-ff) + 255*ff
			a := float64(nb) / (ss * ss) * 255
			img.Set(px, py, color.NRGBA{uint8(r), uint8(g), uint8(bl), uint8(a)})
		}
	}
	return img
}

func pngBytes(size int) []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, render(size))
	return b.Bytes()
}

func ico(sizes []int) []byte {
	var imgs [][]byte
	for _, s := range sizes {
		imgs = append(imgs, pngBytes(s))
	}
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, []uint16{0, 1, uint16(len(sizes))})
	off := 6 + 16*len(sizes)
	for i, s := range sizes {
		w := byte(s)
		if s >= 256 {
			w = 0
		}
		b.Write([]byte{w, w, 0, 0})
		binary.Write(&b, binary.LittleEndian, []uint16{1, 32})
		binary.Write(&b, binary.LittleEndian, []uint32{uint32(len(imgs[i])), uint32(off)})
		off += len(imgs[i])
	}
	for _, im := range imgs {
		b.Write(im)
	}
	return b.Bytes()
}

func main() {
	ext := filepath.Join("..", "extension")
	_ = os.MkdirAll(filepath.Join(ext, "icons"), 0o755)
	for _, s := range []int{16, 32, 48, 128} {
		_ = os.WriteFile(filepath.Join(ext, "icons", fmt.Sprintf("icon%d.png", s)), pngBytes(s), 0o644)
	}
	_ = os.WriteFile("icon.ico", ico([]int{16, 24, 32, 48, 64, 256}), 0o644)
	_ = os.WriteFile(filepath.Join("ui", "icon.png"), pngBytes(64), 0o644)
	_ = os.WriteFile("icon.png", pngBytes(256), 0o644)

	keyFile := filepath.Join(ext, "key.pub.b64")
	pub, err := os.ReadFile(keyFile)
	if err != nil {
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
		pub = []byte(base64.StdEncoding.EncodeToString(der))
		_ = os.WriteFile(keyFile, pub, 0o644)
	}
	der, _ := base64.StdEncoding.DecodeString(string(pub))
	sum := sha256.Sum256(der)
	h := hex.EncodeToString(sum[:16])
	id := make([]byte, 32)
	for i, c := range h {
		v := c - '0'
		if c >= 'a' {
			v = c - 'a' + 10
		}
		id[i] = byte('a' + v)
	}
	_ = os.WriteFile("ext_id.go", []byte("package main\n\n// ID расширения постоянный: ключ лежит в extension/manifest.json (поле key).\nconst ExtensionID = \""+string(id)+"\"\n"), 0o644)
	fmt.Println("ExtensionID:", string(id))
	fmt.Println("key:", string(pub))
}
