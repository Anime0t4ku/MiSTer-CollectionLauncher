package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const (
	misterMapEntries = 32
	misterKeyMax     = 0x2ff
	misterKeyEmu     = misterKeyMax + 1
	misterBtnRight   = 0
	misterBtnLeft    = 1
	misterBtnDown    = 2
	misterBtnUp      = 3
	misterBtnA       = 4
	misterBtnB       = 5
	misterBtnX       = 6
	misterBtnY       = 7
	misterBtnSelect  = 10
)

type inputAbsInfo struct{ Value, Minimum, Maximum, Fuzz, Flat, Resolution int32 }

type misterControllerMap struct {
	values    [misterMapEntries]uint32
	axisInfo  map[uint16]inputAbsInfo
	axisState map[uint16]int
	pressed   map[uint16]bool
}

func readHexInputID(eventPath, field string) (uint16, error) {
	name := filepath.Base(eventPath)
	b, err := os.ReadFile(filepath.Join("/sys/class/input", name, "device/id", field))
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 16, 16)
	return uint16(v), err
}

func findMisterMapPath(eventPath string) string {
	vid, err := readHexInputID(eventPath, "vendor")
	if err != nil {
		return ""
	}
	pid, err := readHexInputID(eventPath, "product")
	if err != nil {
		return ""
	}
	dir := "/media/fat/config/inputs"
	exact := filepath.Join(dir, fmt.Sprintf("input_%04x_%04x_v3.map", vid, pid))
	if st, err := os.Stat(exact); err == nil && !st.IsDir() {
		return exact
	}
	patterns := []string{
		filepath.Join(dir, fmt.Sprintf("input_%04x_%04x_*_v3.map", vid, pid)),
		filepath.Join(dir, fmt.Sprintf("input_%04x_%04x*_v3.map", vid, pid)),
	}
	for _, pattern := range patterns {
		if matches, _ := filepath.Glob(pattern); len(matches) != 0 {
			return matches[0]
		}
	}
	return ""
}

func loadMisterControllerMap(eventPath string) *misterControllerMap {
	path := findMisterMapPath(eventPath)
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) < misterMapEntries*4 {
		return nil
	}
	m := &misterControllerMap{axisInfo: map[uint16]inputAbsInfo{}, axisState: map[uint16]int{}, pressed: map[uint16]bool{}}
	for i := range m.values {
		m.values[i] = binary.LittleEndian.Uint32(b[i*4 : i*4+4])
	}
	return m
}

func eviocgabs(code uint16) uintptr { return uintptr(0x80184540 + uint32(code)) }
func (m *misterControllerMap) absInfoFor(f *os.File, code uint16) inputAbsInfo {
	if info, ok := m.axisInfo[code]; ok {
		return info
	}
	var info inputAbsInfo
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), eviocgabs(code), uintptr(unsafe.Pointer(&info)))
	if errno != 0 || info.Maximum <= info.Minimum {
		info.Minimum = -1
		info.Maximum = 1
	}
	m.axisInfo[code] = info
	return info
}
func axisDigitalState(value int32, info inputAbsInfo) int {
	center := info.Minimum + (info.Maximum-info.Minimum)/2
	r := info.Maximum - info.Minimum
	if r <= 2 {
		if value < center {
			return -1
		}
		if value > center {
			return 1
		}
		return 0
	}
	threshold := r / 4
	if value <= center-threshold {
		return -1
	}
	if value >= center+threshold {
		return 1
	}
	return 0
}
func (m *misterControllerMap) actionForCode(code uint16) (action, bool) {
	match := func(slot int) bool { v := uint16(m.values[slot] & 0xffff); return v != 0 && v == code }
	switch {
	case match(misterBtnRight):
		return actRight, true
	case match(misterBtnLeft):
		return actLeft, true
	case match(misterBtnDown):
		return actDown, true
	case match(misterBtnUp):
		return actUp, true
	case match(misterBtnA):
		if swapABInput.Load() {
			return actBack, true
		}
		return actConfirm, true
	case match(misterBtnB):
		if swapABInput.Load() {
			return actConfirm, true
		}
		return actBack, true
	case match(misterBtnX):
		if swapXYInput.Load() {
			return actY, true
		}
		return actX, true
	case match(misterBtnY):
		if swapXYInput.Load() {
			return actX, true
		}
		return actY, true
	case match(misterBtnSelect):
		return actSettings, true
	}
	return actNone, false
}
func (m *misterControllerMap) process(f *os.File, ev inputEvent) (action, bool, bool) {
	if ev.Type == evKey {
		if ev.Code < 256 {
			return actNone, false, false
		}
		down := ev.Value != 0
		wasDown := m.pressed[ev.Code]
		m.pressed[ev.Code] = down
		if !down || wasDown {
			return actNone, true, false
		}
		a, ok := m.actionForCode(ev.Code)
		face := ok && (a == actConfirm || a == actBack || a == actX || a == actY)
		return a, true, face
	}
	if ev.Type == evAbs {
		info := m.absInfoFor(f, ev.Code)
		state := axisDigitalState(ev.Value, info)
		old := m.axisState[ev.Code]
		if state == old {
			return actNone, true, false
		}
		m.axisState[ev.Code] = state
		if state == 0 {
			return actNone, true, false
		}
		code := uint16(misterKeyEmu + int(ev.Code)*2)
		if state > 0 {
			code++
		}
		a, _ := m.actionForCode(code)
		return a, true, false
	}
	return actNone, false, false
}
