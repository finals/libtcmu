package libtcmu

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jochenvg/go-udev"
	//"util/fs"
	"path/filepath"
)

const (
	CREATE_TIMEOUT = 10 * time.Second

	SUCCESS = 0
	ERROR = 1
	TIMEOUT = 2
)

var (
	hba *HBA = nil
)

type HBA struct {
	sync.Mutex
	id              int
	devPath         string
	lunid           int
	module          string
	vbdInitializing *VirBlkDev
	devEvent        chan *udev.Device
	vbds            map[string]*VirBlkDev
	stopC           chan struct{}
}

func NewHBA(module string) (*HBA, error) {
	if hba != nil && module == hba.module {
		return hba, nil
	}

	devPath := fmt.Sprintf("/dev/%s", module)
	if IsDirExists(devPath) == false {
		err := os.Mkdir(devPath, os.ModeDir)
		if err != nil {
			return nil, err
		}
	}

	hba = &HBA{
		id:      42,
		devPath: devPath,
		lunid:   0,
		module:  module,
	}
	hba.stopC = make(chan struct{})
	hba.devEvent = make(chan *udev.Device, 32)
	hba.vbds = make(map[string]*VirBlkDev)
	hba.vbdInitializing = nil
	return hba, nil
}

func (h *HBA) Start() error {
	go h.monitorDeviceEvent()
	return nil
}

func (h *HBA) Stop() error {
	close(h.stopC)
	return nil
}

func (h *HBA) CreateDevice(name string, size int64, sectorSize int64, rw ReadWriteAt, threads int) (*VirBlkDev, error) {
	h.Lock()
	//defer h.Unlock()

	if h.vbdInitializing != nil {
		return nil, fmt.Errorf("Error: other vbd initializing, try again")
	}

	handler := &ScsiHandler{
		HBA:        h.id,
		LUN:        h.lunid,
		VolumeName: name,
		WWN:        GenerateTestWWN(name),
		DataSizes:  DataSizes{size, sectorSize},
		DevReady: MultiThreadedDevReady(
			ReadWriteAtCmdHandler{
				RW: rw,
			},
			threads,
		),
	}
	h.lunid++

	completion := make(chan int)
	go h.CreateDeviceComplete(completion)
	vbd, err := newVirtBlockDevice(h.devPath, handler)
	if err != nil {
		log.Errorf("[CreateDevice] devPath:%s error:%s", h.devPath, err.Error())
		return nil, err
	}
	h.vbdInitializing = vbd
	result := <-completion
	if result != SUCCESS {
		vbd.Close()
		log.Errorf("[CreateDevice] devPath:%s, wait to generate device error:%d", result)
		return nil, fmt.Errorf("wait to generate device error:%d", result)
	}
	return vbd, nil
}

func (h *HBA) CreateDeviceComplete(completion chan int) {
	//log.Infof("[CreateDeviceComplete] start")
	timeout := time.After(CREATE_TIMEOUT)
	for {
		select {
		case dev := <-h.devEvent:
		//log.Infof("[CreateDeviceComplete] receive event")
			if "add" != dev.Action() {
				continue
			}
		//log.Infof("[CreateDeviceComplete] ID_MODEL: %s", dev.PropertyValue("ID_MODEL"))
			if !strings.Contains(dev.PropertyValue("ID_MODEL"), "TCMU_Device") {
				log.Errorf("[CreateDeviceComplete] udev report not tcmu device, wait for:%s", dev.Devnode())
				continue
			}

			if h.vbdInitializing != nil {
				dnum := dev.Devnum()
				//log.Infof("[CreateDeviceComplete] major:%d, minor:%d", dnum.Major(), dnum.Minor())
				h.vbdInitializing.SetDeviceNumber(dnum.Major(), dnum.Minor())
				err := h.vbdInitializing.GenerateDevEntry()
				h.Unlock()
				if err != nil {
					log.Errorf("[CreateDeviceComplete] Generate virtdisk device error:%s", err.Error())
					completion <- ERROR
				} else {
					h.vbds[h.vbdInitializing.deviceName] = h.vbdInitializing
					completion <- SUCCESS
				}
				h.vbdInitializing = nil
				return
			}
		case <-timeout:
			log.Errorf("[CreateDeviceComplete] Generate virtdisk device error:Timeout")
			h.Unlock()
			completion <- TIMEOUT
			return
		}
		break
	}
}

func (h *HBA) RemoveDevice(name string) {
	vbd, exist := h.vbds[name]
	if !exist {
		return
	}
	remove(filepath.Join(vbd.devPath, vbd.scsi.VolumeName))
	vbd.Close()
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
		//log.Infof("[monitorDeviceEvent] ID_MODEL: %s", dev.PropertyValue("ID_MODEL"))
			if !strings.Contains(dev.PropertyValue("ID_MODEL"), "TCMU_Device") {
				log.Errorf("[monitorDeviceEvent] udev report not tcmu device, wait for:%s", dev.Devnode())
				continue
			}
		//log.Infof("[monitorDeviceEvent] dev: %s", dev.Devnode())
			h.devEvent <- dev
		//log.Debugf("[monitorDeviceEvent] receive event:%s", dev.Action())
		case <-h.stopC:
			log.Infof("[monitorDeviceEvent] Stop Monitor Device Event")
			return
		}

	}
}

func IsDirExists(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return os.IsExist(err)
	} else {
		return fi.IsDir()
	}
}
