package libtcmu

import (
	"fmt"

	"libtcmu/scsi"

	"golang.org/x/sys/unix"
)

func (vbd *VirBlkDev) beginPoll() {
	// Entry point for the goroutine.
	go vbd.recvResponse()

	buf := make([]byte, 4)
	for {
		var n int
		var err error
		n, err = unix.Read(vbd.uioFd, buf)
		if n == -1 && err != nil {
			log.Debugf(err.Error())
			break
		}
		for {
			cmd, err := vbd.getNextCommand()
			if err != nil {
				log.Debugf("error getting next command: %s", err.Error())
				break
			}
			if cmd == nil {
				break
			}
			vbd.cmdChan <- cmd
		}
	}
	close(vbd.cmdChan)
	log.Infof("beginPoll exit")
}

func (vbd *VirBlkDev) startPoll() {
	// Entry point for the goroutine.
	go vbd.recvResponse()

	buf := make([]byte, 4)
	for {
		pfd := []unix.PollFd{
			{
				Fd:      int32(vbd.uioFd),
				Events:  unix.POLLIN,
				Revents: 0,
			},
			{
				Fd:      int32(vbd.pipeFds[0]),
				Events:  unix.POLLIN,
				Revents: 0,
			},
		}

		_, err := unix.Poll(pfd, -1)
		if err != nil {
			fmt.Println("Poll command failed: ", err)
			break
		}
		if pfd[1].Revents == unix.POLLIN {
			fmt.Println("Poll command receive finish signal")
			vbd.wait <- struct{}{}
			return
		}

		if pfd[0].Revents != 0 && pfd[0].Events != unix.POLLIN {
			fmt.Println("Poll received unexpect event: ", pfd[0].Revents)
			continue
		}

		var n int
		n, err = unix.Read(vbd.uioFd, buf)
		if n == -1 && err != nil {
			fmt.Println(err.Error())
			break
		}
		for {
			cmd, err := vbd.getNextCommand()
			if err != nil {
				fmt.Println("error getting next command: %s", err.Error())
				break
			}
			if cmd == nil {
				//fmt.Println("cmd == nil uioFd: ", vbd.uioFd)
				break
			}

			vbd.cmdChan <- cmd
		}
	}
	//close(vbd.cmdChan)
	//log.Infof("beginPoll exit")
}

func (vbd *VirBlkDev) stopPoll() {
	if vbd.initialize {
		if _, err := unix.Write(vbd.pipeFds[1], []byte{0}); err != nil {
			log.Errorln("Fail to notify poll for finishing: ", err)
		}
	}
}

func (vbd *VirBlkDev) recvResponse() {
	var n int
	buf := make([]byte, 4)
	for resp := range vbd.respChan {
		err := vbd.completeCommand(resp)
		if err != nil {
			log.Debugf("error completing command: %s", err.Error())
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

func (vbd *VirBlkDev) completeCommand(resp ScsiResponse) error {
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

func (vbd *VirBlkDev) getNextCommand() (*ScsiCmd, error) {
	//vbd.debugPrintMb()
	//fmt.Printf("nextEntryOff: %d\n", vbd.nextEntryOff())
	//fmt.Printf("headEntryOff: %d\n", vbd.headEntryOff())
	for vbd.nextEntryOff() != vbd.headEntryOff() {
		off := vbd.nextEntryOff()
		if vbd.entHdrOp(off) == tcmuOpPad {
			vbd.cmdTail = (vbd.cmdTail + uint32(vbd.entHdrGetLen(off))) % vbd.mbCmdrSize()
		} else if vbd.entHdrOp(off) == tcmuOpCmd {
			//d.printEnt(off)
			out := &ScsiCmd{
				id:  vbd.entCmdId(off),
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

func (vbd *VirBlkDev) printEnt(off int) {
	for i, x := range vbd.mmap[off : off+vbd.entHdrGetLen(off)] {
		fmt.Printf("0x%02x ", x)
		if i%16 == 15 {
			fmt.Printf("\n")
		}
	}
}

func (vbd *VirBlkDev) nextEntryOff() int {
	return int(vbd.cmdTail + vbd.mbCmdrOffset())
}

func (vbd *VirBlkDev) headEntryOff() int {
	return int(vbd.mbCmdHead() + vbd.mbCmdrOffset())
}

func (vbd *VirBlkDev) tailEntryOff() int {
	return int(vbd.mbCmdTail() + vbd.mbCmdrOffset())
}
