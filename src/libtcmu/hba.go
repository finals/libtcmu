package libtcmu

import (
	"sync"
	"fmt"

	"github.com/jochenvg/go-udev"
	"strings"
)

var (
	hba *HBA = nil
)

type HBA struct {
	sync.Mutex
	vbdInitializing *VirtBlockDevice
	stopC           chan struct{}
}

func NewHBA() *HBA {
	if hba != nil {
		return hba
	}

	hba = &HBA{}
	hba.stopC = make(chan struct{})
	hba.vbdInitializing = nil
	return hba
}

func (h *HBA) Start() error {
	go h.monitorDeviceEvent()
	return nil
}

func (h *HBA) Stop() error {
	close(h.stopC)
	return nil
}

func (h *HBA) CreateDevice(devPath string, scsi *ScsiHandler) (*VirtBlockDevice, error) {
	h.Lock()
	defer h.Unlock()

	if h.vbdInitializing != nil {
		return nil, fmt.Errorf("Error: other vbd initializing, try again")
	}

	vbd, err := newVirtBlockDevice(devPath, scsi)
	if err != nil {
		log.Errorf("[CreateDevice] devPath:%s error:%s", devPath, err.Error())
		return nil, err
	}
	h.vbdInitializing = vbd
	return vbd, nil
}

func (h *HBA) monitorDeviceEvent() {
	log.Infof("[monitorDeviceEvent] Start Monitor Device Event")
	u := udev.Udev{}
	m := u.NewMonitorFromNetlink("udev")

	// Add filters to monitor
	m.FilterAddMatchSubsystemDevtype("block", "disk")
	//m.FilterAddMatchTag("systemd")

	// Create a done signal channel

	// Start monitor goroutine and get receive channel
	done := make(chan struct{})
	ch, _ := m.DeviceChan(done)
	for {
		select {
		case dev := <-ch:
			if "add" != dev.Action() {
				continue
			}
			log.Infof("[monitorDeviceEvent] ID_MODEL: %s", dev.PropertyValue("ID_MODEL"))
		//if "TCMU_device" != dev.PropertyValue("ID_MODEL") {
			if !strings.Contains(dev.PropertyValue("ID_MODEL"), "TCMU_Device") {
				log.Errorf("[monitorDeviceEvent] udev report not tcmu device, wait for:%s", dev.Devnode())
				continue
			}

			h.Lock()
		//log.Infof("[monitorDeviceEvent] VBD Initializing: %+v", h.vbdInitializing)
			if h.vbdInitializing != nil {
				dnum := dev.Devnum()
				log.Infof("[monitorDeviceEvent] major:%d, minor:%d", dnum.Major(), dnum.Minor())
				h.vbdInitializing.SetDeviceNumber(dnum.Major(), dnum.Minor())
				h.vbdInitializing = nil
			}
			h.Unlock()
		//log.Debugf("[monitorDeviceEvent] receive event:%s", dev.Action())
		case <-h.stopC:
			log.Infof("[monitorDeviceEvent] Stop Monitor Device Event")
			return
		}

	}
}
