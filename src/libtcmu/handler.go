package libtcmu

import (
	"fmt"

	"libtcmu/scsi"

	"golang.org/x/sys/unix"
)

func (vbd *VirtBlockDevice) beginPoll() {
	// Entry point for the goroutine.
	go vbd.recvResponse()

	buf := make([]byte, 4)
	for {
		var n int
		var err error
		n, err = unix.Read(vbd.uioFd, buf)
		if n == -1 && err != nil {
			fmt.Println(err)
			break
		}
		for {
			cmd, err := vbd.getNextCommand()
			if err != nil {
				fmt.Printf("error getting next command: %s", err)
				break
			}
			if cmd == nil {
				break
			}
			vbd.cmdChan <- cmd
		}
	}
	close(vbd.cmdChan)
}

func (vbd *VirtBlockDevice) recvResponse() {
	var n int
	buf := make([]byte, 4)
	for resp := range vbd.respChan {
		err := vbd.completeCommand(resp)
		if err != nil {
			fmt.Printf("error completing command: %s", err)
			return
		}
		/* Tell the fd there's something new */
		n, err = unix.Write(vbd.uioFd, buf)
		if n == -1 && err != nil {
			fmt.Printf("poll write")
			return
		}
	}
}

func (vbd *VirtBlockDevice)  completeCommand(resp ScsiResponse) error {
	off := vbd.tailEntryOff()
	for vbd.entHdrOp(off) != tcmuOpCmd {
		vbd.mbSetTail((vbd.mbCmdTail() + uint32(vbd.entHdrGetLen(off))) % vbd.mbCmdrSize())
		off = vbd.tailEntryOff()
	}
	if vbd.entCmdId(off) != resp.id {
		vbd.setEntCmdId(off, resp.id)
	}
	vbd.setEntRespSCSIStatus(off, resp.status)
	if resp.status != scsi.SamStatGood {
		vbd.copyEntRespSenseData(off, resp.senseBuffer)
	}
	vbd.mbSetTail((vbd.mbCmdTail() + uint32(vbd.entHdrGetLen(off))) % vbd.mbCmdrSize())
	return nil
}

func (vbd *VirtBlockDevice) getNextCommand() (*ScsiCmd, error) {
	//d.debugPrintMb()
	//fmt.Printf("nextEntryOff: %d\n", d.nextEntryOff())
	//fmt.Printf("headEntryOff: %d\n", d.headEntryOff())
	for vbd.nextEntryOff() != vbd.headEntryOff() {
		off := vbd.nextEntryOff()
		if vbd.entHdrOp(off) == tcmuOpPad {
			vbd.cmdTail = (vbd.cmdTail + uint32(vbd.entHdrGetLen(off))) % vbd.mbCmdrSize()
		} else if vbd.entHdrOp(off) == tcmuOpCmd {
			//d.printEnt(off)
			out := &ScsiCmd{
				id:     vbd.entCmdId(off),
				vbd: vbd,
			}
			out.cdb = vbd.entCdb(off)
			vecs := int(vbd.entReqIovCnt(off))
			out.vecs = make([][]byte, vecs)
			for i := 0; i < vecs; i++ {
				v := vbd.entIovecN(off, i)
				out.vecs[i] = v
			}
			vbd.cmdTail = (vbd.cmdTail + uint32(vbd.entHdrGetLen(off))) % vbd.mbCmdrSize()
			return out, nil
		} else {
			panic(fmt.Sprintf("unsupported command from tcmu? %d", vbd.entHdrOp(off)))
		}
	}
	return nil, nil
}

func (vbd *VirtBlockDevice) printEnt(off int) {
	for i, x := range vbd.mmap[off : off + vbd.entHdrGetLen(off)] {
		fmt.Printf("0x%02x ", x)
		if i % 16 == 15 {
			fmt.Printf("\n")
		}
	}
}

func (vbd *VirtBlockDevice)nextEntryOff() int {
	return int(vbd.cmdTail + vbd.mbCmdrOffset())
}

func (vbd *VirtBlockDevice) headEntryOff() int {
	return int(vbd.mbCmdHead() + vbd.mbCmdrOffset())
}

func (vbd *VirtBlockDevice) tailEntryOff() int {
	return int(vbd.mbCmdTail() + vbd.mbCmdrOffset())
}