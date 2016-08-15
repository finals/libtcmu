package libtcmu

import (
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"

	"libtcmu/scsi"
)

const (
	SENSE_BUFFER_SIZE = 96
)

// ScsiCmd represents a single SCSI command received from the kernel to the virtual target.
type ScsiCmd struct {
	id        uint16
	cdb       []byte
	vecs      [][]byte
	offset    int
	vecoffset int
	vbd       *VirtBlockDevice

	//Buffer, if provided, may be used as a scratch buffer for copying data to and from the kernel.
	Buffer    []byte
}

// A ScsiResponse is generated from methods on ScscCmd.
type ScsiResponse struct {
	id          uint16
	status      byte
	senseBuffer []byte
}

// Command returns the SCSI command byte for the command. Useful when used as a comparision to the constants in the scsi package:
// c.Command() == scsi.Read6
func (cmd *ScsiCmd) Command() byte {
	return cmd.cdb[0]
}

// CdbLen returns the length of the command, in bytes.
func (cmd *ScsiCmd) CdbLen() int {
	opcode := cmd.cdb[0]
	// See spc-4 4.2.5.1 operation code
	//
	if opcode <= 0x1f {
		return 6
	} else if opcode <= 0x5f {
		return 10
	} else if opcode == 0x7f {
		return int(cmd.cdb[7]) + 8
	} else if opcode >= 0x80 && opcode <= 0x9f {
		return 16
	} else if opcode >= 0xa0 && opcode <= 0xbf {
		return 12
	}
	panic(fmt.Sprintf("what opcode is %x", opcode))
}

// LBA returns the block address that this command wishes to access.
func (cmd *ScsiCmd) LBA() uint64 {
	order := binary.BigEndian

	switch cmd.CdbLen() {
	case 6:
		val6 := uint8(order.Uint16(cmd.cdb[2:4]))
		if val6 == 0 {
			return 256
		}
		return uint64(val6)
	case 10:
		return uint64(order.Uint32(cmd.cdb[2:6]))
	case 12:
		return uint64(order.Uint32(cmd.cdb[2:6]))
	case 16:
		return uint64(order.Uint64(cmd.cdb[2:10]))
	default:
		panic("unusal scsi command length")
	}
}

// XferLen returns the length of the data buffer this command provides for transfering data to/from the kernel.
func (c *ScsiCmd) XferLen() uint32 {
	order := binary.BigEndian
	switch c.CdbLen() {
	case 6:
		return uint32(c.cdb[4])
	case 10:
		return uint32(order.Uint16(c.cdb[7:9]))
	case 12:
		return uint32(order.Uint32(c.cdb[6:10]))
	case 16:
		return uint32(order.Uint32(c.cdb[10:14]))
	default:
		log.Debugf("What XferLen has this length: %d", c.CdbLen())
		panic("unusal scsi command length")
	}
}

// Write, for a ScsiCmd is a io.Writer to the data buffer attached to this ScsiCmd command.
// It's writting *to* the buffer, which happens most commonly when responding to Read commands
// (take data and write it back to the kernel buffer)
func (cmd *ScsiCmd) Write(b []byte) (int, error) {
	toWrite := len(b)
	boff := 0
	for toWrite != 0 {
		if cmd.vecoffset == len(cmd.vecs) {
			return boff, errors.New("out of buffer scsi cmd buffer space")
		}
		wrote := copy(cmd.vecs[cmd.vecoffset][cmd.offset:], b[boff:])
		boff += wrote
		toWrite -= wrote
		cmd.offset += wrote
		if cmd.offset == len(cmd.vecs[cmd.vecoffset]) {
			cmd.vecoffset++
			cmd.offset = 0
		}
	}
	return boff, nil
}

// Read, for a ScsiCmd, is a io.Reader from the data buffer attached to this SCSI command.
// If there's data to be written to the virtual target, this is the way to access it.
func (cmd *ScsiCmd) Read(b []byte) (int, error) {
	toRead := len(b)
	boff := 0
	for toRead != 0 {
		if cmd.vecoffset == len(cmd.vecs) {
			return boff, io.EOF
		}
		read := copy(b[boff:], cmd.vecs[cmd.vecoffset][cmd.offset:])
		boff += read
		toRead -= read
		if cmd.offset == len(cmd.vecs[cmd.vecoffset]) {
			cmd.vecoffset++
			cmd.offset = 0
		}
	}
	return boff, nil
}

// VirtBlockDevice access the details of the SCSI device this command is handling.
func (cmd *ScsiCmd) VirtBlockDevice() *VirtBlockDevice {
	return cmd.vbd
}

// Ok creates a SCSI Response to this command with SAM_STAT_GOOD, the common case for commands that succeed.
func (cmd *ScsiCmd) Ok() ScsiResponse {
	return ScsiResponse{
		id: cmd.id,
		status: scsi.SamStatGood,
	}
}

// GetCDB returns the byte at `index` inside the command.
func (cmd *ScsiCmd) GetCDB(index int) byte {
	return cmd.cdb[index]
}

// ResponseStatus returns a ScsiResponse with the given status byte set. Ok() is equivalent to ResponseStatus(scsi.SamStatGood).
func (cmd *ScsiCmd) ResponseStatus(status byte) ScsiResponse {
	return ScsiResponse{
		id: cmd.id,
		status: status,
	}
}

// ResponseSenseData returns a ScsiResponse with the given status byte set and takes a byte array representing the
// SCSI sense data to be written.
func (cmd *ScsiCmd) ResponseSenseData(status byte, sense []byte) ScsiResponse {
	return ScsiResponse{
		id: cmd.id,
		status: status,
		senseBuffer: sense,
	}
}

// NotHandled creates a response and sense data that tells the kernel this device does not emulate this command.
func (cmd *ScsiCmd) NotHandled() ScsiResponse {
	buf := make([]byte, SENSE_BUFFER_SIZE)
	buf[0] = 0x70  // fixed, current
	buf[2] = 0x5   // illegal request
	buf[7] = 0xa
	buf[12] = 0x20 // ASC: invalid command operation code
	buf[13] = 0x0  // ASCQ: (none)

	return ScsiResponse{
		id: cmd.id,
		status: scsi.SamStatCheckCondition,
		senseBuffer: buf,
	}
}

// CheckCondition returns a response providing extra sense data. Takes a Sense Key and an Additional Sense Code.
func (cmd *ScsiCmd) CheckCondition(key byte, asc uint16) ScsiResponse {
	buf := make([]byte, SENSE_BUFFER_SIZE)
	buf[0] = 0x70  // fixed, current
	buf[2] = key
	buf[7] = 0xa
	buf[12] = byte(uint8((asc >> 8) & 0xff)) // high 8 bits
	buf[13] = byte(uint8(asc & 0xff))        // low 8 bits

	return ScsiResponse{
		id: cmd.id,
		status: scsi.SamStatCheckCondition,
		senseBuffer: buf,
	}
}

// MediumError is a preset response for a read error condition from the device
func (cmd *ScsiCmd) MediumError() ScsiResponse {
	return cmd.CheckCondition(scsi.SenseMediumError, scsi.AscReadError)
}

// IllegalRequest is a preset response for a request that is malformed or unexpected.
func (cmd *ScsiCmd) IllegalRequest() ScsiResponse {
	return cmd.CheckCondition(scsi.SenseIllegalRequest, scsi.AscInvalidFieldInCdb)
}

// Target Failure is a preset response for returning a hardware error.
func (cmd *ScsiCmd) TargetFailure() ScsiResponse {
	return cmd.CheckCondition(scsi.SenseHardwareError, scsi.AscInternalTargetFailure)
}

type DataSizes struct {
	VolumeSize int64
	BlockSize  int64
}

type DevReadyFunc func(chan *ScsiCmd, chan ScsiResponse) error

// ScsiHandler is the high-level data for the emulated SCSI device.
type ScsiHandler struct {
	// The volume name and resultant device name.
	VolumeName string
	// The size of the device and the blocksize for the device.
	DataSizes  DataSizes
	// The loopback HBA for the emulated SCSI device
	HBA        int
	// The LUN for the emulated HBA
	LUN        int
	// The Scsi World Wide Identifer for the device
	WWN        WWN
	// Called once the device is ready. Should spawn a goroutine (or several)
	// to handle commands coming in the first channel, and send their associated
	// responses down the second channel, ordering optional.
	DevReady   DevReadyFunc
}

// NaaWWN represents the World Wide Name of the SCSI device we are emulating, using the
// Network Address Authority standard.
type NaaWWN struct {
	// OUI is the first three bytes (six hex digits), in ASCII, of your
	// IEEE Organizationally Unique Identifier, eg, "05abcd".
	OUI         string
	// The VendorID is the first four bytes(eight hex digits), in ASCII, of
	// the device's vendor-specific ID (perhaps a serial number), eg, "2416c05f".
	VendorID    string
	// The VendorIDExt is an optional eight more bytes (16 hex digits) in the same format
	// as the above, if necessary.
	VendorIDExt string
}

func (n NaaWWN) DeviceID() string {
	return n.genID("1")
}

func (n NaaWWN) NexusID() string {
	return n.genID("1")
}

func (n NaaWWN) genID(s string) string {
	n.assertCorrect()
	naa := "naa.5"
	vend := n.VendorID + n.VendorIDExt
	if len(n.VendorIDExt) == 16 {
		naa = "naa.6"
	}
	return naa + n.OUI + s + vend
}

func (n NaaWWN) assertCorrect() {
	if len(n.OUI) != 6 {
		panic("OUI needs to be exactly 6 hex characters")
	}
	if len(n.VendorID) != 8 {
		panic("VendorID needs to be exactly 8 hex characters")
	}
	if len(n.VendorIDExt) != 0 && len(n.VendorIDExt) != 16 {
		panic("VendorIDExt needs to be zero or 16 hex characters")
	}
}

func GenerateSerial(name string) string {
	digest := md5.New()
	digest.Write([]byte(name))
	return hex.EncodeToString(digest.Sum([]byte{}))[:8]
}

func GenerateTestWWN(name string) WWN {
	return NaaWWN{
		OUI: "000000",
		VendorID: GenerateSerial(name),
	}
}

type ReadWriteAt interface {
	io.ReaderAt
	io.WriterAt
}

func BasicScsiHandler(rw ReadWriteAt) *ScsiHandler {
	return &ScsiHandler{
		HBA: 42,
		LUN:0,
		WWN: GenerateTestWWN("testvbd"),
		VolumeName: "testvbd",
		DataSizes: DataSizes{1024 * 1024 * 1024, 1024}, //1G, 1k
		DevReady: MultiThreadedDevReady(
			ReadWriteAtCmdHandler{
				RW: rw,
			},
			2,
		),
	}
}

func BasicScsiHandler2(rw ReadWriteAt) *ScsiHandler {
	return &ScsiHandler{
		HBA: 42,
		LUN:1,
		WWN: GenerateTestWWN("testvbd2"),
		VolumeName: "testvbd2",
		DataSizes: DataSizes{1024 * 1024 * 1024, 1024}, //1G, 1k
		DevReady: MultiThreadedDevReady(
			ReadWriteAtCmdHandler{
				RW: rw,
			},
			2,
		),
	}
}

func SingleThreadedDevReady(h ScsiCmdHandler) DevReadyFunc {
	return func(in chan *ScsiCmd, out chan ScsiResponse) error {
		go func(h ScsiCmdHandler, in chan *ScsiCmd, out chan ScsiResponse) {
			// Use io.Copy's trick
			buf := make([]byte, 32 * 1024)
			for {
				v, ok := <-in
				if !ok {
					close(out)
					return
				}
				v.Buffer = buf
				x, err := h.HandleCommand(v)
				buf = v.Buffer
				if err != nil {
					return
				}
				out <- x
			}
		}(h, in, out)
		return nil
	}
}

func MultiThreadedDevReady(h ScsiCmdHandler, threads int) DevReadyFunc {
	return func(in chan *ScsiCmd, out chan ScsiResponse) error {
		go func(h ScsiCmdHandler, in chan *ScsiCmd, out chan ScsiResponse, threads int) {
			w := sync.WaitGroup{}
			w.Add(threads)
			for i := 0; i < threads; i++ {
				go func(h ScsiCmdHandler, in chan *ScsiCmd, out chan ScsiResponse, w *sync.WaitGroup) {
					buf := make([]byte, 32 * 1024)
					for {
						v, ok := <-in
						if !ok {
							break
						}
						v.Buffer = buf
						x, err := h.HandleCommand(v)
						buf = v.Buffer
						if err != nil {
							return
						}
						out <- x
					}
					w.Done()
				}(h, in, out, &w)
			}
			w.Wait()
			close(out)
		}(h, in, out, threads)
		return nil
	}
}
