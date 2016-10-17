package tcmu

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/jochenvg/go-udev"

)

const (
	CREATE_TIMEOUT = 15 * time.Second
/*
	SUCCESS = 0
	ERROR   = 1
	TIMEOUT = 2
*/
	DEV_DIR_NAME = "comet"
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
	for name, _ := range h.vbds {
		h.RemoveDevice(name)
	}

	close(h.stopC)
	return nil
}

func (h *HBA) CreateDevice(name string, size int64, sectorSize int64, rw ReadWriteAt) (*VirBlkDev, error) {
	h.Lock()
	//defer h.Unlock()

	if h.vbdInitializing != nil {
		return nil, fmt.Errorf("other vbd initializing, try again")
	}

	handler := &ScsiHandler{
		HBA:        h.id,
		LUN:        h.lunid,
		VolumeName: name,
		WWN:        GenerateTestWWN(name),
		DataSizes:  DataSizes{size, sectorSize},
		Handler:    ReadWriteAtCmdHandler{RW: rw},
		/*
			DevReady: MultiThreadedDevReady(
				ReadWriteAtCmdHandler{
					RW: rw,
				},
				threads,
			),
		*/
	}
	//h.lunid++

	completion := make(chan int)
	go h.CreateDeviceComplete(completion)
	vbd, err := newVirtBlockDevice(h.devPath, handler)
	if err != nil {
		log.Errorf("[CreateDevice] devPath:%s error:%s", h.devPath, err.Error())
		return nil, fmt.Errorf(err.Error())
	}
	h.vbdInitializing = vbd
	result := <-completion
	if result != SUCCESS {
		vbd.Close()
		h.vbdInitializing = nil
		log.Errorf("[CreateDevice] devPath:%s, wait to generate device error:%d", result)
		return nil, fmt.Errorf("wait generate device error/timeout")
	}
	h.vbdInitializing = nil
	return vbd, nil
}

func (h *HBA) CreateDeviceComplete(completion chan int) {
	timeout := time.After(CREATE_TIMEOUT)
	for {
		select {
		case dev := <-h.devEvent:
			//log.Infof("[CreateDeviceComplete] receive event")
			if "add" != dev.Action() {
				continue
			}

			res, err := IsTcmuDevice(dev.Devnode())
			if res == false || err != nil {
				log.Errorf("[CreateDeviceComplete] udev report not tcmu device:%s, waiting", dev.Devnode())
				continue
			}

		    retry := 300
		    for i := 0; i < retry; i++ {
			    if h.vbdInitializing != nil {
				    dnum := dev.Devnum()

				    h.vbdInitializing.SetDeviceNumber(dnum.Major(), dnum.Minor())
				    err := h.vbdInitializing.GenerateDevice()
				    h.Unlock()
				    if err != nil {
					    log.Errorf("[CreateDeviceComplete] Generate virtdisk device error:%s", err.Error())
					    completion <- ERROR
				    } else {
					    h.vbds[h.vbdInitializing.deviceName] = h.vbdInitializing
					    completion <- SUCCESS
				    }
				    return
			    }
			    time.Sleep(50 * time.Millisecond)
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

func (h *HBA) RemoveDevice(name string) error {
	vbd, exist := h.vbds[name]
	if !exist {
		return nil
	}

	if vbd.IsBusy() {
		log.Infof("[RemoveDevice] vbd busy name:%s", name)
		return fmt.Errorf("get mount info error")
	}

	remove(vbd.devPath)
	vbd.Close()
	delete(h.vbds, name)
	return nil
}

func (h *HBA) monitorDeviceEvent() {
	defer func() {
		if err := recover(); err != nil {

		}
	}()

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
			// avoid strace process cause udev panic
			if dev == nil {
				ch, _ = m.DeviceChan(done)
				continue
			}

			if "add" != dev.Action() {
				continue
			}

			res, err := IsTcmuDevice(dev.Devnode())
			if res == false || err != nil {
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
