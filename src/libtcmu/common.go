package tcmu

import (
	"github.com/Sirupsen/logrus"
	"io"
	"io/ioutil"
	"strings"
	"os"
)

var (
	log = logrus.WithFields(logrus.Fields{"pkg": "tcmu"})
)

type ReadWriteAt interface {
	io.ReaderAt
	io.WriterAt
}

func IsTcmuDevice(bd string) (bool, error) {
	blockdevice := strings.TrimLeft(bd, "/dev/")
	buf, err := ioutil.ReadFile("/sys/block/" + blockdevice + "/device/model")
	if err != nil {
		return false, err
	}
	return strings.Contains(string(buf), "TCMU"), nil
}

func IsDirExists(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return os.IsExist(err)
	} else {
		return fi.IsDir()
	}
}
