package libtcmu

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
)

const (
	CONFIG_DIR_FORMAT = "/sys/kernel/config/target/core/user_%d"
	SCSI_DIR = "/sys/kernel/config/target/loopback"
)

type VirtBlockDevice struct {
	scsi       *ScsiHandler
	devPath    string
	hbaDir     string
	deviceName string

	uioFd      int
	mapsize    uint64
	mmap       []byte
	cmdChan    chan *ScsiCmd
	respChan   chan ScsiResponse
	cmdTail    uint32
}

// WWN provides two WWNs, one for the device itself and one for the loopback device created by the kernel.
type WWN interface {
	DeviceID() string
	NexusID() string
}

func (vbd *VirtBlockDevice) GetDevConfig() string {
	return fmt.Sprintf("libtcmu//%s", vbd.scsi.VolumeName)
}

func (vbd *VirtBlockDevice) Sizes() DataSizes {
	return vbd.scsi.DataSizes
}

// NewVirtBlockDevice creates the virtual device based on the details in the ScsiHandler, eventually creating
// a device under devPath (eg, "/dev") with the file name scsi.VolumeName;
// The returned vbd represents the open device connection to the kernel, and must be closed.
func NewVirtBlockDevice(devPath string, scsi *ScsiHandler) (*VirtBlockDevice, error) {
	vbd := &VirtBlockDevice{
		scsi: scsi,
		devPath: devPath,
		uioFd: -1,
		hbaDir: fmt.Sprintf(CONFIG_DIR_FORMAT, scsi.HBA),
	}
	err := vbd.Close()
	if err != nil {
		return nil, err
	}

	if err := vbd.preEnableTcmu(); err != nil {
		return nil, err
	}
	if err := vbd.boot(); err != nil {
		return nil, err
	}

	return vbd, vbd.postEnableTcmu()
}

func (vbd *VirtBlockDevice) Close() error {
	err := vbd.teardown()
	if err != nil {
		return err
	}

	if vbd.uioFd != -1 {
		unix.Close(vbd.uioFd)
	}
	return nil
}

func (vbd *VirtBlockDevice) preEnableTcmu() error {
	err := writeLines(path.Join(vbd.hbaDir, vbd.scsi.VolumeName, "control"), []string{
		fmt.Sprintf("dev_size=%d", vbd.scsi.DataSizes.VolumeSize),
		fmt.Sprintf("dev_config=%s", vbd.GetDevConfig()),
		fmt.Sprintf("hw_block_size=%d", vbd.scsi.DataSizes.BlockSize),
		"async=1",
	})
	if err != nil {
		return err
	}

	return writeLines(path.Join(vbd.hbaDir, vbd.scsi.VolumeName, "enable"), []string{
		"1",
	})
}

func (vbd *VirtBlockDevice) getSCSIPrefixAndWnn() (string, string) {
	return path.Join(SCSI_DIR, vbd.scsi.WWN.DeviceID(), "tpgt_1"), vbd.scsi.WWN.NexusID()
}

func (vbd *VirtBlockDevice) getLunPath(prefix string) string {
	return path.Join(prefix, "lun", fmt.Sprintf("lun_%d", vbd.scsi.LUN))
}

func (vbd *VirtBlockDevice) postEnableTcmu() error {
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

	return vbd.createDevEntry()
}

func (vbd *VirtBlockDevice) createDevEntry() error {
	os.MkdirAll(vbd.devPath, 0755)

	dev := filepath.Join(vbd.devPath, vbd.scsi.VolumeName)

	if _, err := os.Stat(dev); err == nil {
		return fmt.Errorf("Device %s already exists, can not create", dev)
	}

	tgt, _ := vbd.getSCSIPrefixAndWnn()

	address, err := ioutil.ReadFile(path.Join(tgt, "address"))
	if err != nil {
		return err
	}

	found := false
	matches := []string{}
	path := fmt.Sprintf("/sys/bus/scsi/devices/%s*/block/*/dev", strings.TrimSpace(string(address)))
	for i := 0; i < 30; i++ {
		var err error
		matches, err = filepath.Glob(path)
		if len(matches) > 0 && err == nil {
			found = true
			break
		}

		fmt.Printf("Waiting for %s", path)
		time.Sleep(1 * time.Second)
	}

	if !found {
		return fmt.Errorf("Failed to find %s", path)
	}

	if len(matches) == 0 {
		return fmt.Errorf("Failed to find %s", path)
	}

	if len(matches) > 1 {
		return fmt.Errorf("Too many matches for %s, found %d", path, len(matches))
	}

	majorMinor, err := ioutil.ReadFile(matches[0])
	if err != nil {
		return err
	}

	parts := strings.Split(strings.TrimSpace(string(majorMinor)), ":")
	if len(parts) != 2 {
		return fmt.Errorf("Invalid major:minor string %s", string(majorMinor))
	}

	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return err
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return err
	}

	fmt.Printf("Creating device %s %d:%d", dev, major, minor)
	return mknod(dev, major, minor)
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
		fmt.Printf("Creating directory: %s", dir)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	} else if !stat.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}

	for _, line := range lines {
		content := []byte(line + "\n")
		fmt.Printf("Setting %s: %s", target, line)
		if err := ioutil.WriteFile(target, content, 0755); err != nil {
			fmt.Printf("Failed to write %s to %s: %v", line, target, err)
			return err
		}
	}

	return nil
}

func (vbd *VirtBlockDevice) boot() (err error) {
	err = vbd.findDevice()
	if err != nil {
		return
	}

	vbd.cmdChan = make(chan *ScsiCmd, 5)
	vbd.respChan = make(chan ScsiResponse, 5)
	go vbd.beginPoll()
	vbd.scsi.DevReady(vbd.cmdChan, vbd.respChan)
	return
}

func (vbd *VirtBlockDevice) findDevice() error {
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

func (vbd *VirtBlockDevice) openDevice(user string, vol string, uio string) error {
	var err error
	vbd.deviceName = vol

	vbd.uioFd, err = syscall.Open(fmt.Sprintf("/dev/%s", uio), syscall.O_RDWR | syscall.O_CLOEXEC, 0600)
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
	vbd.debugPrintMb()

	return err
}

func (vbd *VirtBlockDevice) debugPrintMb() {
	fmt.Printf("Got a TCMU mailbox, version: %d\n", vbd.mbVersion())
	fmt.Printf("mapsize: %d\n", vbd.mapsize)
	fmt.Printf("mbFlags: %d\n", vbd.mbFlags())
	fmt.Printf("mbCmdrOffset: %d\n", vbd.mbCmdrOffset())
	fmt.Printf("mbCmdrSize: %d\n", vbd.mbCmdrSize())
	fmt.Printf("mbCmdHead: %d\n", vbd.mbCmdHead())
	fmt.Printf("mbCmdTail: %d\n", vbd.mbCmdTail())
}

func (vbd *VirtBlockDevice) teardown() error {
	dev := filepath.Join(vbd.devPath, vbd.scsi.VolumeName)
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
	if _, err := os.Stat(dev); err == nil {
		err := remove(dev)
		if err != nil {
			return err
		}
	}

	return nil
}

func removeAsync(path string, done chan <- error) {
	fmt.Printf("Removing: %s", path)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Errorf("Unable to remove: %v", path)
		done <- err
	}
	fmt.Printf("Removed: %s", path)
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