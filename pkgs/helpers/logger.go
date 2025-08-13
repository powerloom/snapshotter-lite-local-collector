package helpers

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/writer"
	"gopkg.in/natefinch/lumberjack.v2"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

func InitLogger() {
	// Set log level from environment first, then fall back to command line arg
	var level log.Level
	logLevelStr := os.Getenv("LOG_LEVEL")
	
	if logLevelStr != "" {
		// Try to parse from environment variable
		parsedLevel, err := log.ParseLevel(logLevelStr)
		if err != nil {
			level = log.InfoLevel
		} else {
			level = parsedLevel
		}
	} else if len(os.Args) >= 2 {
		// Fall back to command line argument for backwards compatibility
		logLevel, err := strconv.ParseUint(os.Args[1], 10, 32)
		if err != nil || logLevel > 6 {
			level = log.InfoLevel
		} else {
			level = log.Level(logLevel)
		}
	} else {
		// Default to info level instead of debug
		level = log.InfoLevel
	}
	
	log.SetLevel(level)
	log.SetReportCaller(true)
	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	
	// Check if LOG_FILE environment variable is set
	logFile := os.Getenv("LOG_FILE")
	if logFile != "" {
		// Ensure log directory exists
		logDir := filepath.Dir(logFile)
		if err := os.MkdirAll(logDir, 0755); err != nil {
			fmt.Printf("Failed to create log directory: %v\n", err)
			// Fall back to stdout/stderr split
			setupConsoleLogging()
			return
		}
		
		// Parse rotation settings from environment
		maxSize := 100 // Default 100MB
		if val := os.Getenv("LOG_MAX_SIZE_MB"); val != "" {
			if size, err := strconv.Atoi(val); err == nil {
				maxSize = size
			}
		}
		
		maxBackups := 5 // Default keep 5 old files
		if val := os.Getenv("LOG_MAX_BACKUPS"); val != "" {
			if backups, err := strconv.Atoi(val); err == nil {
				maxBackups = backups
			}
		}
		
		maxAge := 30 // Default 30 days
		if val := os.Getenv("LOG_MAX_AGE_DAYS"); val != "" {
			if age, err := strconv.Atoi(val); err == nil {
				maxAge = age
			}
		}
		
		compress := false
		if val := os.Getenv("LOG_COMPRESS"); val == "true" || val == "1" {
			compress = true
		}
		
		// Create rotating log writer
		rotator := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    maxSize,    // megabytes
			MaxBackups: maxBackups, // number of old files
			MaxAge:     maxAge,     // days
			Compress:   compress,   // compress old files
			LocalTime:  true,
		}
		
		// Write to both rotating file and stdout
		multiWriter := io.MultiWriter(rotator, os.Stdout)
		log.SetOutput(multiWriter)
		
		log.WithFields(log.Fields{
			"file":       logFile,
			"maxSize":    maxSize,
			"maxBackups": maxBackups,
			"maxAge":     maxAge,
			"compress":   compress,
			"logLevel":   level.String(),
		}).Info("Initialized rotating logger")
	} else {
		// If no log file, use stdout/stderr split
		setupConsoleLogging()
		log.WithField("logLevel", level.String()).Info("Initialized console logger")
	}
}

func setupConsoleLogging() {
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
