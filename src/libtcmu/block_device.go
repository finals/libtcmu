package tcmu

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"github.com/docker/docker/pkg/mount"
	"sync"
)

const (
	CONFIG_DIR_FORMAT = "/sys/kernel/config/target/core/user_%d"
	SCSI_DIR = "/sys/kernel/config/target/loopback"

	CMD_RING_SIZE = 128
)

type VirBlkDev struct {
	sync.Mutex

	scsi       *ScsiHandler
	devPath    string
	hbaDir     string
	deviceName string
	major      int
	minor      int

	uioFd      int
	mapsize    uint64
	mmap       []byte
	//cmdChan    chan *ScsiCmd
	//respChan   chan ScsiResponse
	cmdTail    uint32
	pipeFds    []int
	initialize bool
	shut       chan struct{}
	wait       chan struct{}

	cmdRing    *ScsiResponseRing
	cmdDone    chan int
}

// WWN provides two WWNs, one for the device itself and one for the loopback device created by the kernel.
type WWN interface {
	DeviceID() string
	NexusID() string
}

func (vbd *VirBlkDev) GetDevConfig() string {
	return fmt.Sprintf("libtcmu//%s", vbd.scsi.VolumeName)
}

func (vbd *VirBlkDev) Sizes() DataSizes {
	return vbd.scsi.DataSizes
}

func (vbd *VirBlkDev) Capacity() int64 {
	return vbd.scsi.DataSizes.VolumeSize
}

func (vbd *VirBlkDev) GetDevice() string {
	return vbd.devPath
}

func (vbd *VirBlkDev) Name() string {
	return vbd.scsi.VolumeName
}

func (vbd *VirBlkDev) IsBusy() bool {
	minfo, err := mount.GetMounts()
	if err != nil {
		return true
	}
	for _, info := range minfo {
		if info.Major == vbd.major && info.Minor == vbd.minor {
			return true
		}
	}

	return false
}

// newVirtBlockDevice creates the virtual device based on the details in the ScsiHandler, eventually creating
// a device under devPath (eg, "/dev") with the file name scsi.VolumeName;
// The returned vbd represents the open device connection to the kernel, and must be closed.
func newVirtBlockDevice(devPath string, scsi *ScsiHandler) (*VirBlkDev, error) {
	vbd := &VirBlkDev{
		scsi:       scsi,
		devPath:    filepath.Join(devPath, scsi.VolumeName),
		uioFd:      -1,
		hbaDir:     fmt.Sprintf(CONFIG_DIR_FORMAT, scsi.HBA),
		initialize: false,
		shut:       make(chan struct{}),
		wait:       make(chan struct{}),
		cmdRing:    &ScsiResponseRing{
			capacity: CMD_RING_SIZE,
			head:     0,
			tail:     0,
			data:     make([]*ScsiResponse, CMD_RING_SIZE),
		},
		cmdDone:    make(chan int, CMD_RING_SIZE),
	}
	err := vbd.Close()
	if err != nil {
		return nil, err
	}

	vbd.pipeFds = make([]int, 2)
	if err := unix.Pipe(vbd.pipeFds); err != nil {
		log.Errorf("[newVirtBlockDevice] vbd:%s create pipe error:%s", vbd.devPath, err)
		return nil, err
	}

	vbd.initialize = true
	if err := vbd.preEnableTcmu(); err != nil {
		log.Errorf("[newVirtBlockDevice] vbd:%s preEnableTcmu error:%s", vbd.devPath, err.Error())
		return nil, err
	}

	if err := vbd.start(); err != nil {
		return nil, err
	}

	return vbd, vbd.postEnableTcmu()
}

func (vbd *VirBlkDev) Close() error {
	err := vbd.teardown()
	if err != nil {
		return err
	}

	if vbd.initialize {
		vbd.stopPoll()
		vbd.closeDevice()

		select {
		case <-vbd.wait:
			break
		case <-time.After(30 * time.Second):
		}

		if err := unix.Close(vbd.pipeFds[0]); err != nil {
			log.Errorf("[Close] vbd:%s Fail to close pipeFds[0]: %s", vbd.devPath, err)
		}
		if err := unix.Close(vbd.pipeFds[1]); err != nil {
			log.Errorf("[Close] vbd:%s Fail to close pipeFds[1]: %s", vbd.devPath, err)
		}
	}

	return nil
}

func (vbd *VirBlkDev) preEnableTcmu() error {
	err := writeLines(path.Join(vbd.hbaDir, vbd.scsi.VolumeName, "control"), []string{
		fmt.Sprintf("dev_size=%d", vbd.scsi.DataSizes.VolumeSize),
		fmt.Sprintf("dev_config=%s", vbd.GetDevConfig()),
		fmt.Sprintf("hw_block_size=%d", vbd.scsi.DataSizes.SectorSize),
		"async=1",
	})
	if err != nil {
		return err
	}

	return writeLines(path.Join(vbd.hbaDir, vbd.scsi.VolumeName, "enable"), []string{
		"1",
	})
}

func (vbd *VirBlkDev) getSCSIPrefixAndWnn() (string, string) {
	return path.Join(SCSI_DIR, vbd.scsi.WWN.DeviceID(), "tpgt_1"), vbd.scsi.WWN.NexusID()
}

func (vbd *VirBlkDev) getLunPath(prefix string) string {
	return path.Join(prefix, "lun", fmt.Sprintf("lun_%d", vbd.scsi.LUN))
}

func (vbd *VirBlkDev) postEnableTcmu() error {
	prefix, nexusWnn := vbd.getSCSIPrefixAndWnn()

	err := writeLines(path.Join(prefix, "nexus"), []string{
		nexusWnn,
	})
	if err != nil {
		return err
	}

	lunPath := vbd.getLunPath(prefix)
	if err := os.MkdirAll(lunPath, 0755); err != nil && !os.IsExist(err) {
		return err
	}

	if err := os.Symlink(path.Join(vbd.hbaDir, vbd.scsi.VolumeName), path.Join(lunPath, vbd.scsi.VolumeName)); err != nil {
		return err
	}

	return nil
}

func (vbd *VirBlkDev) SetDeviceNumber(major, minor int) {
	vbd.major = major
	vbd.minor = minor
}

func (vbd *VirBlkDev) GenerateDevice() error {
	//dev := filepath.Join(vbd.devPath, vbd.scsi.VolumeName)
	//log.Infof("[GenerateDevEntry] dev:%s  major:%d, minor:%d", vbd.devPath, vbd.major, vbd.minor)
	err := mknod(vbd.devPath, vbd.major, vbd.minor)
	if err != nil {
		log.Infof("[GenerateDevEntry] vbd:%s error:%s", vbd.devPath, err.Error())
		return err
	}
	return nil
}

func (vbd *VirBlkDev) GetDeviceAttr(attr string) (int, error) {
	att, err := ioutil.ReadFile(fmt.Sprintf("/sys/kernel/config/target/core/user_%d/%s/attrib/%s", vbd.scsi.HBA, vbd.scsi.VolumeName, attr))
	if err != nil {
		return 0, err
	}

	i, err := strconv.Atoi(string(att))
	if err != nil {
		return 0, err
	}

	return i, nil
}

func mknod(device string, major, minor int) error {
	var fileMode os.FileMode = 0600
	fileMode |= syscall.S_IFBLK
	dev := int((major << 8) | (minor & 0xff) | ((minor & 0xfff00) << 12))

	return syscall.Mknod(device, uint32(fileMode), dev)
}

func writeLines(target string, lines []string) error {
	dir := path.Dir(target)
	if stat, err := os.Stat(dir); os.IsNotExist(err) {
		//log.Debugf("Creating directory: %s", dir)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	} else if !stat.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}

	for _, line := range lines {
		content := []byte(line + "\n")
		//log.Debugf("Setting %s: %s", target, line)
		if err := ioutil.WriteFile(target, content, 0755); err != nil {
			//log.Debugf("Failed to write %s to %s: %v", line, target, err)
			return err
		}
	}

	return nil
}

func (vbd *VirBlkDev) start() (err error) {
	err = vbd.findDevice()
	if err != nil {
		return
	}

	//vbd.cmdChan = make(chan *ScsiCmd, 128)
	//vbd.respChan = make(chan ScsiResponse, 128)
	//go vbd.startPollx()
	go vbd.startPoll()
	//vbd.scsi.DevReady(vbd.cmdChan, vbd.respChan)
	return
}

func (vbd *VirBlkDev) findDevice() error {
	err := filepath.Walk("/dev", func(path string, i os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if i.IsDir() && path != "/dev" {
			return filepath.SkipDir
		}

		if !strings.HasPrefix(i.Name(), "uio") {
			return nil
		}
		sysfile := fmt.Sprintf("/sys/class/uio/%s/name", i.Name())
		content, err := ioutil.ReadFile(sysfile)
		if err != nil {
			return err
		}
		split := strings.SplitN(strings.TrimRight(string(content), "\n"), "/", 4)
		if split[0] != "tcm-user" {
			// Not a TCM device
			return nil
		}
		if split[3] != vbd.GetDevConfig() {
			// Not a TCM device
			return nil
		}
		err = vbd.openDevice(split[1], split[2], i.Name())
		if err != nil {
			return err
		}
		return filepath.SkipDir
	})
	if err == filepath.SkipDir {
		return nil
	}
	return err
}

func (vbd *VirBlkDev) openDevice(user string, vol string, uio string) error {
	var err error
	vbd.deviceName = vol

	vbd.uioFd, err = syscall.Open(fmt.Sprintf("/dev/%s", uio), syscall.O_RDWR | syscall.O_NONBLOCK | syscall.O_CLOEXEC, 0600)
	if err != nil {
		return err
	}
	size, err := ioutil.ReadFile(fmt.Sprintf("/sys/class/uio/%s/maps/map0/size", uio))
	if err != nil {
		return err
	}

	vbd.mapsize, err = strconv.ParseUint(strings.TrimRight(string(size), "\n"), 0, 64)
	if err != nil {
		return err
	}

	vbd.mmap, err = syscall.Mmap(vbd.uioFd, 0, int(vbd.mapsize), syscall.PROT_READ | syscall.PROT_WRITE, syscall.MAP_SHARED)
	vbd.cmdTail = vbd.mbCmdTail()
	//vbd.debugPrintMb()

	return err
}

func (vbd *VirBlkDev) closeDevice() {
	syscall.Munmap(vbd.mmap)

	if vbd.uioFd != -1 {
		unix.Close(vbd.uioFd)
	}

	//if vbd.cmdChan != nil {
	//	close(vbd.cmdChan)
	//}

	//if _, isClose := <-vbd.respChan; !isClose {
	//	close(vbd.respChan)
	//}
}

func (vbd *VirBlkDev) debugPrintMb() {
	fmt.Printf("Got a TCMU mailbox, version: %d\n", vbd.mbVersion())
	fmt.Printf("mapsize: %d\n", vbd.mapsize)
	fmt.Printf("mbFlags: %d\n", vbd.mbFlags())
	fmt.Printf("mbCmdrOffset: %d\n", vbd.mbCmdrOffset())
	fmt.Printf("mbCmdrSize: %d\n", vbd.mbCmdrSize())
	fmt.Printf("mbCmdHead: %d\n", vbd.mbCmdHead())
	fmt.Printf("mbCmdTail: %d\n", vbd.mbCmdTail())
}

func (vbd *VirBlkDev) teardown() error {
	//dev := filepath.Join(vbd.devPath, vbd.scsi.VolumeName)
	tpgtPath, _ := vbd.getSCSIPrefixAndWnn()
	lunPath := vbd.getLunPath(tpgtPath)

	/*
		We're removing:
		/sys/kernel/config/target/loopback/naa.<id>/tpgt_1/lun/lun_0/<volume name>
		/sys/kernel/config/target/loopback/naa.<id>/tpgt_1/lun/lun_0
		/sys/kernel/config/target/loopback/naa.<id>/tpgt_1
		/sys/kernel/config/target/loopback/naa.<id>
		/sys/kernel/config/target/core/user_42/<volume name>
	*/
	pathsToRemove := []string{
		path.Join(lunPath, vbd.scsi.VolumeName),
		lunPath,
		tpgtPath,
		path.Dir(tpgtPath),
		path.Join(vbd.hbaDir, vbd.scsi.VolumeName),
	}

	for _, p := range pathsToRemove {
		err := remove(p)
		if err != nil {
			return err
		}
	}

	// Should be cleaned up automatically, but if it isn't remove it
	if _, err := os.Stat(vbd.devPath); err == nil {
		err := remove(vbd.devPath)
		if err != nil {
			return err
		}
	}

	return nil
}

func removeAsync(path string, done chan <- error) {
	//log.Debugf("Removing: %s", path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Debugf("Unable to remove: %v", path)
		done <- err
	}
	//log.Debugf("Removed: %s", path)
	done <- nil
}

func remove(path string) error {
	done := make(chan error)
	go removeAsync(path, done)
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		return fmt.Errorf("Timeout trying to delete %s.", path)
	}
}

func remove1(path string) error {
	//fmt.Printf("Removing: %s\n", path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Warnf("[remove] Unable to remove: %s", path)
	}
	//fmt.Printf("Removed: %s\n", path)
	return nil
}
