package helpers

import (
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/writer"
	"io"
	"os"
	"proto-snapshot-server/config"
)

func InitLogger() {
	log.SetOutput(io.Discard) // Send all logs to nowhere by default

	log.SetReportCaller(true)

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

	// Set log level based on config.SettingsObj.LogLevel
	lvl, err := log.ParseLevel(config.SettingsObj.LogLevel)
	if err != nil {
		log.Warnf("Invalid log level '%s' from config, defaulting to 'info'", config.SettingsObj.LogLevel)
		lvl = log.InfoLevel
	}
	log.SetLevel(lvl)
	log.Infof("Log level set to '%s'", lvl.String())

	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
}
