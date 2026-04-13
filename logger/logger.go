package logger

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var (
	fileLogger *log.Logger
	logFile    *os.File
	mu         sync.Mutex
)

// Init sets up file-based logging under ~/.mtssh/logs/
func Init() error {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	dir := filepath.Join(home, ".mtssh", "logs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}

	name := fmt.Sprintf("mtssh_%s.log", time.Now().Format("2006-01-02"))
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
	mu.Lock()
	if fileLogger != nil {
		fileLogger.Println(entry)
	}
	mu.Unlock()
}

// Error logs an error message
func Error(session, msg string) {
	entry := fmt.Sprintf("[%s] ERROR %s", session, msg)
	fmt.Fprintln(os.Stderr, entry)
	mu.Lock()
	if fileLogger != nil {
		fileLogger.Println(entry)
	}
	mu.Unlock()
}

// Close flushes and closes the log file
func Close() {
	mu.Lock()
	defer mu.Unlock()
	if logFile != nil {
		logFile.Close()
		logFile = nil
		fileLogger = nil
	}
}
