package tcmu

import (
	"github.com/Sirupsen/logrus"
	"io"
)

var (
	log = logrus.WithFields(logrus.Fields{"pkg": "tcmu"})
)

type ReadWriteAt interface {
	io.ReaderAt
	io.WriterAt
}