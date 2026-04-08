package logger

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

var fileLogger *log.Logger
var logFile *os.File

// Init sets up file-based logging under ~/.mtputty/logs/
func Init() error {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".mtputty", "logs")
	os.MkdirAll(dir, 0700)

	name := fmt.Sprintf("mtputty_%s.log", time.Now().Format("2006-01-02"))
	path := filepath.Join(dir, name)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	logFile = f
	fileLogger = log.New(f, "", log.LstdFlags)
	return nil
}

// Info logs an informational message
func Info(session, msg string) {
	entry := fmt.Sprintf("[%s] INFO  %s", session, msg)
	fmt.Println(entry)
	if fileLogger != nil {
		fileLogger.Println(entry)
	}
}

// Error logs an error message
func Error(session, msg string) {
	entry := fmt.Sprintf("[%s] ERROR %s", session, msg)
	fmt.Fprintln(os.Stderr, entry)
	if fileLogger != nil {
		fileLogger.Println(entry)
	}
}

// Close flushes and closes the log file
func Close() {
	if logFile != nil {
		logFile.Close()
	}
}
