package main

import (
	"fmt"
	"os"
	"os/signal"

	"libtcmu"
	"time"
)

func test1(hba *libtcmu.HBA) {
	filename := "vol11"
	f, err := os.OpenFile(filename, os.O_RDWR, 0700)
	if err != nil {
		die("couldn't open: %v", err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	handler := libtcmu.BasicScsiHandler(f)
	handler.VolumeName = fi.Name()
	handler.DataSizes.VolumeSize = fi.Size()
	d, err := hba.CreateDevice("/dev/tcmufile", handler)
	if err != nil {
		die("couldn't tcmu: %v", err)
	}
	defer d.Close()
	fmt.Printf("go-tcmu attached to %s/%s\n", "/dev/tcmufile", fi.Name())
	time.Sleep(time.Second)
	d.GenerateDevEntry()
	mainClose := make(chan bool)
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)

	go func() {
		for _ = range signalChan {
			fmt.Println("\nReceived an interrupt, stopping services...")
			close(mainClose)
		}
	}()
	<-mainClose
}

func main() {
	hba := libtcmu.NewHBA()
	hba.Start()
	time.Sleep(2 * time.Second)
	go test1(hba)

	mainClose := make(chan bool)
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)

	go func() {
		for _ = range signalChan {
			fmt.Println("\nReceived an interrupt, stopping services...")
			close(mainClose)
		}
	}()
	<-mainClose
}

func die(why string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, why + "\n", args...)
	os.Exit(1)
}