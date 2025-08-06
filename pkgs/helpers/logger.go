package helpers

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/writer"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

func InitLogger() {
	// Check if LOG_FILE environment variable is set
	logFile := os.Getenv("LOG_FILE")
	if logFile != "" {
		// Ensure log directory exists
		logDir := filepath.Dir(logFile)
		if err := os.MkdirAll(logDir, 0755); err != nil {
			fmt.Printf("Failed to create log directory: %v\n", err)
			// Fall back to stdout/stderr
			logFile = ""
		} else {
			// Open log file
			file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
			if err != nil {
				fmt.Printf("Failed to open log file: %v\n", err)
				// Fall back to stdout/stderr
				logFile = ""
			} else {
				// Write to both file and stdout/stderr
				log.SetOutput(io.MultiWriter(file, os.Stdout))
			}
		}
	}

	// If no log file or failed to open, use default behavior
	if logFile == "" {
		log.SetOutput(io.Discard) // Send all logs to nowhere by default

		log.AddHook(&writer.Hook{ // Send logs with level higher than warning to stderr
			Writer: os.Stderr,
			LogLevels: []log.Level{
				log.PanicLevel,
				log.FatalLevel,
				log.ErrorLevel,
				log.WarnLevel,
			},
		})
		log.AddHook(&writer.Hook{ // Send info and debug logs to stdout
			Writer: os.Stdout,
			LogLevels: []log.Level{
				log.TraceLevel,
				log.InfoLevel,
				log.DebugLevel,
			},
		})
	}

	log.SetReportCaller(true)

	if len(os.Args) < 2 {
		fmt.Println("Pass loglevel as an argument if you don't want default(INFO) to be set.")
		fmt.Println("Values to be passed for logLevel: ERROR(2),INFO(4),DEBUG(5)")
		log.SetLevel(log.DebugLevel)
	} else {
		logLevel, err := strconv.ParseUint(os.Args[1], 10, 32)
		if err != nil || logLevel > 6 {
			log.SetLevel(log.DebugLevel) //TODO: Change default level to error
		} else {
			//TODO: Need to come up with approach to dynamically update logLevel.
			log.SetLevel(log.Level(logLevel))
		}
	}
	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
}
