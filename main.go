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

const version = "0.5.0"
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

type LaunchFile struct {
	Role string `json:"role,omitempty"`
	Path string `json:"path"`
}

type Launch struct {
	System string       `json:"system"`
	Path   string       `json:"path,omitempty"`
	Files  []LaunchFile `json:"files,omitempty"`
	RAM    string       `json:"ram,omitempty"`
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
		if e.Command == "" && (e.Launch.System == "" || (e.Launch.Path == "" && len(e.Launch.Files) == 0)) {
			return nil, fmt.Errorf("entry %d requires launch.system and launch.path or launch.files (or legacy command)", i+1)
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

type mglVariant struct {
	Role  string
	Label string
	Exts  []string
	Delay int
	Type  string
	Index int
}

type systemPreset struct {
	ID       string
	RBF      string
	Aliases  []string
	Variants []mglVariant
}

var systemPresets = []systemPreset{
	{
		ID: "AdventureVision", RBF: "_Console/AdventureVision",
		Aliases: []string{"AVision", "Adventure Vision"},
		Variants: []mglVariant{
			{Role: "game", Label: "Game", Exts: []string{".bin"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "Amiga", RBF: "_Computer/Minimig",
		Aliases: []string{"Minimig", "Amiga"},
		Variants: []mglVariant{
			{Role: "floppy1", Label: "df0", Exts: []string{".adf"}, Delay: 1, Type: "f", Index: 0},
		},
	},
	{
		ID: "AmigaCD32", RBF: "_Computer/Minimig",
		Aliases: []string{"AmigaCD32", "Amiga CD32"},
		Variants: []mglVariant{
			{Role: "cd", Label: "CD Image", Exts: []string{".cue", ".chd"}, Delay: 1, Type: "s", Index: 1},
		},
	},
	{
		ID: "Amstrad", RBF: "_Computer/Amstrad",
		Aliases: []string{"Amstrad CPC"},
		Variants: []mglVariant{
			{Role: "floppy1", Label: "A:", Exts: []string{".dsk"}, Delay: 1, Type: "s", Index: 0},
			{Role: "floppy2", Label: "B:", Exts: []string{".dsk"}, Delay: 1, Type: "s", Index: 1},
			{Role: "expansion", Label: "Expansion", Exts: []string{".e??"}, Delay: 1, Type: "f", Index: 3},
			{Role: "tape", Label: "Tape", Exts: []string{".cdt"}, Delay: 1, Type: "f", Index: 4},
		},
	},
	{
		ID: "AmstradPCW", RBF: "_Computer/Amstrad-PCW",
		Aliases: []string{"Amstrad-PCW", "Amstrad PCW"},
		Variants: []mglVariant{
			{Role: "floppy1", Label: "A:", Exts: []string{".dsk"}, Delay: 1, Type: "s", Index: 0},
			{Role: "floppy2", Label: "B:", Exts: []string{".dsk"}, Delay: 1, Type: "s", Index: 1},
		},
	},
	{
		ID: "Apogee", RBF: "_Computer/Apogee",
		Aliases: []string{"Apogee BK-01"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".rka", ".rkr", ".gam"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "AppleI", RBF: "_Computer/Apple-I",
		Aliases: []string{"Apple-I", "Apple I"},
		Variants: []mglVariant{
			{Role: "ascii", Label: "ASCII", Exts: []string{".txt"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "AppleII", RBF: "_Computer/Apple-II",
		Aliases: []string{"Apple-II", "Apple IIe"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".nib", ".dsk", ".do", ".po"}, Delay: 1, Type: "s", Index: 0},
			{Role: "game", Label: "-", Exts: []string{".hdv"}, Delay: 1, Type: "s", Index: 1},
		},
	},
	{
		ID: "Arcadia", RBF: "_Console/Arcadia",
		Aliases: []string{"Arcadia 2001"},
		Variants: []mglVariant{
			{Role: "cart", Label: "Cartridge", Exts: []string{".bin"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "Arduboy", RBF: "_Other/Arduboy",
		Aliases: []string{"Arduboy"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".bin", ".hex"}, Delay: 1, Type: "f", Index: 0},
		},
	},
	{
		ID: "Atari2600", RBF: "_Console/Atari7800",
		Aliases: []string{"Atari 2600"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".a26"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "Atari5200", RBF: "_Console/Atari5200",
		Aliases: []string{"Atari 5200"},
		Variants: []mglVariant{
			{Role: "cart", Label: "Cart", Exts: []string{".car", ".a52", ".bin", ".rom"}, Delay: 1, Type: "s", Index: 1},
		},
	},
	{
		ID: "Atari7800", RBF: "_Console/Atari7800",
		Aliases: []string{"Atari 7800"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".a78", ".bin"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "Atari800", RBF: "_Computer/Atari800",
		Aliases: []string{"Atari 800XL"},
		Variants: []mglVariant{
			{Role: "d1", Label: "D1", Exts: []string{".atr", ".xex", ".xfd", ".atx"}, Delay: 1, Type: "s", Index: 0},
			{Role: "d2", Label: "D2", Exts: []string{".atr", ".xex", ".xfd", ".atx"}, Delay: 1, Type: "s", Index: 1},
			{Role: "cart", Label: "Cartridge", Exts: []string{".car", ".rom", ".bin"}, Delay: 1, Type: "s", Index: 2},
		},
	},
	{
		ID: "AtariLynx", RBF: "_Console/AtariLynx",
		Aliases: []string{"Atari Lynx"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".lnx"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "AcornAtom", RBF: "_Computer/AcornAtom",
		Aliases: []string{"Atom"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".vhd"}, Delay: 1, Type: "s", Index: 1},
		},
	},
	{
		ID: "BBCMicro", RBF: "_Computer/BBCMicro",
		Aliases: []string{"BBC Micro/Master"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".vhd"}, Delay: 1, Type: "s", Index: 0},
			{Role: "game", Label: "-", Exts: []string{".ssd", ".dsd"}, Delay: 1, Type: "s", Index: 1},
			{Role: "game", Label: "-", Exts: []string{".ssd", ".dsd"}, Delay: 1, Type: "s", Index: 2},
		},
	},
	{
		ID: "BK0011M", RBF: "_Computer/BK0011M",
		Aliases: []string{"BK0011M"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".bin"}, Delay: 1, Type: "f", Index: 1},
			{Role: "fdda", Label: "FDD(A)", Exts: []string{".dsk"}, Delay: 1, Type: "s", Index: 1},
			{Role: "fddb", Label: "FDD(B)", Exts: []string{".dsk"}, Delay: 1, Type: "s", Index: 2},
			{Role: "hdd", Label: "HDD", Exts: []string{".vhd"}, Delay: 1, Type: "s", Index: 0},
		},
	},
	{
		ID: "Astrocade", RBF: "_Console/Astrocade",
		Aliases: []string{"Bally Astrocade"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".bin"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "CDI", RBF: "_Console/CDi",
		Aliases: []string{"CD-I"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".cue", ".chd"}, Delay: 1, Type: "s", Index: 1},
		},
	},
	{
		ID: "Chip8", RBF: "_Other/Chip8",
		Aliases: []string{"CHIP-8"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".ch8"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "CasioPV1000", RBF: "_Console/Casio_PV-1000",
		Aliases: []string{"Casio_PV-1000", "Casio PV-1000"},
		Variants: []mglVariant{
			{Role: "cart", Label: "Cartridge", Exts: []string{".bin"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "CasioPV2000", RBF: "_Computer/Casio_PV-2000",
		Aliases: []string{"Casio_PV-2000", "Casio PV-2000"},
		Variants: []mglVariant{
			{Role: "cart", Label: "Cartridge", Exts: []string{".bin"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "ChannelF", RBF: "_Console/ChannelF",
		Aliases: []string{"Channel F"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".rom", ".bin"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "ColecoVision", RBF: "_Console/ColecoVision",
		Aliases: []string{"Coleco", "ColecoVision"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".col", ".bin", ".rom"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "C16", RBF: "_Computer/C16",
		Aliases: []string{"Commodore 16"},
		Variants: []mglVariant{
			{Role: "8", Label: "#8", Exts: []string{".d64", ".g64"}, Delay: 1, Type: "s", Index: 0},
			{Role: "9", Label: "#9", Exts: []string{".d64", ".g64"}, Delay: 1, Type: "s", Index: 1},
			{Role: "game", Label: "-", Exts: []string{".prg", ".tap", ".bin"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "C64", RBF: "_Computer/C64",
		Aliases: []string{"Commodore 64"},
		Variants: []mglVariant{
			{Role: "8", Label: "#8", Exts: []string{".d64", ".g64", ".t64", ".d81"}, Delay: 1, Type: "s", Index: 0},
			{Role: "9", Label: "#9", Exts: []string{".d64", ".g64", ".t64", ".d81"}, Delay: 1, Type: "s", Index: 1},
			{Role: "game", Label: "-", Exts: []string{".prg", ".crt", ".reu", ".tap"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "PET2001", RBF: "_Computer/PET2001",
		Aliases: []string{"Commodore PET 2001"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".prg", ".tap"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "VIC20", RBF: "_Computer/VIC20",
		Aliases: []string{"Commodore VIC-20"},
		Variants: []mglVariant{
			{Role: "8", Label: "#8", Exts: []string{".d64", ".g64"}, Delay: 1, Type: "s", Index: 0},
			{Role: "9", Label: "#9", Exts: []string{".d64", ".g64"}, Delay: 1, Type: "s", Index: 1},
			{Role: "game", Label: "-", Exts: []string{".prg", ".crt", ".ct?", ".tap"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "EDSAC", RBF: "_Computer/EDSAC",
		Aliases: []string{"EDSAC"},
		Variants: []mglVariant{
			{Role: "tape", Label: "Tape", Exts: []string{".tap"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "AcornElectron", RBF: "_Computer/AcornElectron",
		Aliases: []string{"Electron"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".vhd"}, Delay: 1, Type: "s", Index: 0},
		},
	},
	{
		ID: "FDS", RBF: "_Console/NES",
		Aliases: []string{"FamicomDiskSystem", "Famicom Disk System"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".fds"}, Delay: 2, Type: "f", Index: 1},
		},
	},
	{
		ID: "Galaksija", RBF: "_Computer/Galaksija",
		Aliases: []string{"Galaksija"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".tap"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "Gamate", RBF: "_Console/Gamate",
		Aliases: []string{"Gamate"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".bin"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "GameNWatch", RBF: "_Console/GnW",
		Aliases: []string{"Game & Watch"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".bin"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "GameGear", RBF: "_Console/SMS",
		Aliases: []string{"GG", "Game Gear"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".gg"}, Delay: 1, Type: "f", Index: 2},
		},
	},
	{
		ID: "Gameboy", RBF: "_Console/Gameboy",
		Aliases: []string{"GB", "Gameboy"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".gb"}, Delay: 2, Type: "f", Index: 1},
		},
	},
	{
		ID: "Gameboy2P", RBF: "_Console/Gameboy2P",
		Aliases: []string{"Gameboy (2 Player)"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".gb", ".gbc"}, Delay: 2, Type: "f", Index: 1},
		},
	},
	{
		ID: "GBA", RBF: "_Console/GBA",
		Aliases: []string{"GameboyAdvance", "Gameboy Advance"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".gba"}, Delay: 2, Type: "f", Index: 1},
		},
	},
	{
		ID: "GBA2P", RBF: "_Console/GBA2P",
		Aliases: []string{"Gameboy Advance (2 Player)"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".gba"}, Delay: 2, Type: "f", Index: 1},
		},
	},
	{
		ID: "GameboyColor", RBF: "_Console/Gameboy",
		Aliases: []string{"GBC", "Gameboy Color"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".gbc"}, Delay: 2, Type: "f", Index: 1},
		},
	},
	{
		ID: "Genesis", RBF: "_Console/MegaDrive",
		Aliases: []string{"MegaDrive", "Genesis"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".bin", ".gen", ".md"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "Sega32X", RBF: "_Console/S32X",
		Aliases: []string{"S32X", "32X", "Genesis 32X"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".32x"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "Groovy", RBF: "_Utility/Groovy",
		Aliases: []string{"Groovy"},
		Variants: []mglVariant{
			{Role: "gmc", Label: "GMC", Exts: []string{".gmc"}, Delay: 3, Type: "f", Index: 1},
		},
	},
	{
		ID: "Intellivision", RBF: "_Console/Intellivision",
		Aliases: []string{"Intellivision"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".int", ".bin"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "Interact", RBF: "_Computer/Interact",
		Aliases: []string{"Interact"},
		Variants: []mglVariant{
			{Role: "tape", Label: "Tape", Exts: []string{".cin", ".k7"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "Jaguar", RBF: "_Console/Jaguar",
		Aliases: []string{"Jaguar"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".jag", ".j64", ".rom", ".bin"}, Delay: 1, Type: "s", Index: 1},
		},
	},
	{
		ID: "Jupiter", RBF: "_Computer/Jupiter",
		Aliases: []string{"Jupiter Ace"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".ace"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "Laser", RBF: "_Computer/Laser310",
		Aliases: []string{"Laser310", "Laser 350/500/700"},
		Variants: []mglVariant{
			{Role: "vzimage", Label: "VZ Image", Exts: []string{".vz"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "Lynx48", RBF: "_Computer/Lynx48",
		Aliases: []string{"Lynx 48/96K"},
		Variants: []mglVariant{
			{Role: "tape", Label: "Cassette", Exts: []string{".tap"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "SordM5", RBF: "_Computer/SordM5",
		Aliases: []string{"Sord M5", "M5"},
		Variants: []mglVariant{
			{Role: "rom", Label: "ROM", Exts: []string{".bin", ".rom"}, Delay: 1, Type: "f", Index: 1},
			{Role: "tape", Label: "Tape", Exts: []string{".cas"}, Delay: 1, Type: "f", Index: 2},
		},
	},
	{
		ID: "MSX", RBF: "_Computer/MSX",
		Aliases: []string{"MSX"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".vhd"}, Delay: 1, Type: "s", Index: 1},
		},
	},
	{
		ID: "MSX1", RBF: "_Computer/MSX1",
		Aliases: []string{"MSX1"},
		Variants: []mglVariant{
			{Role: "floppy1", Label: "Drive A:", Exts: []string{".dsk"}, Delay: 1, Type: "s", Index: 1},
			{Role: "slota", Label: "SLOT A", Exts: []string{".rom"}, Delay: 1, Type: "f", Index: 2},
			{Role: "slotb", Label: "SLOT B", Exts: []string{".rom"}, Delay: 1, Type: "f", Index: 3},
		},
	},
	{
		ID: "MacPlus", RBF: "_Computer/MacPlus",
		Aliases: []string{"Macintosh Plus"},
		Variants: []mglVariant{
			{Role: "prifloppy", Label: "Pri Floppy", Exts: []string{".dsk"}, Delay: 1, Type: "f", Index: 1},
			{Role: "secfloppy", Label: "Sec Floppy", Exts: []string{".dsk"}, Delay: 1, Type: "f", Index: 2},
			{Role: "scsi6", Label: "SCSI-6", Exts: []string{".img", ".vhd"}, Delay: 1, Type: "s", Index: 0},
			{Role: "scsi5", Label: "SCSI-5", Exts: []string{".img", ".vhd"}, Delay: 1, Type: "s", Index: 1},
		},
	},
	{
		ID: "Odyssey2", RBF: "_Console/Odyssey2",
		Aliases: []string{"Magnavox Odyssey2"},
		Variants: []mglVariant{
			{Role: "cart", Label: "Cartridge", Exts: []string{".bin"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "MasterSystem", RBF: "_Console/SMS",
		Aliases: []string{"SMS", "Master System"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".sms"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "Aquarius", RBF: "_Computer/Aquarius",
		Aliases: []string{"Mattel Aquarius"},
		Variants: []mglVariant{
			{Role: "cart", Label: "Cartridge", Exts: []string{".bin"}, Delay: 1, Type: "f", Index: 1},
			{Role: "tape", Label: "Tape", Exts: []string{".caq"}, Delay: 1, Type: "f", Index: 2},
		},
	},
	{
		ID: "MegaDuck", RBF: "_Console/Gameboy",
		Aliases: []string{"Mega Duck"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".bin"}, Delay: 2, Type: "f", Index: 1},
		},
	},
	{
		ID: "MultiComp", RBF: "_Computer/MultiComp",
		Aliases: []string{"MultiComp"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".img"}, Delay: 1, Type: "s", Index: 1},
		},
	},
	{
		ID: "NES", RBF: "_Console/NES",
		Aliases: []string{"NES"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".nes"}, Delay: 2, Type: "f", Index: 1},
		},
	},
	{
		ID: "NESMusic", RBF: "_Console/NES",
		Aliases: []string{"NES Music"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".nsf"}, Delay: 2, Type: "f", Index: 1},
		},
	},
	{
		ID: "NeoGeoCD", RBF: "_Console/NeoGeo",
		Aliases: []string{"Neo Geo CD"},
		Variants: []mglVariant{
			{Role: "cd", Label: "CD Image", Exts: []string{".cue", ".chd"}, Delay: 1, Type: "s", Index: 1},
		},
	},
	{
		ID: "NeoGeo", RBF: "_Console/NeoGeo",
		Aliases: []string{"Neo Geo MVS/AES"},
		Variants: []mglVariant{
			{Role: "cart", Label: "ROM set", Exts: []string{".neo"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "Nintendo64", RBF: "_Console/N64",
		Aliases: []string{"N64", "Nintendo 64"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".n64", ".z64"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "Orao", RBF: "_Computer/ORAO",
		Aliases: []string{"Orao"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".tap"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "Oric", RBF: "_Computer/Oric",
		Aliases: []string{"Oric"},
		Variants: []mglVariant{
			{Role: "floppy1", Label: "Drive A:", Exts: []string{".dsk"}, Delay: 1, Type: "s", Index: 0},
		},
	},
	{
		ID: "ao486", RBF: "_Computer/ao486",
		Aliases: []string{"PC (486SX)"},
		Variants: []mglVariant{
			{Role: "floppy1", Label: "Floppy A:", Exts: []string{".img", ".ima", ".vfd"}, Delay: 1, Type: "s", Index: 0},
			{Role: "hdd", Label: "IDE 0-0", Exts: []string{".vhd"}, Delay: 1, Type: "s", Index: 2},
			{Role: "cd", Label: "CD", Exts: []string{".iso"}, Delay: 1, Type: "s", Index: 4},
		},
	},
	{
		ID: "PCXT", RBF: "_Computer/PCXT",
		Aliases: []string{"PC/XT"},
		Variants: []mglVariant{
			{Role: "floppy1", Label: "Floppy A:", Exts: []string{".img", ".ima", ".vfd"}, Delay: 1, Type: "s", Index: 0},
			{Role: "floppy2", Label: "Floppy B:", Exts: []string{".img", ".ima", ".vfd"}, Delay: 1, Type: "s", Index: 1},
			{Role: "hdd", Label: "IDE 0-0", Exts: []string{".vhd"}, Delay: 1, Type: "s", Index: 2},
			{Role: "hdd2", Label: "IDE 0-1", Exts: []string{".vhd"}, Delay: 1, Type: "s", Index: 3},
		},
	},
	{
		ID: "PDP1", RBF: "_Computer/PDP1",
		Aliases: []string{"PDP-1"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".pdp", ".rim", ".bin"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "PMD85", RBF: "_Computer/PMD85",
		Aliases: []string{"PMD 85-2A"},
		Variants: []mglVariant{
			{Role: "rompack", Label: "ROM Pack", Exts: []string{".rmm"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "PSX", RBF: "_Console/PSX",
		Aliases: []string{"Playstation", "PS1"},
		Variants: []mglVariant{
			{Role: "cd", Label: "CD", Exts: []string{".cue", ".chd"}, Delay: 1, Type: "s", Index: 1},
			{Role: "exe", Label: "Exe", Exts: []string{".exe"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "PocketChallengeV2", RBF: "_Console/WonderSwan",
		Aliases: []string{"Pocket Challenge V2"},
		Variants: []mglVariant{
			{Role: "rom", Label: "ROM", Exts: []string{".pc2"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "PokemonMini", RBF: "_Console/PokemonMini",
		Aliases: []string{"Pokemon Mini"},
		Variants: []mglVariant{
			{Role: "rom", Label: "ROM", Exts: []string{".min"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "RX78", RBF: "_Computer/RX78",
		Aliases: []string{"RX-78 Gundam"},
		Variants: []mglVariant{
			{Role: "cart", Label: "Cartridge", Exts: []string{".bin"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "SAMCoupe", RBF: "_Computer/SAMCoupe",
		Aliases: []string{"SAM Coupe"},
		Variants: []mglVariant{
			{Role: "drive1", Label: "Drive 1", Exts: []string{".dsk", ".mgt", ".img"}, Delay: 1, Type: "s", Index: 0},
			{Role: "drive2", Label: "Drive 2", Exts: []string{".dsk", ".mgt", ".img"}, Delay: 1, Type: "s", Index: 1},
		},
	},
	{
		ID: "SG1000", RBF: "_Console/ColecoVision",
		Aliases: []string{"SG-1000"},
		Variants: []mglVariant{
			{Role: "sg1000", Label: "SG-1000", Exts: []string{".sg"}, Delay: 1, Type: "f", Index: 0},
		},
	},
	{
		ID: "SNES", RBF: "_Console/SNES",
		Aliases: []string{"SuperNintendo", "SNES"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".sfc", ".smc", ".bin", ".bs"}, Delay: 2, Type: "f", Index: 0},
		},
	},
	{
		ID: "SNESMusic", RBF: "_Console/SNES",
		Aliases: []string{"SNES Music"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".spc"}, Delay: 2, Type: "f", Index: 1},
		},
	},
	{
		ID: "SVI328", RBF: "_Computer/Svi328",
		Aliases: []string{"SV-328"},
		Variants: []mglVariant{
			{Role: "cart", Label: "Cartridge", Exts: []string{".bin", ".rom"}, Delay: 1, Type: "f", Index: 1},
			{Role: "casfile", Label: "CAS File", Exts: []string{".cas"}, Delay: 1, Type: "f", Index: 2},
		},
	},
	{
		ID: "Saturn", RBF: "_Console/Saturn",
		Aliases: []string{"Saturn"},
		Variants: []mglVariant{
			{Role: "disk", Label: "Disk", Exts: []string{".cue", ".chd"}, Delay: 1, Type: "s", Index: 0},
		},
	},
	{
		ID: "MegaCD", RBF: "_Console/MegaCD",
		Aliases: []string{"SegaCD", "Sega CD"},
		Variants: []mglVariant{
			{Role: "disk", Label: "Disk", Exts: []string{".cue", ".chd"}, Delay: 1, Type: "s", Index: 0},
		},
	},
	{
		ID: "QL", RBF: "_Computer/QL",
		Aliases: []string{"Sinclair QL"},
		Variants: []mglVariant{
			{Role: "hdd", Label: "HD Image", Exts: []string{".win"}, Delay: 1, Type: "s", Index: 0},
			{Role: "mdvimage", Label: "MDV Image", Exts: []string{".mdv"}, Delay: 1, Type: "f", Index: 2},
		},
	},
	{
		ID: "Specialist", RBF: "_Computer/Specialist",
		Aliases: []string{"SPMX", "Specialist/MX"},
		Variants: []mglVariant{
			{Role: "tape", Label: "Tape", Exts: []string{".rks"}, Delay: 1, Type: "f", Index: 0},
			{Role: "disk", Label: "Disk", Exts: []string{".odi"}, Delay: 1, Type: "s", Index: 0},
		},
	},
	{
		ID: "SuperGameboy", RBF: "_Console/SGB",
		Aliases: []string{"SGB", "Super Gameboy"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".gb", ".gbc"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "SuperGrafx", RBF: "_Console/TurboGrafx16",
		Aliases: []string{"SuperGrafx"},
		Variants: []mglVariant{
			{Role: "supergrafx", Label: "SuperGrafx", Exts: []string{".sgx"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "SuperVision", RBF: "_Console/SuperVision",
		Aliases: []string{"SuperVision"},
		Variants: []mglVariant{
			{Role: "cart", Label: "Cartridge", Exts: []string{".bin", ".sv"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "TI994A", RBF: "_Computer/Ti994a",
		Aliases: []string{"TI-99_4A", "TI-99/4A"},
		Variants: []mglVariant{
			{Role: "fullcart", Label: "Full Cart", Exts: []string{".m99", ".bin"}, Delay: 1, Type: "f", Index: 1},
			{Role: "romcart", Label: "ROM Cart", Exts: []string{".bin"}, Delay: 1, Type: "f", Index: 2},
			{Role: "gromcart", Label: "GROM Cart", Exts: []string{".bin"}, Delay: 1, Type: "f", Index: 3},
		},
	},
	{
		ID: "TRS80", RBF: "_Computer/TRS-80",
		Aliases: []string{"TRS-80"},
		Variants: []mglVariant{
			{Role: "floppy1", Label: "Disk 0", Exts: []string{".dsk", ".jvi"}, Delay: 1, Type: "s", Index: 0},
			{Role: "floppy2", Label: "Disk 1", Exts: []string{".dsk", ".jvi"}, Delay: 1, Type: "s", Index: 1},
			{Role: "program", Label: "Program", Exts: []string{".cmd"}, Delay: 1, Type: "f", Index: 2},
			{Role: "tape", Label: "Cassette", Exts: []string{".cas"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "CoCo2", RBF: "_Computer/CoCo2",
		Aliases: []string{"TRS-80 CoCo 2"},
		Variants: []mglVariant{
			{Role: "cart", Label: "Cartridge", Exts: []string{".rom", ".ccc"}, Delay: 1, Type: "f", Index: 1},
			{Role: "diskdrive0", Label: "Disk Drive 0", Exts: []string{".dsk"}, Delay: 1, Type: "s", Index: 0},
			{Role: "diskdrive1", Label: "Disk Drive 1", Exts: []string{".dsk"}, Delay: 1, Type: "s", Index: 1},
			{Role: "diskdrive2", Label: "Disk Drive 2", Exts: []string{".dsk"}, Delay: 1, Type: "s", Index: 2},
			{Role: "diskdrive3", Label: "Disk Drive 3", Exts: []string{".dsk"}, Delay: 1, Type: "s", Index: 3},
			{Role: "tape", Label: "Cassette", Exts: []string{".cas"}, Delay: 1, Type: "f", Index: 2},
		},
	},
	{
		ID: "ZX81", RBF: "_Computer/ZX81",
		Aliases: []string{"TS-1500"},
		Variants: []mglVariant{
			{Role: "tape", Label: "Tape", Exts: []string{".0", ".p"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "TSConf", RBF: "_Computer/TSConf",
		Aliases: []string{"TS-Config"},
		Variants: []mglVariant{
			{Role: "virtualsd", Label: "Virtual SD", Exts: []string{".vhd"}, Delay: 1, Type: "s", Index: 0},
		},
	},
	{
		ID: "AliceMC10", RBF: "_Computer/AliceMC10",
		Aliases: []string{"Tandy MC-10"},
		Variants: []mglVariant{
			{Role: "tape", Label: "Tape", Exts: []string{".c10"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "TatungEinstein", RBF: "_Computer/TatungEinstein",
		Aliases: []string{"Tatung Einstein"},
		Variants: []mglVariant{
			{Role: "floppy1", Label: "Disk 0", Exts: []string{".dsk"}, Delay: 1, Type: "s", Index: 0},
		},
	},
	{
		ID: "TurboGrafx16", RBF: "_Console/TurboGrafx16",
		Aliases: []string{"TGFX16", "PCEngine", "TurboGrafx-16"},
		Variants: []mglVariant{
			{Role: "turbografx", Label: "TurboGrafx", Exts: []string{".bin", ".pce"}, Delay: 1, Type: "f", Index: 0},
		},
	},
	{
		ID: "TurboGrafx16CD", RBF: "_Console/TurboGrafx16",
		Aliases: []string{"TGFX16-CD", "PCEngineCD", "TurboGrafx-16 CD"},
		Variants: []mglVariant{
			{Role: "cd", Label: "CD", Exts: []string{".cue", ".chd"}, Delay: 1, Type: "s", Index: 0},
		},
	},
	{
		ID: "TomyTutor", RBF: "_Computer/TomyTutor",
		Aliases: []string{"Tutor"},
		Variants: []mglVariant{
			{Role: "cart", Label: "Cartridge", Exts: []string{".bin"}, Delay: 1, Type: "f", Index: 2},
			{Role: "tapeimage", Label: "Tape Image", Exts: []string{".cas"}, Delay: 1, Type: "s", Index: 0},
		},
	},
	{
		ID: "UK101", RBF: "_Computer/UK101",
		Aliases: []string{"UK101"},
		Variants: []mglVariant{
			{Role: "ascii", Label: "ASCII", Exts: []string{".txt", ".bas", ".lod"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "VC4000", RBF: "_Console/VC4000",
		Aliases: []string{"VC4000"},
		Variants: []mglVariant{
			{Role: "cart", Label: "Cartridge", Exts: []string{".bin"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "CreatiVision", RBF: "_Console/CreatiVision",
		Aliases: []string{"VTech CreatiVision"},
		Variants: []mglVariant{
			{Role: "cart", Label: "Cartridge", Exts: []string{".rom", ".bin"}, Delay: 1, Type: "f", Index: 1},
			{Role: "basic", Label: "BASIC", Exts: []string{".bas"}, Delay: 1, Type: "f", Index: 3},
		},
	},
	{
		ID: "Vector06C", RBF: "_Computer/Vector-06C",
		Aliases: []string{"Vector06", "Vector-06C"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".rom", ".com", ".c00", ".edd"}, Delay: 1, Type: "f", Index: 1},
			{Role: "diska", Label: "Disk A", Exts: []string{".fdd"}, Delay: 1, Type: "s", Index: 0},
			{Role: "diskb", Label: "Disk B", Exts: []string{".fdd"}, Delay: 1, Type: "s", Index: 1},
		},
	},
	{
		ID: "Vectrex", RBF: "_Console/Vectrex",
		Aliases: []string{"Vectrex"},
		Variants: []mglVariant{
			{Role: "game", Label: "-", Exts: []string{".vec", ".bin", ".rom"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "WonderSwan", RBF: "_Console/WonderSwan",
		Aliases: []string{"WonderSwan"},
		Variants: []mglVariant{
			{Role: "rom", Label: "ROM", Exts: []string{".ws"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "WonderSwanColor", RBF: "_Console/WonderSwan",
		Aliases: []string{"WonderSwan Color"},
		Variants: []mglVariant{
			{Role: "rom", Label: "ROM", Exts: []string{".wsc"}, Delay: 1, Type: "f", Index: 1},
		},
	},
	{
		ID: "X68000", RBF: "_Computer/X68000",
		Aliases: []string{"X68000"},
		Variants: []mglVariant{
			{Role: "floppy1", Label: "FDD0", Exts: []string{".d88"}, Delay: 1, Type: "s", Index: 0},
			{Role: "floppy2", Label: "FDD1", Exts: []string{".d88"}, Delay: 1, Type: "s", Index: 1},
			{Role: "hdd", Label: "SASI Hard Disk", Exts: []string{".hdf"}, Delay: 1, Type: "s", Index: 2},
			{Role: "ram", Label: "RAM", Exts: []string{".ram"}, Delay: 1, Type: "s", Index: 3},
		},
	},
	{
		ID: "ZXSpectrum", RBF: "_Computer/ZX-Spectrum",
		Aliases: []string{"Spectrum", "ZX Spectrum"},
		Variants: []mglVariant{
			{Role: "disk", Label: "Disk", Exts: []string{".trd", ".img", ".dsk", ".mgt"}, Delay: 1, Type: "s", Index: 0},
			{Role: "tape", Label: "Tape", Exts: []string{".tap", ".csw", ".tzx"}, Delay: 1, Type: "f", Index: 2},
			{Role: "snapshot", Label: "Snapshot", Exts: []string{".z80", ".sna"}, Delay: 1, Type: "f", Index: 4},
			{Role: "divmmc", Label: "DivMMC", Exts: []string{".vhd"}, Delay: 1, Type: "s", Index: 1},
		},
	},
	{
		ID: "ZXNext", RBF: "_Computer/ZXNext",
		Aliases: []string{"ZX Spectrum Next"},
		Variants: []mglVariant{
			{Role: "c", Label: "C:", Exts: []string{".vhd"}, Delay: 1, Type: "s", Index: 0},
			{Role: "d", Label: "D:", Exts: []string{".vhd"}, Delay: 1, Type: "s", Index: 1},
			{Role: "tape", Label: "Tape", Exts: []string{".tzx", ".csw"}, Delay: 1, Type: "f", Index: 1},
		},
	},
}

func normalizeSystem(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	r := strings.NewReplacer(" ", "", "-", "", "_", "", "/", "", "(", "", ")", "", ".", "", "&", "AND")
	return r.Replace(s)
}

func findSystemPreset(system string) (*systemPreset, bool) {
	want := normalizeSystem(system)
	for i := range systemPresets {
		p := &systemPresets[i]
		if normalizeSystem(p.ID) == want {
			return p, true
		}
		for _, a := range p.Aliases {
			if normalizeSystem(a) == want {
				return p, true
			}
		}
	}
	switch want {
	case "PSX", "PS1", "PLAYSTATION1":
		return findSystemPreset("Playstation")
	case "SEGASATURN":
		return findSystemPreset("Saturn")
	case "SUPERFAMICOM", "SFC":
		return findSystemPreset("SNES")
	case "MEGADRIVE":
		return findSystemPreset("Genesis")
	}
	return nil, false
}

func extensionMatches(path, pattern string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	if pattern == ext {
		return true
	}
	if strings.Contains(pattern, "?") && len(pattern) == len(ext) {
		for i := range pattern {
			if pattern[i] != '?' && pattern[i] != ext[i] {
				return false
			}
		}
		return true
	}
	return false
}

func roleMatches(want string, v mglVariant) bool {
	want = normalizeSystem(want)
	if want == "" {
		return true
	}
	if normalizeSystem(v.Role) == want || normalizeSystem(v.Label) == want {
		return true
	}
	switch want {
	case "FLOPPY", "FLOPPY1", "DISK1", "DRIVEA", "A":
		return v.Role == "floppy1"
	case "FLOPPY2", "DISK2", "DRIVEB", "B":
		return v.Role == "floppy2"
	case "HDD", "HARDDISK", "HARDDSK":
		return v.Role == "hdd"
	case "HDD2", "HARDDISK2":
		return v.Role == "hdd2"
	case "CD", "CDROM", "DISC":
		return v.Role == "cd"
	case "CART", "CARTRIDGE", "ROM":
		return v.Role == "cart"
	case "TAPE", "CASSETTE":
		return v.Role == "tape"
	case "SNAPSHOT":
		return v.Role == "snapshot"
	}
	return false
}

func variantForPath(p *systemPreset, path, role string) (mglVariant, error) {
	var matches []mglVariant
	for _, v := range p.Variants {
		if role != "" && !roleMatches(role, v) {
			continue
		}
		for _, ext := range v.Exts {
			if extensionMatches(path, ext) {
				matches = append(matches, v)
				break
			}
		}
	}
	if len(matches) == 0 {
		if role != "" {
			return mglVariant{}, fmt.Errorf("system %s has no %s slot for %s", p.ID, role, filepath.Ext(path))
		}
		return mglVariant{}, fmt.Errorf("system %s does not support %s files", p.ID, filepath.Ext(path))
	}
	return matches[0], nil
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

func validateLaunchPath(path string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("launch path must be absolute: %s", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("game file not found: %s", path)
	}
	if st.IsDir() {
		return "", fmt.Errorf("launch path is not a file: %s", path)
	}
	return filepath.ToSlash(filepath.Clean(path)), nil
}

func prepareSaturnRAM(ram string) (string, error) {
	value := byte(0)
	name := "Saturn_CL_None"
	switch normalizeSystem(ram) {
	case "", "NONE":
	case "1MB", "1M", "DRAM1M":
		value = 2
		name = "Saturn_CL_1MB"
	case "4MB", "4M", "DRAM4M":
		value = 3
		name = "Saturn_CL_4MB"
	default:
		return "", fmt.Errorf("unsupported Saturn RAM setting %q; use none, 1MB or 4MB", ram)
	}

	cfg := make([]byte, 16)
	basePath := "/media/fat/config/Saturn.CFG"
	if raw, err := os.ReadFile(basePath); err == nil {
		copy(cfg, raw)
	}
	cfg[2] = (cfg[2] & byte(0x1F)) | byte((value&7)<<5)

	configDir := "/media/fat/config"
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return "", fmt.Errorf("create MiSTer config directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, name+".CFG"), cfg, 0644); err != nil {
		return "", fmt.Errorf("write Saturn RAM profile: %w", err)
	}
	return name, nil
}

func generateMGL(launch Launch) (string, *systemPreset, error) {
	preset, ok := findSystemPreset(launch.System)
	if !ok {
		return "", nil, fmt.Errorf("unsupported MGL system %q", launch.System)
	}
	if normalizeSystem(preset.ID) == "ARCADE" {
		return "", nil, fmt.Errorf("Arcade cannot be launched through MGL")
	}
	if launch.RAM != "" && normalizeSystem(preset.ID) != "SATURN" {
		return "", nil, fmt.Errorf("ram is only supported for Saturn")
	}

	type resolvedFile struct {
		Path    string
		Variant mglVariant
	}
	var resolved []resolvedFile

	if launch.Path != "" {
		p, err := validateLaunchPath(launch.Path)
		if err != nil {
			return "", nil, err
		}
		v, err := variantForPath(preset, p, "")
		if err != nil {
			return "", nil, err
		}
		resolved = append(resolved, resolvedFile{Path: p, Variant: v})
	}

	for _, f := range launch.Files {
		p, err := validateLaunchPath(f.Path)
		if err != nil {
			return "", nil, err
		}
		v, err := variantForPath(preset, p, f.Role)
		if err != nil {
			return "", nil, err
		}
		resolved = append(resolved, resolvedFile{Path: p, Variant: v})
	}

	if len(resolved) == 0 {
		return "", nil, fmt.Errorf("launch has no files")
	}

	setName := ""
	setNameSameDir := false
	switch normalizeSystem(preset.ID) {
	case "SATURN":
		var err error
		setName, err = prepareSaturnRAM(launch.RAM)
		if err != nil {
			return "", nil, err
		}
		setNameSameDir = true
	case "GAMEGEAR":
		setName = "GameGear"
	case "GAMEBOYCOLOR":
		setName = "GBC"
		setNameSameDir = true
	}

	tmpDir := filepath.Join(runtimeBase, "tmp")
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return "", nil, fmt.Errorf("create CollectionLauncher tmp directory: %w", err)
	}
	mglPath := filepath.Join(tmpDir, "CollectionLauncher.mgl")

	var b strings.Builder
	b.WriteString("<mistergamedescription>\n")
	fmt.Fprintf(&b, "<rbf>%s</rbf>\n", xmlEscape(resolveRBF(preset.RBF)))
	if setName != "" {
		if setNameSameDir {
			fmt.Fprintf(&b, "<setname same_dir=\"1\">%s</setname>\n", xmlEscape(setName))
		} else {
			fmt.Fprintf(&b, "<setname>%s</setname>\n", xmlEscape(setName))
		}
	}
	for _, rf := range resolved {
		fmt.Fprintf(&b, "<file delay=\"%d\" type=\"%s\" index=\"%d\" path=\"%s\"/>\n",
			rf.Variant.Delay, rf.Variant.Type, rf.Variant.Index, xmlEscape(rf.Path))
	}
	b.WriteString("</mistergamedescription>\n")
	if err := os.WriteFile(mglPath, []byte(b.String()), 0644); err != nil {
		return "", nil, fmt.Errorf("write temporary MGL: %w", err)
	}
	return mglPath, preset, nil
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

func coreNameMatchesPreset(coreName string, preset *systemPreset) bool {
	if preset == nil {
		return false
	}
	name := normalizeSystem(coreName)
	if name == "" {
		return false
	}
	candidates := []string{preset.ID, filepath.Base(preset.RBF)}
	candidates = append(candidates, preset.Aliases...)
	for _, c := range candidates {
		n := normalizeSystem(c)
		if n != "" && strings.HasPrefix(name, n) {
			return true
		}
	}
	if normalizeSystem(preset.ID) == "SATURN" && strings.HasPrefix(name, "SATURNCL") {
		return true
	}
	return false
}

func waitForExpectedCore(preset *systemPreset, before coreState, timeout time.Duration) (string, bool) {
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

		if transitionSeen && coreNameMatchesPreset(now.Name, preset) {
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

func isArcadeSystem(system string) bool {
	switch normalizeSystem(system) {
	case "ARCADE", "MRA":
		return true
	default:
		return false
	}
}

func validateArcadePath(path string) (string, error) {
	p, err := validateLaunchPath(path)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(filepath.Ext(p), ".mra") {
		return "", fmt.Errorf("Arcade launch path must be an .mra file: %s", path)
	}
	return p, nil
}

func waitForArcadeCore(before coreState, timeout time.Duration) (string, bool) {
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

		name := normalizeSystem(now.Name)
		if transitionSeen && (name == "ARCADE" || strings.HasPrefix(name, "ARCADE")) {
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
	if e.Launch.System != "" && (e.Launch.Path != "" || len(e.Launch.Files) > 0) {
		if isArcadeSystem(e.Launch.System) {
			if len(e.Launch.Files) > 0 {
				return fmt.Errorf("Arcade uses a single MRA path, not launch.files")
			}
			if e.Launch.RAM != "" {
				return fmt.Errorf("ram is not supported for Arcade")
			}

			mraPath, err := validateArcadePath(e.Launch.Path)
			if err != nil {
				appendLaunchLog("Arcade path validation failed: %v", err)
				return err
			}

			before := readCoreState()
			appendLaunchLog("arcade launch mra=%s core_before=%q core_before_mtime=%s", mraPath, before.Name, before.ModTime.Format(time.RFC3339Nano))

			if err = sendMiSTerLoadCore(mraPath); err != nil {
				appendLaunchLog("Arcade MiSTer handoff failed: %v", err)
				return err
			}

			after, switched := waitForArcadeCore(before, 10*time.Second)
			appendLaunchLog("arcade core_after=%q switched=%v", after, switched)
			if !switched {
				return fmt.Errorf("MiSTer did not reach an Arcade core (current %q)", after)
			}

			music.Stop()
			term.Restore()
			fb.close()
			os.Exit(0)
		}

		appendLaunchLog("selected system=%s path=%s files=%d ram=%s", e.Launch.System, e.Launch.Path, len(e.Launch.Files), e.Launch.RAM)
		mglPath, preset, err := generateMGL(e.Launch)
		if err != nil {
			appendLaunchLog("MGL generation failed: %v", err)
			return err
		}
		before := readCoreState()
		appendLaunchLog("launch system=%s mgl=%s core_before=%q core_before_mtime=%s", e.Launch.System, mglPath, before.Name, before.ModTime.Format(time.RFC3339Nano))

		if err = sendMiSTerLoadCore(mglPath); err != nil {
			appendLaunchLog("MiSTer shell handoff failed: %v", err)
			return err
		}

		after, switched := waitForExpectedCore(preset, before, 7*time.Second)
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
