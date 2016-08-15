package main

import (
	"fmt"
	"os"
	"os/signal"

	"libtcmu"
	"time"
)

func test1() {
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
	d, err := libtcmu.NewVirtBlockDevice("/dev/tcmufile", handler)
	if err != nil {
		die("couldn't tcmu: %v", err)
	}
	defer d.Close()
	fmt.Printf("go-tcmu attached to %s/%s\n", "/dev/tcmufile", fi.Name())

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

func test2() {
	filename := "vol22"
	f, err := os.OpenFile(filename, os.O_RDWR, 0700)
	if err != nil {
		die("couldn't open: %v", err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	handler := libtcmu.BasicScsiHandler2(f)
	handler.VolumeName = fi.Name()
	handler.DataSizes.VolumeSize = fi.Size()
	d, err := libtcmu.NewVirtBlockDevice("/dev/tcmufile", handler)
	if err != nil {
		die("couldn't tcmu: %v", err)
	}
	defer d.Close()
	fmt.Printf("go-tcmu attached to %s/%s\n", "/dev/tcmufile", fi.Name())

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
	go test1()
	//go test2()

	for {
		time.Sleep(30 * time.Second)
	}
}

func die(why string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, why + "\n", args...)
	os.Exit(1)
}