package tcmu

import (
	"bytes"
	"encoding/binary"
	"io"

	"libtcmu/scsi"
	//"crypto/md5"
)

// ScsiCmdHandler is a simple request/response handler for SCSI commands commint to TCMU
// A SCSI error is reported as an SCSIResponse with an error bit set, while returning a Go error is for flagrant,
// process-ending errors (OOM, perhaps)
type ScsiCmdHandler interface {
	HandleCommand(cmd *ScsiCmd) (ScsiResponse, error)
}

type ReadWriteAtCmdHandler struct {
	RW  ReadWriteAt
	Inq *InquiryInfo
}

// InquiryInfo holds the general vendor information for the emulated SCSI Device.
// Fields used from this will be padded or truncated to meet the spec.
type InquiryInfo struct {
	VendorID   string
	ProductID  string
	ProductRev string
}

var defaultInquiry = InquiryInfo{
	VendorID:   "libtcmu",
	ProductID:  "TCMU Device",
	ProductRev: "0001",
}

func (h ReadWriteAtCmdHandler) HandleCommand(cmd *ScsiCmd) (ScsiResponse, error) {
	switch cmd.Command() {
	case scsi.TestUnitReady:
		return EmulateTestUnitReady(cmd)
	case scsi.Inquiry:
		if h.Inq == nil {
			h.Inq = &defaultInquiry
		}
		return EmulateInquiry(cmd, h.Inq)
	case scsi.Read6, scsi.Read10, scsi.Read12, scsi.Read16:
		return EmulateRead(cmd, h.RW)
	case scsi.Write6, scsi.Write10, scsi.Write12, scsi.Write16:
		return EmulateWrite(cmd, h.RW)
	case scsi.ServiceActionIn16:
		return EmulateServiceActionIn(cmd)
	case scsi.ModeSense, scsi.ModeSense10:
		return EmulateModeSense(cmd, false)
	case scsi.ModeSelect, scsi.ModeSelect10:
		return EmulateModeSelect(cmd, false)
	default:
		return cmd.NotHandled(), nil
	}
}

func EmulateInquiry(cmd *ScsiCmd, inq *InquiryInfo) (ScsiResponse, error) {
	if (cmd.GetCDB(1) & 0x01) == 0 {
		if cmd.GetCDB(2) == 0x00 {
			return EmulateStdInquiry(cmd, inq)
		}
		return cmd.IllegalRequest(), nil
	}
	return EmulateEvpdInquiry(cmd, inq)
}

func FixedString(s string, length int) []byte {
	p := []byte(s)
	l := len(p)
	if l >= length {
		return p[:length]
	}
	sp := bytes.Repeat([]byte{' '}, length-l)
	return append(p, sp...)
}

func EmulateStdInquiry(cmd *ScsiCmd, inq *InquiryInfo) (ScsiResponse, error) {
	buf := make([]byte, 36)
	buf[2] = 0x05 // SPC-3
	buf[3] = 0x02 // response data format
	buf[7] = 0x02 // CmdQue

	vendorID := FixedString(inq.VendorID, 8)
	copy(buf[8:16], vendorID)
	productID := FixedString(inq.ProductID, 16)
	copy(buf[16:32], productID)
	productRev := FixedString(inq.ProductRev, 4)
	copy(buf[32:36], productRev)

	buf[4] = 31 // Set additional length to 31
	_, err := cmd.Write(buf)
	if err != nil {
		return ScsiResponse{}, err
	}

	return cmd.Ok(), nil
}

func EmulateEvpdInquiry(cmd *ScsiCmd, inq *InquiryInfo) (ScsiResponse, error) {
	vpdType := cmd.GetCDB(2)

	switch vpdType {
	case 0x0: // Supported VPD pages
		// The absolute minimum.
		data := make([]byte, 6)

		// support 0x00 and 0x83 only
		data[3] = 2
		data[4] = 0x00
		data[5] = 0x83

		cmd.Write(data)
		return cmd.Ok(), nil
	case 0x83: // Device identification
		used := 4
		data := make([]byte, 512)
		data[1] = 0x83
		wwn := []byte("") // TODO(barakmich): Report WWN. See tcmu_get_wwwn;

		// 1/3: T10 Vendor id
		ptr := data[used:]
		ptr[0] = 2 // code set: ASCII
		ptr[1] = 1 // identifier: T10 vendor id
		copy(ptr[4:], FixedString(inq.VendorID, 8))
		n := copy(ptr[12:], wwn)
		ptr[3] = byte(8 + n + 1)
		used += int(ptr[3]) + 4

		// 2/3: NAA binary  // TODO(barakmich): Emulate given a real WWN
		ptr = data[used:]
		ptr[0] = 1  // code set: binary
		ptr[1] = 3  // identifier: NAA
		ptr[3] = 16 // body length for naa registered extended format

		// Set type 6 and use OpenFabrics IEEE Company ID: 00 14 05
		ptr[4] = 0x60
		ptr[5] = 0x01
		ptr[6] = 0x40
		ptr[7] = 0x50
		next := true
		i := 7

		for _, x := range wwn {
			if i >= 20 {
				break
			}
			v, ok := charToHex(x)
			if !ok {
				continue
			}

			if next {
				next = false
				ptr[i] |= v
				i++
			} else {
				next = true
				ptr[i] = (v << 4)
			}
		}
		used += 20

		// 3/3: Vendor specific
		ptr = data[used:]
		ptr[0] = 2 // code set: ASCII
		ptr[1] = 0 // identifier: vendor-specific

		cfgString := cmd.VirBlkDev().GetDevConfig()
		n = copy(ptr[4:], []byte(cfgString))
		ptr[3] = byte(n + 1)

		used += n + 1 + 4

		order := binary.BigEndian
		order.PutUint16(data[2:4], uint16(used-4))

		cmd.Write(data[:used])
		return cmd.Ok(), nil
	case 0xb0:  // Block Limits
		data := make([]byte, 64)
		data[1] = 0xb0

		order := binary.BigEndian
		order.PutUint16(data[2:4], uint16(0x3c))

		blockSize, err := cmd.VirBlkDev().GetDeviceAttr("hw_block_size")
		if err != nil {
			return cmd.IllegalRequest(), nil
		}

		maxSectors, err := cmd.VirBlkDev().GetDeviceAttr("hw_max_sectors")
		if err != nil {
			return cmd.IllegalRequest(), nil
		}

		masXferLength := maxSectors / (blockSize / 512)
		order = binary.BigEndian
		order.PutUint32(data[8:12], uint32(masXferLength))
		order.PutUint32(data[12:16], uint32(masXferLength))
		cmd.Write(data[:64])
		return cmd.Ok(), nil
	default:
		return cmd.IllegalRequest(), nil
	}
}

func EmulateTestUnitReady(cmd *ScsiCmd) (ScsiResponse, error) {
	return cmd.Ok(), nil
}

func EmulateServiceActionIn(cmd *ScsiCmd) (ScsiResponse, error) {
	if cmd.GetCDB(1) == scsi.ReadCapacity16 {
		return EmulateReadCapacity16(cmd)
	}
	return cmd.NotHandled(), nil
}

func EmulateReadCapacity16(cmd *ScsiCmd) (ScsiResponse, error) {
	buf := make([]byte, 32)
	order := binary.BigEndian
	// This is in LBAs, and the "index of the last LBA", so minus 1. Friggin spec.
	order.PutUint64(buf[0:8], uint64(cmd.VirBlkDev().Sizes().VolumeSize/cmd.VirBlkDev().Sizes().SectorSize)-1)
	// This is in BlockSize
	order.PutUint32(buf[8:12], uint32(cmd.VirBlkDev().Sizes().SectorSize))
	// All the rest is 0
	cmd.Write(buf)
	return cmd.Ok(), nil
}

func charToHex(c byte) (byte, bool) {
	if c >= '0' && c <= '9' {
		return c - '0', true
	}
	if c >= 'a' && c <= 'f' {
		return c - 'a' + 10, true
	}
	if c >= 'A' && c <= 'F' {
		return c - 'A' + 10, true
	}
	return 0x00, false
}

func CachingModePage(w io.Writer, wce bool) {
	buf := make([]byte, 20)
	buf[0] = 0x08 // caching mode page
	buf[1] = 0x12 // page length (20, forced)
	//buf[2] = buf[2] | 0x01  //set RCD
	if wce {
		buf[2] = buf[2] | 0x04  //set WCE
	}
	w.Write(buf)
}

// EmulateModeSense responds to a static Mode Sense command. `wce` enables or diables
// the SCSI "Write Cache Enabled" flag.
func EmulateModeSense(cmd *ScsiCmd, wce bool) (ScsiResponse, error) {
	pgs := &bytes.Buffer{}
	outlen := int(cmd.XferLen())

	page := cmd.GetCDB(2)
	if page == 0x3f || page == 0x08 {
		CachingModePage(pgs, wce)
	}
	scsiCmd := cmd.Command()

	dsp := byte(0x10) // Support DPO/FUA

	pgdata := pgs.Bytes()
	var hdr []byte
	if scsiCmd == scsi.ModeSense {
		// MODE_SENSE_6
		hdr = make([]byte, 4)
		hdr[0] = byte(len(pgdata) + 3)
		hdr[1] = 0x00 // Device type
		hdr[2] = dsp
	} else {
		// MODE_SENSE_10
		hdr = make([]byte, 8)
		order := binary.BigEndian
		order.PutUint16(hdr, uint16(len(pgdata)+6))
		hdr[2] = 0x00 // Device type
		hdr[3] = dsp
	}
	data := append(hdr, pgdata...)
	if outlen < len(data) {
		data = data[:outlen]
	}
	cmd.Write(data)
	return cmd.Ok(), nil
}

// EmulateModeSelect checks that the only mode selected is the static one returned from
// EmulateModeSense. `wce` should match the Write Cache Enabled of the EmulateModeSense call.
func EmulateModeSelect(cmd *ScsiCmd, wce bool) (ScsiResponse, error) {
	selectTen := (cmd.GetCDB(0) == scsi.ModeSelect10)
	page := cmd.GetCDB(2) & 0x3f
	subpage := cmd.GetCDB(3)
	allocLen := cmd.XferLen()
	hdrLen := 4
	if selectTen {
		hdrLen = 8
	}
	inBuf := make([]byte, 512)
	gotSense := false

	if allocLen == 0 {
		return cmd.Ok(), nil
	}
	n, err := cmd.Read(inBuf)
	if err != nil {
		return ScsiResponse{}, err
	}
	if n >= len(inBuf) {
		return cmd.CheckCondition(scsi.SenseIllegalRequest, scsi.AscParameterListLengthError), nil
	}

	cdbone := cmd.GetCDB(1)
	if cdbone&0x10 == 0 || cdbone&0x01 != 0 {
		return cmd.IllegalRequest(), nil
	}

	pgs := &bytes.Buffer{}
	// TODO(barakmich): select over handlers. Today we have one.
	if page == 0x08 && subpage == 0 {
		CachingModePage(pgs, wce)
		gotSense = true
	}
	if !gotSense {
		return cmd.IllegalRequest(), nil
	}
	b := pgs.Bytes()
	if int(allocLen) < (hdrLen + len(b)) {
		return cmd.CheckCondition(scsi.SenseIllegalRequest, scsi.AscParameterListLengthError), nil
	}
	/* Verify what was selected is identical to what sense returns, since we
	don't support actually setting anything. */
	if !bytes.Equal(inBuf[hdrLen:len(b)], b) {
		//log.Errorf("not equal for some reason: %#v %#v", inBuf[hdrLen:len(b)], b)
		return cmd.CheckCondition(scsi.SenseIllegalRequest, scsi.AscInvalidFieldInParameterList), nil
	}
	return cmd.Ok(), nil
}

func EmulateRead(cmd *ScsiCmd, r io.ReaderAt) (ScsiResponse, error) {
	offset := cmd.LBA() * uint64(cmd.VirBlkDev().Sizes().SectorSize)
	length := int(cmd.XferLen() * uint32(cmd.VirBlkDev().Sizes().SectorSize))
    //log.Debugf("EmulateRead offset:%d length:%d", offset, length)
	cmd.Buffer = make([]byte, length)
	/*
		if cmd.Buffer == nil {
			cmd.Buffer = make([]byte, length)
		}
		if len(cmd.Buffer) < int(length) {
			//realloc
			cmd.Buffer = make([]byte, length)
		}
	*/
	n, err := r.ReadAt(cmd.Buffer, int64(offset))
	if n < length {
		log.Errorf("[EmulateRead] ReadAt failed: unable to copy enough")
		return cmd.MediumError(), nil
	}
	if err != nil {
		log.Errorf("[EmulateRead] Read error: %v", err)
		return cmd.MediumError(), nil
	}
	//log.Debugf("[EmulateRead] recv type:%d seq:%d offset:%d size:%d md5:%x", 0, 0, offset, len(cmd.Buffer), md5.Sum(cmd.Buffer))
	n, err = cmd.Write(cmd.Buffer)
	if n < length {
		log.Errorf("[EmulateRead] Write failed: unable to copy enough")
		return cmd.MediumError(), nil
	}

	if err != nil {
		log.Errorf("[EmulateRead] read/write failed: error:", err.Error())
		return cmd.MediumError(), nil
	}

	return cmd.Ok(), nil
}

func EmulateWrite(cmd *ScsiCmd, r io.WriterAt) (ScsiResponse, error) {
	offset := cmd.LBA() * uint64(cmd.VirBlkDev().Sizes().SectorSize)
	length := int(cmd.XferLen() * uint32(cmd.VirBlkDev().Sizes().SectorSize))
	//log.Debugf("EmulateWrite offset:%d length:%d", offset, length)
	cmd.Buffer = make([]byte, length)
	/*
		if cmd.Buffer == nil {
			cmd.Buffer = make([]byte, length)
		}
		if len(cmd.Buffer) < int(length) {
			//realloc
			cmd.Buffer = make([]byte, length)
		}
	*/

	n, err := cmd.Read(cmd.Buffer)
	if n < length {
		log.Debugf("write/read failed: unable to copy enough")
		return cmd.MediumError(), nil
	}
	if err != nil {
		log.Debugf("write/read failed: error:", err.Error())
		return cmd.MediumError(), nil
	}

	n, err = r.WriteAt(cmd.Buffer, int64(offset))
	if n < length {
		log.Debugf("write/write failed: unable to copy enough")
		return cmd.MediumError(), nil
	}
	if err != nil {
		log.Debugf("read/write failed: error:", err.Error())
		return cmd.MediumError(), nil
	}

	return cmd.Ok(), nil
}
