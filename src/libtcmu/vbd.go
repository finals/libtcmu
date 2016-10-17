package tcmu

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jochenvg/go-udev"
)

const (
	DEVPATH = "/dev/tcomet"

	READY   = 0
	SUCCESS = 1
	ERROR   = 2
	TIMEOUT = 3
)

type VBD struct {
	devPath string // eg. /dev/comet/vdev1
	sysPath string // eg. /dev/sdb
	vbd     *VirBlkDev
}

func NewVBD() *VBD {
	return &VBD{}
}

func (vbd *VBD) Create(name string, size int64, sectorSize int64, rw ReadWriteAt) error {
	handler := &ScsiHandler{
		HBA:        42,
		LUN:        0,
		VolumeName: name,
		WWN:        GenerateTestWWN(name),
		DataSizes:  DataSizes{size, sectorSize},
		Handler:    ReadWriteAtCmdHandler{RW: rw},
	}

	wait := make(chan int)
	go vbd.completion(wait)
	result := <-wait
	if result != READY {
		return fmt.Errorf("start udev monitor error")
	}

	var err error
	if IsDirExists(DEVPATH) == false {
		err = os.Mkdir(DEVPATH, os.ModeDir)
		if err != nil {
			return err
		}
	}
	vbd.devPath = filepath.Join(DEVPATH, name)
	vbd.vbd, err = newVirtBlockDevice(vbd.devPath, handler)
	if err != nil {
		log.Errorf("[Create] devPath:%s error:%s", DEVPATH, err.Error())
		return fmt.Errorf(err.Error())
	}

	result = <-wait
	if result != SUCCESS {
		vbd.Delete()
		log.Errorf("[Create] devPath:%s, wait to generate device error:%d", result)
		return fmt.Errorf("wait generate device error/timeout")
	}

	return nil
}

/*
func (vbd *VBD) Create1(name string, size int64, sectorSize int64, rw virtual.ReaderWriterAt) (virtual.Virtual, error) {
	handler := &ScsiHandler{
		HBA:        42,
		LUN:        0,
		VolumeName: name,
		WWN:        GenerateTestWWN(name),
		DataSizes:  DataSizes{size, sectorSize},
		Handler:    ReadWriteAtCmdHandler{RW: rw},
	}

	wait := make(chan int)
	go vbd.completion(wait)
	result := <-wait
	if result != READY {
		return nil, errno.NewError(EcodeTcmuCreateDeviceError, "start udev monitor error")
	}

	var err error
	if IsDirExists(DEVPATH) == false {
		err = os.Mkdir(DEVPATH, os.ModeDir)
		if err != nil {
			return nil, err
		}
	}
	vbd.devPath = filepath.Join(DEVPATH, name)
	vbd.vbd, err = newVirtBlockDevice(vbd.devPath, handler)
	if err != nil {
		log.Errorf("[CreateDevice] devPath:%s error:%s", DEVPATH, err.Error())
		return nil, errno.NewError(EcodeTcmuCreateDeviceError, err.Error())
	}

	result = <-wait
	if result != SUCCESS {
		vbd.Delete()
		log.Errorf("[CreateDevice] devPath:%s, wait to generate device error:%d", result)
		return nil, errno.NewError(EcodeTcmuCreateDeviceError, "wait generate device error/timeout")
	}

	return vbd, nil
}
*/

func (vbd *VBD) completion(wait chan int) {
	u := udev.Udev{}
	m := u.NewMonitorFromNetlink("udev")

	// Add filters to monitor
	m.FilterAddMatchSubsystemDevtype("block", "disk")
	//m.FilterAddMatchTag("systemd")

	// Create a done signal channel, Start monitor goroutine and get receive channel
	done := make(chan struct{})

	ch, _ := m.DeviceChan(done)
	wait <- READY
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
				log.Errorf("[completion] udev report not tcmu device, wait for:%s", dev.Devnode())
				continue
			}

			vbd.sysPath = dev.Devnode()
			dnum := dev.Devnum()
			vbd.vbd.major = dnum.Major()
			vbd.vbd.minor = dnum.Minor()
			err = vbd.vbd.GenerateDevice()
			if err != nil {
				log.Errorf("[completion] Generate virtdisk device error:%s", err.Error())
				wait <- ERROR
			} else {
				wait <- SUCCESS
			}
			return
		case <-time.After(30 * time.Second):
			log.Infof("[completion] Stop Monitor Device Event")
			wait <- TIMEOUT
			return
		}
	}
}

func (vbd *VBD) Delete() error {
	if vbd.vbd.IsBusy() {
		log.Infof("[Delete] vbd busy path:%s", vbd.devPath)
		return fmt.Errorf("get mount info error")
	}

	vbd.vbd.Close()
	return nil
}

func (vbd *VBD) Path() (string, string) {
	return vbd.devPath, vbd.sysPath
}

