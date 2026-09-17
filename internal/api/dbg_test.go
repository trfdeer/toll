package api

import (
	"os"

	"github.com/charmbracelet/log"
)

func dbgLogger() *log.Logger {
	return log.NewWithOptions(os.Stderr, log.Options{Level: log.DebugLevel})
}
