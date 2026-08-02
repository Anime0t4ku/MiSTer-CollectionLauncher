package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const version = "0.4.8"
const defaultBase = "/media/fat/Scripts/.config/CollectionLauncher"

var runtimeBase = defaultBase

var faceMapMu sync.Mutex
var faceConfirmCode uint16
var faceBackCode uint16

type faceMapping struct {
	Confirm uint16 `json:"confirm"`
	Back    uint16 `json:"back"`
}

func faceMappingPath() string {
	return filepath.Join(runtimeBase, "controller.json")
}

func loadFaceMapping() {
	b, err := os.ReadFile(faceMappingPath())
	if err != nil {
		return
	}
	var m faceMapping
	if json.Unmarshal(b, &m) == nil && (m.Confirm == btnSouth || m.Confirm == btnEast) {
		faceMapMu.Lock()
		faceConfirmCode = m.Confirm
		faceBackCode = m.Back
		faceMapMu.Unlock()
		appendLaunchLog("controller mapping loaded confirm=%d back=%d", m.Confirm, m.Back)
	}
}

func resolveFaceButton(code uint16) action {
	faceMapMu.Lock()
	defer faceMapMu.Unlock()
	if faceConfirmCode == 0 {
		faceConfirmCode = code
		if code == btnSouth {
			faceBackCode = btnEast
		} else {
			faceBackCode = btnSouth
		}
		m := faceMapping{Confirm: faceConfirmCode, Back: faceBackCode}
		if b, err := json.MarshalIndent(m, "", "  "); err == nil {
			_ = os.WriteFile(faceMappingPath(), b, 0644)
		}
		appendLaunchLog("controller mapping learned confirm=%d back=%d", faceConfirmCode, faceBackCode)
	}
	if code == faceConfirmCode {
		return actConfirm
	}
	if code == faceBackCode {
		return actBack
	}
	return actNone
}

const (
	evKey    = 1
	evAbs    = 3
	keyEsc   = 1
	keyEnter = 28
	keyUp    = 103
	keyLeft  = 105
	keyRight = 106
	keyDown  = 108
	keyBack  = 158
	btnSouth = 304
	btnEast  = 305
	btnStart = 315
	btnMode  = 316
	absHatX  = 16
	absHatY  = 17
)

type Launch struct {
	System string `json:"system"`
	Path   string `json:"path"`
}

type Entry struct {
	Label   string `json:"label"`
	Artwork string `json:"artwork"`
	Launch  Launch `json:"launch"`
	Command string `json:"command,omitempty"`
}

type Collection struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Wallpaper string  `json:"wallpaper"`
	Logo      string  `json:"logo,omitempty"`
	Music     string  `json:"music,omitempty"`
	Entries   []Entry `json:"entries"`
	Dir       string  `json:"-"`
}

type inputEvent struct {
	Time  syscall.Timeval
	Type  uint16
	Code  uint16
	Value int32
}

type action int

const (
	actNone action = iota
	actLeft
	actRight
	actUp
	actDown
	actConfirm
	actBack
	actHome
)

type fbVar struct {
	Xres, Yres, XresVirtual, YresVirtual                        uint32
	Xoffset, Yoffset                                            uint32
	BitsPerPixel, Grayscale                                     uint32
	RedOffset, RedLength, RedMsb                                uint32
	GreenOffset, GreenLength, GreenMsb                          uint32
	BlueOffset, BlueLength, BlueMsb                             uint32
	TranspOffset, TranspLength, TranspMsb                       uint32
	Nonstd, Activate                                            uint32
	Height, Width                                               uint32
	AccelFlags                                                  uint32
	Pixclock, LeftMargin, RightMargin, UpperMargin, LowerMargin uint32
	HsyncLen, VsyncLen, Sync, Vmode, Rotate, Colorspace         uint32
	Reserved                                                    [4]uint32
}

type framebuffer struct {
	f                 *os.File
	data              []byte
	w, h, stride, bpp int
}

func openFramebuffer() (*framebuffer, error) {
	f, err := os.OpenFile("/dev/fb0", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var v fbVar
	const FBIOGET_VSCREENINFO = 0x4600
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), FBIOGET_VSCREENINFO, uintptr(unsafe.Pointer(&v)))
	if errno != 0 {
		f.Close()
		return nil, errno
	}
	bpp := int(v.BitsPerPixel / 8)
	if bpp != 2 && bpp != 4 {
		f.Close()
		return nil, fmt.Errorf("unsupported framebuffer depth %d", v.BitsPerPixel)
	}
	stride := int(v.XresVirtual) * bpp
	size := stride * int(v.YresVirtual)
	data, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &framebuffer{f: f, data: data, w: int(v.Xres), h: int(v.Yres), stride: stride, bpp: bpp}, nil
}

func (fb *framebuffer) close() {
	if fb == nil {
		return
	}
	if fb.data != nil {
		_ = syscall.Munmap(fb.data)
		fb.data = nil
	}
	if fb.f != nil {
		_ = fb.f.Close()
		fb.f = nil
	}
}

func (fb *framebuffer) put(x, y int, c color.RGBA) {
	if x < 0 || y < 0 || x >= fb.w || y >= fb.h {
		return
	}
	off := y*fb.stride + x*fb.bpp
	if fb.bpp == 4 {
		fb.data[off] = c.B
		fb.data[off+1] = c.G
		fb.data[off+2] = c.R
		fb.data[off+3] = 0xff
	} else {
		v := uint16((uint16(c.R>>3) << 11) | (uint16(c.G>>2) << 5) | uint16(c.B>>3))
		fb.data[off] = byte(v)
		fb.data[off+1] = byte(v >> 8)
	}
}

func (fb *framebuffer) fill(c color.RGBA) {
	for y := 0; y < fb.h; y++ {
		for x := 0; x < fb.w; x++ {
			fb.put(x, y, c)
		}
	}
}

func loadImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	return img, err
}

func sample(img image.Image, sx, sy int) color.RGBA {
	r, g, b, a := img.At(sx, sy).RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), uint8(a >> 8)}
}

func blend(dst, src color.RGBA) color.RGBA {
	a := int(src.A)
	ia := 255 - a
	return color.RGBA{uint8((int(src.R)*a + int(dst.R)*ia) / 255), uint8((int(src.G)*a + int(dst.G)*ia) / 255), uint8((int(src.B)*a + int(dst.B)*ia) / 255), 255}
}

func (fb *framebuffer) get(x, y int) color.RGBA {
	off := y*fb.stride + x*fb.bpp
	if fb.bpp == 4 {
		return color.RGBA{fb.data[off+2], fb.data[off+1], fb.data[off], 255}
	}
	v := uint16(fb.data[off]) | uint16(fb.data[off+1])<<8
	return color.RGBA{uint8(((v >> 11) & 31) << 3), uint8(((v >> 5) & 63) << 2), uint8((v & 31) << 3), 255}
}

func (fb *framebuffer) drawImage(img image.Image, dx, dy, dw, dh int, contain bool) {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw < 1 || sh < 1 || dw < 1 || dh < 1 {
		return
	}
	tx, ty, tw, th := dx, dy, dw, dh
	if contain {
		scale := float64(dw) / float64(sw)
		if float64(sh)*scale > float64(dh) {
			scale = float64(dh) / float64(sh)
		}
		tw = int(float64(sw) * scale)
		th = int(float64(sh) * scale)
		tx = dx + (dw-tw)/2
		ty = dy + (dh-th)/2
	}
	for y := 0; y < th; y++ {
		sy := b.Min.Y + y*sh/th
		for x := 0; x < tw; x++ {
			sx := b.Min.X + x*sw/tw
			c := sample(img, sx, sy)
			if c.A < 255 {
				c = blend(fb.get(tx+x, ty+y), c)
			}
			fb.put(tx+x, ty+y, c)
		}
	}
}

func (fb *framebuffer) rect(x, y, w, h int, c color.RGBA) {
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			fb.put(xx, yy, c)
		}
	}
}
func (fb *framebuffer) border(x, y, w, h, t int, c color.RGBA) {
	fb.rect(x, y, w, t, c)
	fb.rect(x, y+h-t, w, t, c)
	fb.rect(x, y, t, h, c)
	fb.rect(x+w-t, y, t, h, c)
}

var font = map[rune][7]byte{
	'A': {14, 17, 17, 31, 17, 17, 17}, 'B': {30, 17, 17, 30, 17, 17, 30}, 'C': {14, 17, 16, 16, 16, 17, 14}, 'D': {30, 17, 17, 17, 17, 17, 30}, 'E': {31, 16, 16, 30, 16, 16, 31}, 'F': {31, 16, 16, 30, 16, 16, 16}, 'G': {14, 17, 16, 23, 17, 17, 15}, 'H': {17, 17, 17, 31, 17, 17, 17}, 'I': {14, 4, 4, 4, 4, 4, 14}, 'J': {7, 2, 2, 2, 18, 18, 12}, 'K': {17, 18, 20, 24, 20, 18, 17}, 'L': {16, 16, 16, 16, 16, 16, 31}, 'M': {17, 27, 21, 21, 17, 17, 17}, 'N': {17, 25, 21, 19, 17, 17, 17}, 'O': {14, 17, 17, 17, 17, 17, 14}, 'P': {30, 17, 17, 30, 16, 16, 16}, 'Q': {14, 17, 17, 17, 21, 18, 13}, 'R': {30, 17, 17, 30, 20, 18, 17}, 'S': {15, 16, 16, 14, 1, 1, 30}, 'T': {31, 4, 4, 4, 4, 4, 4}, 'U': {17, 17, 17, 17, 17, 17, 14}, 'V': {17, 17, 17, 17, 17, 10, 4}, 'W': {17, 17, 17, 21, 21, 21, 10}, 'X': {17, 17, 10, 4, 10, 17, 17}, 'Y': {17, 17, 10, 4, 4, 4, 4}, 'Z': {31, 1, 2, 4, 8, 16, 31},
	'0': {14, 17, 19, 21, 25, 17, 14}, '1': {4, 12, 4, 4, 4, 4, 14}, '2': {14, 17, 1, 2, 4, 8, 31}, '3': {30, 1, 1, 14, 1, 1, 30}, '4': {2, 6, 10, 18, 31, 2, 2}, '5': {31, 16, 16, 30, 1, 1, 30}, '6': {14, 16, 16, 30, 17, 17, 14}, '7': {31, 1, 2, 4, 8, 8, 8}, '8': {14, 17, 17, 14, 17, 17, 14}, '9': {14, 17, 17, 15, 1, 1, 14},
	'-': {0, 0, 0, 31, 0, 0, 0}, '_': {0, 0, 0, 0, 0, 0, 31}, '.': {0, 0, 0, 0, 0, 12, 12}, '/': {1, 2, 2, 4, 8, 8, 16}, ':': {0, 12, 12, 0, 12, 12, 0}, ' ': {0, 0, 0, 0, 0, 0, 0},
}

func (fb *framebuffer) text(x, y, scale int, s string, c color.RGBA) {
	s = strings.ToUpper(s)
	cx := x
	for _, ch := range s {
		glyph, ok := font[ch]
		if !ok {
			glyph = font[' ']
		}
		for gy, row := range glyph {
			for gx := 0; gx < 5; gx++ {
				if row&(1<<(4-gx)) != 0 {
					fb.rect(cx+gx*scale, y+gy*scale, scale, scale, c)
				}
			}
		}
		cx += 6 * scale
	}
}
func textWidth(scale int, s string) int { return len([]rune(s)) * 6 * scale }

func loadCollection(path string) (*Collection, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Collection
	if err = json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	c.Dir = filepath.Dir(path)
	if c.ID == "" {
		c.ID = filepath.Base(c.Dir)
	}
	if c.Title == "" {
		c.Title = c.ID
	}
	if c.Wallpaper == "" {
		return nil, fmt.Errorf("wallpaper is required")
	}
	if len(c.Entries) == 0 {
		return nil, fmt.Errorf("at least one entry is required")
	}
	for i, e := range c.Entries {
		if e.Label == "" || e.Artwork == "" {
			return nil, fmt.Errorf("entry %d requires label and artwork", i+1)
		}
		if e.Command == "" && (e.Launch.System == "" || e.Launch.Path == "") {
			return nil, fmt.Errorf("entry %d requires launch.system + launch.path (or legacy command)", i+1)
		}
	}
	return &c, nil
}

func scanCollections(base string) ([]*Collection, error) {
	root := filepath.Join(base, "Collections")
	os.MkdirAll(root, 0755)
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []*Collection
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		p := filepath.Join(root, d.Name(), "collection.json")
		c, err := loadCollection(p)
		if err == nil {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out, nil
}

func absPath(c *Collection, p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir, p)
}

type terminalState struct {
	fd   uintptr
	orig syscall.Termios
	ok   bool
	tty  *os.File
}

func silenceTerminal() *terminalState {
	state := &terminalState{}
	if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		state.tty = tty
		_, _ = tty.WriteString("\x1b[?25l")
	}

	fd := os.Stdin.Fd()
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&t)))
	if errno != 0 {
		return state
	}
	orig := t
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON
	t.Iflag &^= syscall.ICRNL | syscall.INLCR
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&t)))
	if errno != 0 {
		return state
	}
	state.fd = fd
	state.orig = orig
	state.ok = true
	return state
}

func (t *terminalState) Restore() {
	if t == nil {
		return
	}
	if t.ok {
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, t.fd, uintptr(syscall.TCSETS), uintptr(unsafe.Pointer(&t.orig)))
		t.ok = false
	}
	if t.tty != nil {
		_, _ = t.tty.WriteString("\x1b[?25h")
		_ = t.tty.Close()
		t.tty = nil
	}
}

func inputLoop(ch chan<- action, done <-chan struct{}) {
	matches, _ := filepath.Glob("/dev/input/event*")
	var wg sync.WaitGroup
	var emitMu sync.Mutex
	lastAction := actNone
	lastAt := time.Time{}
	emit := func(a action) {
		if a == actNone {
			return
		}
		emitMu.Lock()
		now := time.Now()

		if a == lastAction && now.Sub(lastAt) < 300*time.Millisecond {
			emitMu.Unlock()
			return
		}

		if lastAction == actConfirm && a == actBack && now.Sub(lastAt) < 180*time.Millisecond {
			emitMu.Unlock()
			return
		}
		lastAction = a
		lastAt = now
		emitMu.Unlock()
		select {
		case ch <- a:
		default:
		}
	}
	for _, p := range matches {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		const eviocgrab = 0x40044590
		_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(eviocgrab), uintptr(1))
		wg.Add(1)
		go func(f *os.File) {
			defer wg.Done()
			defer f.Close()
			var hatX int32
			var hatY int32
			pressed := make(map[uint16]bool)
			for {
				select {
				case <-done:
					return
				default:
				}
				var ev inputEvent
				err := binary.Read(f, binary.LittleEndian, &ev)
				if err != nil {
					if err != io.EOF {
						time.Sleep(50 * time.Millisecond)
					}
					return
				}
				var a action
				if ev.Type == evKey {
					if ev.Value == 0 {
						pressed[ev.Code] = false
					} else if ev.Value == 1 && !pressed[ev.Code] {
						pressed[ev.Code] = true
						switch ev.Code {
						case keyLeft:
							a = actLeft
						case keyRight:
							a = actRight
						case keyUp:
							a = actUp
						case keyDown:
							a = actDown
						case keyEnter, btnStart:
							a = actConfirm
						case keyEsc, keyBack:
							a = actBack
						case btnMode:
							a = actHome
						case btnSouth, btnEast:
							a = resolveFaceButton(ev.Code)
						}
					}
				}
				if ev.Type == evAbs {
					if ev.Code == absHatX && ev.Value != hatX {
						hatX = ev.Value
						if ev.Value < 0 {
							a = actLeft
						} else if ev.Value > 0 {
							a = actRight
						}
					}
					if ev.Code == absHatY && ev.Value != hatY {
						hatY = ev.Value
						if ev.Value < 0 {
							a = actUp
						} else if ev.Value > 0 {
							a = actDown
						}
					}
				}
				emit(a)
			}
		}(f)
	}
	wg.Wait()
}

type musicPlayer struct {
	cmd  *exec.Cmd
	stop chan struct{}
}

func startMusic(path string) *musicPlayer {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	if _, err := exec.LookPath("aplay"); err != nil {
		return nil
	}
	mp := &musicPlayer{stop: make(chan struct{})}
	go func() {
		for {
			select {
			case <-mp.stop:
				return
			default:
			}
			cmd := exec.Command("aplay", "-q", path)
			mp.cmd = cmd
			_ = cmd.Run()
		}
	}()
	return mp
}
func (m *musicPlayer) Stop() {
	if m == nil {
		return
	}
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
	}
}

func drawEmpty(fb *framebuffer) {
	fb.fill(color.RGBA{10, 10, 14, 255})
	white := color.RGBA{240, 240, 240, 255}
	gray := color.RGBA{180, 180, 180, 255}
	s := 4
	title := "GAME LAUNCHER"
	fb.text((fb.w-textWidth(s, title))/2, fb.h/3, s, title, white)
	s = 3
	msg := "NO GAME COLLECTIONS FOUND"
	fb.text((fb.w-textWidth(s, msg))/2, fb.h/2, s, msg, white)
	s = 2
	p := "ADD COLLECTIONS TO SCRIPTS/.CONFIG/GAMELAUNCHER/COLLECTIONS"
	fb.text((fb.w-textWidth(s, p))/2, fb.h/2+55, s, p, gray)
	b := "B / ESC  EXIT"
	fb.text((fb.w-textWidth(s, b))/2, fb.h-80, s, b, gray)
}

func drawBrowser(fb *framebuffer, cs []*Collection, sel int) {
	fb.fill(color.RGBA{10, 10, 14, 255})
	white := color.RGBA{245, 245, 245, 255}
	gray := color.RGBA{155, 155, 155, 255}
	hi := color.RGBA{255, 255, 255, 255}
	title := "GAME LAUNCHER"
	fb.text((fb.w-textWidth(4, title))/2, 60, 4, title, white)
	startY := 150
	rowH := 50
	maxRows := (fb.h - startY - 100) / rowH
	first := sel - maxRows/2
	if first < 0 {
		first = 0
	}
	if first+maxRows > len(cs) {
		first = len(cs) - maxRows
		if first < 0 {
			first = 0
		}
	}
	for i := first; i < len(cs) && i < first+maxRows; i++ {
		y := startY + (i-first)*rowH
		c := gray
		prefix := "  "
		if i == sel {

			fb.rect(72, y-9, fb.w-144, 40, color.RGBA{36, 36, 44, 255})
			fb.border(72, y-9, fb.w-144, 40, 3, hi)
			c = hi
			prefix = "> "
		}
		fb.text(100, y, 3, prefix+cs[i].Title, c)
	}
	fb.text(100, fb.h-60, 2, "DPAD  SELECT     A  OPEN     B  EXIT", gray)
}

type imageRect struct {
	x int
	y int
	w int
	h int
}

type collectionView struct {
	c              *Collection
	base           []byte
	cardX          []int
	cardY          int
	cardW          int
	cardH          int
	artRects       []imageRect
	artworks       []image.Image
	wallpaper      image.Image
	logo           image.Image
	viewportBase   []byte
	viewportStart  int
	viewportRects  map[int]imageRect
}

func captureVisible(fb *framebuffer) []byte {
	buf := make([]byte, fb.h*fb.stride)
	copy(buf, fb.data[:fb.h*fb.stride])
	return buf
}

func restoreVisible(fb *framebuffer, buf []byte) {
	if len(buf) < fb.h*fb.stride {
		return
	}
	copy(fb.data[:fb.h*fb.stride], buf[:fb.h*fb.stride])
}

func containedImageRect(img image.Image, dx, dy, dw, dh int) imageRect {
	if img == nil || dw < 1 || dh < 1 {
		return imageRect{x: dx, y: dy, w: dw, h: dh}
	}
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw < 1 || sh < 1 {
		return imageRect{x: dx, y: dy, w: dw, h: dh}
	}
	scale := float64(dw) / float64(sw)
	if float64(sh)*scale > float64(dh) {
		scale = float64(dh) / float64(sh)
	}
	w := int(float64(sw) * scale)
	h := int(float64(sh) * scale)
	return imageRect{
		x: dx + (dw-w)/2,
		y: dy + (dh-h)/2,
		w: w,
		h: h,
	}
}

func prepareCollectionView(fb *framebuffer, c *Collection) *collectionView {
	v := &collectionView{c: c}
	v.wallpaper, _ = loadImage(absPath(c, c.Wallpaper))
	if c.Logo != "" {
		v.logo, _ = loadImage(absPath(c, c.Logo))
	}
	v.artworks = make([]image.Image, len(c.Entries))
	for i, e := range c.Entries {
		v.artworks[i], _ = loadImage(absPath(c, e.Artwork))
	}

	if v.wallpaper != nil {
		fb.drawImage(v.wallpaper, 0, 0, fb.w, fb.h, false)
	} else {
		fb.fill(color.RGBA{8, 8, 12, 255})
	}

	if v.logo != nil {
		b := v.logo.Bounds()
		logoW := b.Dx()
		logoH := b.Dy()
		maxLogoW := 600
		maxLogoH := 200
		if maxLogoW > fb.w {
			maxLogoW = fb.w
		}
		if maxLogoH > fb.h/3 {
			maxLogoH = fb.h / 3
		}
		if logoW > maxLogoW || logoH > maxLogoH {
			scale := float64(maxLogoW) / float64(logoW)
			if float64(logoH)*scale > float64(maxLogoH) {
				scale = float64(maxLogoH) / float64(logoH)
			}
			logoW = int(float64(logoW) * scale)
			logoH = int(float64(logoH) * scale)
		}
		logoX := (fb.w - logoW) / 2
		logoY := fb.h / 24
		fb.drawImage(v.logo, logoX, logoY, logoW, logoH, true)
	}

	maxCardW := fb.w / 5
	if maxCardW > 500 {
		maxCardW = 500
	}
	v.cardH = fb.h / 2
	if v.cardH > 500 {
		v.cardH = 500
	}
	v.cardW = maxCardW

	usableBottom := fb.h - 100
	usableH := usableBottom
	if v.cardH > usableH {
		v.cardH = usableH
	}
	if v.cardH < 100 {
		v.cardH = 100
	}
	v.cardY = (usableH-v.cardH)/2

	fb.rect(0, fb.h-100, fb.w, 100, color.RGBA{0, 0, 0, 255})
	v.base = captureVisible(fb)
	v.viewportStart = -1
	v.viewportRects = make(map[int]imageRect)
	return v
}


func drawChevron(fb *framebuffer, centerX, centerY, size, thickness int, right bool, c color.RGBA) {
	for t := -thickness / 2; t <= thickness / 2; t++ {
		for i := 0; i < size; i++ {
			var x int
			if right {
				x = centerX - size/2 + i
			} else {
				x = centerX + size/2 - i
			}
			y1 := centerY - size/2 + i/2 + t
			y2 := centerY + size/2 - i/2 + t
			fb.put(x, y1, c)
			fb.put(x, y2, c)
		}
	}
}

func drawCollectionSelection(fb *framebuffer, v *collectionView, sel int, windowStart int) {
	n := len(v.c.Entries)
	if sel < 0 || sel >= n {
		return
	}

	visibleCount := n
	if visibleCount > 3 {
		visibleCount = 3
	}
	if windowStart < 0 {
		windowStart = 0
	}
	maxStart := n - visibleCount
	if maxStart < 0 {
		maxStart = 0
	}
	if windowStart > maxStart {
		windowStart = maxStart
	}

	if v.viewportBase == nil || v.viewportStart != windowStart {
		restoreVisible(fb, v.base)
		v.viewportRects = make(map[int]imageRect)

		gap := fb.w / 40
		cardW := v.cardW
		total := visibleCount*cardW + (visibleCount-1)*gap
		maxTotal := fb.w * 4 / 5
		if total > maxTotal {
			cardW = (maxTotal - (visibleCount-1)*gap) / visibleCount
			if cardW < 100 {
				cardW = 100
			}
			total = visibleCount*cardW + (visibleCount-1)*gap
		}
		startX := (fb.w - total) / 2

		for slot := 0; slot < visibleCount; slot++ {
			entryIndex := windowStart + slot
			if entryIndex >= n {
				break
			}
			x := startX + slot*(cardW+gap)
			r := imageRect{x: x, y: v.cardY, w: cardW, h: v.cardH}
			if v.artworks[entryIndex] != nil {
				r = containedImageRect(v.artworks[entryIndex], x, v.cardY, cardW, v.cardH)
				fb.drawImage(v.artworks[entryIndex], x, v.cardY, cardW, v.cardH, true)
			}
			v.viewportRects[entryIndex] = r
		}

		arrowY := v.cardY + v.cardH/2
		arrowSize := fb.h / 10
		if arrowSize > 90 {
			arrowSize = 90
		}
		if arrowSize < 40 {
			arrowSize = 40
		}
		arrowThickness := 7

		if windowStart > 0 {
			drawChevron(fb, startX/2, arrowY, arrowSize, arrowThickness, false, color.RGBA{255, 255, 255, 255})
		}
		if windowStart+visibleCount < n {
			rightSpaceStart := startX + total
			drawChevron(fb, rightSpaceStart+(fb.w-rightSpaceStart)/2, arrowY, arrowSize, arrowThickness, true, color.RGBA{255, 255, 255, 255})
		}

		v.viewportBase = captureVisible(fb)
		v.viewportStart = windowStart
	} else {
		restoreVisible(fb, v.viewportBase)
	}

	r, ok := v.viewportRects[sel]
	if !ok {
		return
	}
	pad := 5
	fb.border(r.x-pad, r.y-pad, r.w+pad*2, r.h+pad*2, 5, color.RGBA{255, 255, 255, 255})

	label := v.c.Entries[sel].Label
	fb.text((fb.w-textWidth(3, label))/2, fb.h-86, 3, label, color.RGBA{255, 255, 255, 255})
	help := "DPAD  SELECT     A  LAUNCH     B  BACK"
	fb.text((fb.w-textWidth(2, help))/2, fb.h-42, 2, help, color.RGBA{210, 210, 210, 255})
}

type mglConfig struct {
	RBF      string
	GamesDir string
	Delay    int
	Type     string
	Index    int
	SetName  string
}

func normalizeSystem(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	r := strings.NewReplacer(" ", "", "-", "", "_", "")
	return r.Replace(s)
}

func mglConfigForSystem(system string) (mglConfig, bool) {
	switch normalizeSystem(system) {
	case "PSX", "PS1", "PLAYSTATION", "PLAYSTATION1":
		return mglConfig{RBF: "_Console/PSX", GamesDir: "/media/fat/games/PSX", Delay: 1, Type: "s", Index: 1}, true
	case "SATURN", "SEGASATURN":
		return mglConfig{RBF: "_Console/Saturn", GamesDir: "/media/fat/games/Saturn", Delay: 1, Type: "s", Index: 0}, true
	case "SNES", "SUPERNINTENDO", "SUPERFAMICOM", "SFC":
		return mglConfig{RBF: "_Console/SNES", GamesDir: "/media/fat/games/SNES", Delay: 2, Type: "f", Index: 0}, true
	default:
		return mglConfig{}, false
	}
}

func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"\"", "&quot;",
		"<", "&lt;",
		">", "&gt;",
	)
	return r.Replace(s)
}

func resolveRBF(configured string) string {
	configured = filepath.ToSlash(configured)
	dir := filepath.Dir(configured)
	base := filepath.Base(configured)
	absDir := filepath.Join("/media/fat", dir)
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return configured
	}
	lowerBase := strings.ToLower(base)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".rbf") {
			continue
		}
		stem := strings.TrimSuffix(name, filepath.Ext(name))
		coreStem := stem
		if n := len(coreStem); n > 9 && coreStem[n-9] == '_' {
			digits := true
			for _, ch := range coreStem[n-8:] {
				if ch < '0' || ch > '9' {
					digits = false
					break
				}
			}
			if digits {
				coreStem = coreStem[:n-9]
			}
		}
		if strings.EqualFold(coreStem, base) || strings.EqualFold(stem, base) || strings.HasPrefix(strings.ToLower(stem), lowerBase+"_") {
			return filepath.ToSlash(filepath.Join(dir, coreStem))
		}
	}
	return configured
}

func generateMGL(system, romPath string) (string, error) {
	cfg, ok := mglConfigForSystem(system)
	if !ok {
		return "", fmt.Errorf("unsupported MGL system %q", system)
	}
	if !strings.HasPrefix(romPath, "/") {
		return "", fmt.Errorf("launch path must be absolute: %s", romPath)
	}
	if st, err := os.Stat(romPath); err != nil || st.IsDir() {
		if err != nil {
			return "", fmt.Errorf("game file not found: %s", romPath)
		}
		return "", fmt.Errorf("launch path is not a file: %s", romPath)
	}

	tmpDir := filepath.Join(runtimeBase, "tmp")
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", fmt.Errorf("create CollectionLauncher tmp directory: %w", err)
	}
	mglPath := filepath.Join(tmpDir, "CollectionLauncher.mgl")

	mediaPath := filepath.ToSlash(filepath.Clean(romPath))
	var b strings.Builder
	b.WriteString("<mistergamedescription>\n")
	resolvedRBF := resolveRBF(cfg.RBF)
	fmt.Fprintf(&b, "<rbf>%s</rbf>\n", xmlEscape(resolvedRBF))
	fmt.Fprintf(&b, "<file delay=\"%d\" type=\"%s\" index=\"%d\" path=\"%s\"/>\n",
		cfg.Delay, cfg.Type, cfg.Index, xmlEscape(mediaPath))
	if cfg.SetName != "" {
		fmt.Fprintf(&b, "<setname>%s</setname>\n", xmlEscape(cfg.SetName))
	}
	b.WriteString("</mistergamedescription>\n")
	if err := os.WriteFile(mglPath, []byte(b.String()), 0644); err != nil {
		return "", fmt.Errorf("write temporary MGL: %w", err)
	}
	return mglPath, nil
}

func appendLaunchLog(format string, args ...interface{}) {
	tmpDir := filepath.Join(runtimeBase, "tmp")
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(tmpDir, "CollectionLauncher.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, time.Now().Format("2006-01-02 15:04:05")+" "+format+"\n", args...)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func drainAllInputDevices(quiet time.Duration) {
	buf := make([]byte, 4096)
	quietUntil := time.Now().Add(quiet)
	hardDeadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(hardDeadline) {
		activity := false
		matches, _ := filepath.Glob("/dev/input/event*")
		for _, dev := range matches {
			fd, err := syscall.Open(dev, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
			if err != nil {
				continue
			}
			for {
				n, err := syscall.Read(fd, buf)
				if n > 0 {
					activity = true
				}
				if err != nil || n <= 0 {
					break
				}
			}
			_ = syscall.Close(fd)
		}

		if activity {
			quietUntil = time.Now().Add(quiet)
		}
		if time.Now().After(quietUntil) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func sendMiSTerLoadCore(path string) error {
	appendLaunchLog("pre_handoff_raw_input_drain begin")
	drainAllInputDevices(400 * time.Millisecond)
	appendLaunchLog("pre_handoff_raw_input_drain complete")

	cmd := "echo " + shellQuote("load_core "+path) + " > /dev/MiSTer_cmd"
	appendLaunchLog("shell handoff: %s", cmd)
	c := exec.Command("/bin/bash", "-lc", cmd)
	out, err := c.CombinedOutput()
	if len(out) > 0 {
		appendLaunchLog("handoff output: %s", strings.TrimSpace(string(out)))
	}
	if err != nil {
		return fmt.Errorf("MiSTer shell handoff failed: %w", err)
	}
	return nil
}

type coreState struct {
	Name    string
	ModTime time.Time
	Exists  bool
}

func readCoreState() coreState {
	const corePath = "/tmp/CORENAME"
	raw, err := os.ReadFile(corePath)
	if err != nil {
		return coreState{}
	}
	st, err := os.Stat(corePath)
	if err != nil {
		return coreState{Name: strings.TrimSpace(string(raw)), Exists: true}
	}
	return coreState{Name: strings.TrimSpace(string(raw)), ModTime: st.ModTime(), Exists: true}
}

func readCoreName() string {
	return readCoreState().Name
}

func coreNameMatchesSystem(coreName, system string) bool {
	name := normalizeSystem(coreName)
	switch normalizeSystem(system) {
	case "PSX", "PS1", "PLAYSTATION", "PLAYSTATION1":
		return strings.HasPrefix(name, "PSX")
	case "SATURN", "SEGASATURN":
		return strings.HasPrefix(name, "SATURN")
	case "SNES", "SUPERNINTENDO", "SUPERFAMICOM", "SFC":
		return strings.HasPrefix(name, "SNES")
	default:
		return false
	}
}

func waitForExpectedCore(system string, before coreState, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	stableSince := time.Time{}
	lastName := ""
	transitionSeen := false

	for time.Now().Before(deadline) {
		now := readCoreState()

		if now.Exists {
			if !before.Exists || now.Name != before.Name || (!now.ModTime.IsZero() && now.ModTime.After(before.ModTime)) {
				transitionSeen = true
			}
		}

		if transitionSeen && coreNameMatchesSystem(now.Name, system) {
			if now.Name != lastName {
				lastName = now.Name
				stableSince = time.Now()
			} else if !stableSince.IsZero() && time.Since(stableSince) >= 600*time.Millisecond {
				return now.Name, true
			}
		} else {
			lastName = now.Name
			stableSince = time.Time{}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return readCoreName(), false
}

func launchEntry(e Entry, music *musicPlayer, fb *framebuffer, term *terminalState) error {
	if e.Launch.System != "" && e.Launch.Path != "" {
		appendLaunchLog("selected system=%s path=%s", e.Launch.System, e.Launch.Path)
		mglPath, err := generateMGL(e.Launch.System, e.Launch.Path)
		if err != nil {
			appendLaunchLog("MGL generation failed: %v", err)
			return err
		}
		before := readCoreState()
		appendLaunchLog("launch system=%s path=%s mgl=%s core_before=%q core_before_mtime=%s", e.Launch.System, e.Launch.Path, mglPath, before.Name, before.ModTime.Format(time.RFC3339Nano))

		if err = sendMiSTerLoadCore(mglPath); err != nil {
			appendLaunchLog("MiSTer shell handoff failed: %v", err)
			return err
		}

		after, switched := waitForExpectedCore(e.Launch.System, before, 7*time.Second)
		appendLaunchLog("core_after=%q expected_system=%s switched=%v", after, e.Launch.System, switched)
		if !switched {
			return fmt.Errorf("MiSTer did not reach expected %s core (current %q)", e.Launch.System, after)
		}

		music.Stop()
		term.Restore()
		fb.close()
		os.Exit(0)
	}

	if e.Command != "" {
		music.Stop()
		term.Restore()
		fb.close()
		shell := "/bin/bash"
		return syscall.Exec(shell, []string{shell, "-lc", e.Command}, os.Environ())
	}
	return fmt.Errorf("entry has no launch target")
}

func validate(base string) int {
	cs, err := scanCollections(base)
	if err != nil {
		fmt.Println("ERROR:", err)
		return 1
	}
	if len(cs) == 0 {
		fmt.Println("No game collections found.")
		return 0
	}
	for _, c := range cs {
		fmt.Printf("OK  %s (%s) - %d entries\n", c.Title, c.ID, len(c.Entries))
	}
	return 0
}

func settleInput(ch <-chan action, quiet time.Duration) {

	t := time.NewTimer(quiet)
	defer t.Stop()
	deadline := time.NewTimer(900 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case <-ch:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(quiet)
		case <-t.C:
			return
		case <-deadline.C:
			return
		}
	}
}

func recoverFailedLaunch(ch <-chan action) {

	t := time.NewTimer(650 * time.Millisecond)
	defer t.Stop()
	deadline := time.NewTimer(2500 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case <-ch:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(650 * time.Millisecond)
		case <-t.C:
			return
		case <-deadline.C:
			return
		}
	}
}

func runGameMenu(fb *framebuffer, acts <-chan action, term *terminalState, c *Collection, mode string) {
	sel := 0
	windowStart := 0
	music := startMusic(absPath(c, c.Music))
	defer music.Stop()
	view := prepareCollectionView(fb, c)
	drawCollectionSelection(fb, view, sel, windowStart)
	appendLaunchLog("game_menu_enter mode=%s collection=%s entries=%d", mode, c.ID, len(c.Entries))

	settleInput(acts, 250*time.Millisecond)
	drawCollectionSelection(fb, view, sel, windowStart)
	menuReadyAt := time.Now()
	appendLaunchLog("game_menu_ready mode=%s collection=%s", mode, c.ID)

	for a := range acts {
		appendLaunchLog("game_menu_action mode=%s collection=%s action=%d selected=%d", mode, c.ID, a, sel)

		if mode == "direct" && a == actBack && time.Since(menuReadyAt) < 2*time.Second {
			appendLaunchLog("game_menu_ignore_startup_back mode=%s collection=%s age=%s", mode, c.ID, time.Since(menuReadyAt))
			continue
		}
		switch a {
		case actLeft, actUp:
			if sel > 0 {
				sel--
				if sel < windowStart {
					windowStart--
				}
				drawCollectionSelection(fb, view, sel, windowStart)
			}
		case actRight, actDown:
			if sel < len(c.Entries)-1 {
				sel++
				if sel > windowStart+2 {
					windowStart++
				}
				drawCollectionSelection(fb, view, sel, windowStart)
			}
		case actConfirm:
			launchTarget := c.Entries[sel]
			appendLaunchLog("confirm mode=%s collection=%s index=%d label=%q system=%s path=%s", mode, c.ID, sel, launchTarget.Label, launchTarget.Launch.System, launchTarget.Launch.Path)
			if err := launchEntry(launchTarget, music, fb, term); err != nil {
				appendLaunchLog("launch failed mode=%s: %v", mode, err)
				recoverFailedLaunch(acts)
				drawCollectionSelection(fb, view, sel, windowStart)
				continue
			}
			return
		case actBack:
			appendLaunchLog("game_menu_return_back mode=%s collection=%s selected=%d", mode, c.ID, sel)
			return
		case actHome:
			continue
		}
	}
}

func main() {
	base := defaultBase
	if v := os.Getenv("GAMELAUNCHER_BASE"); v != "" {
		base = v
	}
	runtimeBase = base
	_ = os.MkdirAll(filepath.Join(runtimeBase, "tmp"), 0755)
	appendLaunchLog("binary start version=%s args=%q base=%s", version, os.Args[1:], runtimeBase)
	loadFaceMapping()
	args := os.Args[1:]
	if len(args) > 0 && (args[0] == "--version" || args[0] == "-v") {
		fmt.Printf("CollectionLauncher v%s\n", version)
		return
	}
	if len(args) > 0 && args[0] == "--validate" {
		os.Exit(validate(base))
	}
	cs, err := scanCollections(base)
	if err != nil {
		fmt.Fprintln(os.Stderr, "CollectionLauncher:", err)
		os.Exit(1)
	}
	fb, err := openFramebuffer()
	if err != nil {
		fmt.Fprintln(os.Stderr, "CollectionLauncher: unable to open /dev/fb0:", err)
		fmt.Fprintln(os.Stderr, "Run with --validate to validate collection files.")
		os.Exit(1)
	}
	defer fb.close()
	term := silenceTerminal()
	defer term.Restore()
	acts := make(chan action, 16)
	done := make(chan struct{})
	defer close(done)
	go inputLoop(acts, done)

	if len(cs) == 0 {
		drawEmpty(fb)
		for a := range acts {
			if a == actBack || a == actConfirm {
				return
			}
		}
		return
	}

	settleInput(acts, 350*time.Millisecond)

	if len(args) > 0 {
		id := args[0]
		var c *Collection
		for _, x := range cs {
			if x.ID == id {
				c = x
				break
			}
		}
		if c == nil {
			drawEmpty(fb)
			for a := range acts {
				if a == actBack || a == actConfirm {
					return
				}
			}
			return
		}
		runGameMenu(fb, acts, term, c, "direct")
		return
	}

	bsel := 0
	drawBrowser(fb, cs, bsel)

	for a := range acts {
		switch a {
		case actUp, actLeft:
			bsel = (bsel - 1 + len(cs)) % len(cs)
			drawBrowser(fb, cs, bsel)
		case actDown, actRight:
			bsel = (bsel + 1) % len(cs)
			drawBrowser(fb, cs, bsel)
		case actBack:
			return
		case actConfirm:
			c := cs[bsel]
			runGameMenu(fb, acts, term, c, "browser")

			bsel = 0
			drawBrowser(fb, cs, bsel)
			settleInput(acts, 250*time.Millisecond)
			drawBrowser(fb, cs, bsel)
		}
	}
}
