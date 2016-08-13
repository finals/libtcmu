package libtcmu

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"unsafe"
)

var byteOrder binary.ByteOrder = binary.LittleEndian

func (vbd *VirtBlockDevice) mbVersion() uint16 {
	return *(*uint16) (unsafe.Pointer(&vbd.mmap[0]))
}

func (vbd *VirtBlockDevice) mbFlags() uint16 {
	return *(*uint16)(unsafe.Pointer(&vbd.mmap[2]))
}

func (vbd *VirtBlockDevice) mbCmdrOffset() uint32 {
	return *(*uint32)(unsafe.Pointer(&vbd.mmap[4]))
}

func (vbd *VirtBlockDevice) mbCmdrSize() uint32 {
	return *(*uint32)(unsafe.Pointer(&vbd.mmap[8]))
}

func (vbd *VirtBlockDevice)mbCmdHead() uint32 {
	return *(*uint32)(unsafe.Pointer(&vbd.mmap[12]))
}

func (vbd *VirtBlockDevice) mbCmdTail() uint32 {
	return *(*uint32)(unsafe.Pointer(&vbd.mmap[64]))
}

func (vbd *VirtBlockDevice) mbSetTail(u uint32) {
	byteOrder.PutUint32(vbd.mmap[64:], u)
}

/*
enum tcmu_opcode {
  TCMU_OP_PAD = 0,
  TCMU_OP_CMD,
};
*/
type tcmuOpcode int

const (
	tcmuOpPad tcmuOpcode = 0
	tcmuOpCmd = 1
)

/*

// Only a few opcodes, and length is 8-byte aligned, so use low bits for opcode.
struct tcmu_cmd_entry_hdr {
  __u32 len_op;
  __u16 cmd_id;
  __u8 kflags;
#define TCMU_UFLAG_UNKNOWN_OP 0x1
  __u8 uflags;

} __packed;
*/
func (vbd *VirtBlockDevice) entHdrOp(off int) tcmuOpcode {
	i := int(*(*uint32)(unsafe.Pointer(&vbd.mmap[off + offLenOp])))
	i = i & 0x7
	return tcmuOpcode(i)
}

func (vbd *VirtBlockDevice) entHdrGetLen(off int) int {
	i := *(*uint32)(unsafe.Pointer(&vbd.mmap[off + offLenOp]))
	i = i &^ 0x7
	return int(i)
}

func (vbd *VirtBlockDevice) entCmdId(off int) uint16 {
	return *(*uint16)(unsafe.Pointer(&vbd.mmap[off + offCmdId]))
}
func (vbd *VirtBlockDevice) setEntCmdId(off int, id uint16) {
	*(*uint16)(unsafe.Pointer(&vbd.mmap[off + offCmdId])) = id
}
func (vbd *VirtBlockDevice) entKflags(off int) uint8 {
	return *(*uint8)(unsafe.Pointer(&vbd.mmap[off + offKFlags]))
}
func (vbd *VirtBlockDevice) entUflags(off int) uint8 {
	return *(*uint8)(unsafe.Pointer(&vbd.mmap[off + offUFlags]))
}

func (vbd *VirtBlockDevice) setEntUflagUnknownOp(off int) {
	vbd.mmap[off + offUFlags] = 0x01
}

/*
#define TCMU_SENSE_BUFFERSIZE 96

struct tcmu_cmd_entry {
	  struct tcmu_cmd_entry_hdr hdr;

		union {
			struct {
				uint32_t iov_cnt; 0
				uint32_t iov_bidi_cnt; 4
				uint32_t iov_dif_cnt; 8
				uint64_t cdb_off; 12
				uint64_t __pad1; 20
				uint64_t __pad2; 28
				struct iovec iov[0];

			} req;
			struct {
				uint8_t scsi_status;
				uint8_t __pad1;
				uint16_t __pad2;
				uint32_t __pad3;
				char sense_buffer[TCMU_SENSE_BUFFERSIZE];

			} rsp;
		};
} __packed;
*/

func (vbd *VirtBlockDevice) entReqIovCnt(off int) uint32 {
	return *(*uint32)(unsafe.Pointer(&vbd.mmap[off + offReqIovCnt]))
}

func (vbd *VirtBlockDevice) entReqIovBidiCnt(off int) uint32 {
	return *(*uint32)(unsafe.Pointer(&vbd.mmap[off + offReqIovBidiCnt]))
}

func (vbd *VirtBlockDevice) entReqIovDifCnt(off int) uint32 {
	return *(*uint32)(unsafe.Pointer(&vbd.mmap[off + offReqIovDifCnt]))
}

func (vbd *VirtBlockDevice) entReqCdbOff(off int) uint64 {
	return *(*uint64)(unsafe.Pointer(&vbd.mmap[off + offReqCdbOff]))
}

func (vbd *VirtBlockDevice) setEntRespSCSIStatus(off int, status byte) {
	vbd.mmap[off + offRespSCSIStatus] = status
}

func (vbd *VirtBlockDevice) copyEntRespSenseData(off int, data []byte) {
	buf := vbd.mmap[off + offRespSense : off + offRespSense + SENSE_BUFFER_SIZE]
	copy(buf, data)
	if len(data) < SENSE_BUFFER_SIZE {
		for i := len(data); i < SENSE_BUFFER_SIZE; i++ {
			buf[i] = 0
		}
	}
}

func (vbd *VirtBlockDevice) entIovecN(off int, idx int) []byte {
	out := syscall.Iovec{}
	p := unsafe.Pointer(&vbd.mmap[off + offReqIov0Base])
	out = *(*syscall.Iovec)(unsafe.Pointer(uintptr(p) + uintptr(idx) * unsafe.Sizeof(out)))
	moff := *(*int)(unsafe.Pointer(&out.Base))
	return vbd.mmap[moff : moff + int(out.Len)]
}

func (vbd *VirtBlockDevice) entCdb(off int) []byte {
	cdbStart := int(vbd.entReqCdbOff(off))
	len := vbd.cdbLen(cdbStart)
	return vbd.mmap[cdbStart : cdbStart + len]
}

func (vbd *VirtBlockDevice) cdbLen(cdbStart int) int {
	opcode := vbd.mmap[cdbStart]
	// See spc-4 4.2.5.1 operation code
	//
	if opcode <= 0x1f {
		return 6
	} else if opcode <= 0x5f {
		return 10
	} else if opcode == 0x7f {
		return int(vbd.mmap[cdbStart + 7]) + 8
	} else if opcode >= 0x80 && opcode <= 0x9f {
		return 16
	} else if opcode >= 0xa0 && opcode <= 0xbf {
		return 12
	} else {
		panic(fmt.Sprintf("what opcode is %x", opcode))
	}
}
